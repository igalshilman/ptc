/**
 * QuickJS WASM Guest — async JS executor with ONE generic host_call import.
 *
 * The guest is a thin wrapper: QuickJS + promise plumbing. It knows nothing
 * about Restate or any specific tool. The host owns the event loop and decides
 * what each named call does.
 *
 * Guest exports: eval_code, run_microtasks, get_pending_count, get_pending_handles,
 *                resolve_handle, get_result_*, get_error_*, guest_alloc/dealloc, cleanup
 * Guest import:  env.host_call(name, name_len, arg, arg_len) -> handle (or -1)
 * JS global:     __hostCall(name, argString) -> Promise (resolves with a string)
 */

#include "quickjs.h"
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

/* ── The single generic host import: register a named call, return a handle ── */

__attribute__((import_module("env"), import_name("host_call")))
extern int32_t host_call(const char *name, int32_t name_len,
                         const char *arg, int32_t arg_len);

/* ── Pending promise tracking ──────────────────────────────────── */

/* pending grows on demand (no fixed cap for legitimate fan-out). PENDING_MAX is a
 * high safety ceiling — far above any real Promise.all — beyond which we reject
 * cleanly, so a pathological program fails with a clear error instead of trapping
 * on an allocation failure or exhausting the 256 MiB linear memory. */
#define PENDING_MAX (1 << 20) /* 1,048,576 concurrent in-flight host calls */

typedef struct {
    int32_t handle;
    JSValue resolve;
    JSValue reject;
} PendingOp;

static JSRuntime *rt;
static JSContext *ctx;
static JSValue main_promise = JS_UNINITIALIZED;
static int main_settled = 0;

static PendingOp *pending = NULL; /* growable; length pending_count, capacity pending_cap */
static int pending_count = 0;
static int pending_cap = 0;

static char *result_buf;
static int32_t result_len;
static int32_t result_is_error;
static char *error_buf;
static int32_t error_len;

/* ── Status codes ──────────────────────────────────────────────── */

#define STATUS_DONE        0
#define STATUS_HAS_PENDING 1
#define STATUS_ERROR       2

/* ── Memory exports ────────────────────────────────────────────── */

__attribute__((export_name("guest_alloc")))
void *guest_alloc(int32_t size) { return malloc(size); }

__attribute__((export_name("guest_dealloc")))
void guest_dealloc(void *ptr, int32_t size) { (void)size; free(ptr); }

/* ── Result exports ────────────────────────────────────────────── */

__attribute__((export_name("get_result_ptr")))
const char *get_result_ptr(void) { return result_buf; }

__attribute__((export_name("get_result_len")))
int32_t get_result_len(void) { return result_len; }

__attribute__((export_name("get_result_is_error")))
int32_t get_result_is_error(void) { return result_is_error; }

__attribute__((export_name("get_error_ptr")))
const char *get_error_ptr(void) { return error_buf; }

__attribute__((export_name("get_error_len")))
int32_t get_error_len(void) { return error_len; }

/* ── Pending handle exports ────────────────────────────────────── */

__attribute__((export_name("get_pending_count")))
int32_t get_pending_count(void) { return pending_count; }

/* Write pending handles to caller-provided buffer */
__attribute__((export_name("get_pending_handles")))
int32_t get_pending_handles(int32_t *out, int32_t capacity) {
    int32_t n = pending_count < capacity ? pending_count : capacity;
    for (int32_t i = 0; i < n; i++) out[i] = pending[i].handle;
    return n;
}

/* ── Internal helpers ──────────────────────────────────────────── */

/* Free every tracked promise capability and clear the list, keeping the backing
 * array for reuse (cleanup() frees the array itself). Safe when pending_count==0,
 * e.g. on a fresh instance before any context exists. */
static void reset_pending(void) {
    for (int i = 0; i < pending_count; i++) {
        JS_FreeValue(ctx, pending[i].resolve);
        JS_FreeValue(ctx, pending[i].reject);
    }
    pending_count = 0;
}

/* Returns 1 on success, 0 if the ceiling is hit or growth failed (the caller then
 * rejects the promise). Grows the array geometrically on demand. */
static int add_pending(int32_t handle, JSValue resolve, JSValue reject) {
    if (pending_count >= PENDING_MAX) return 0;
    if (pending_count == pending_cap) {
        int new_cap = pending_cap ? pending_cap * 2 : 64;
        if (new_cap > PENDING_MAX) new_cap = PENDING_MAX;
        PendingOp *grown = realloc(pending, (size_t)new_cap * sizeof(PendingOp));
        if (!grown) return 0;
        pending = grown;
        pending_cap = new_cap;
    }
    pending[pending_count].handle = handle;
    pending[pending_count].resolve = JS_DupValue(ctx, resolve);
    pending[pending_count].reject = JS_DupValue(ctx, reject);
    pending_count++;
    return 1;
}

static void store_result(const char *s, int32_t len) {
    free(result_buf);
    result_buf = malloc(len + 1);
    memcpy(result_buf, s, len);
    result_buf[len] = 0;
    result_len = len;
}

static void store_error(const char *s, int32_t len) {
    free(error_buf);
    error_buf = malloc(len + 1);
    memcpy(error_buf, s, len);
    error_buf[len] = 0;
    error_len = len;
}

/* Reject a freshly-created promise capability with an Error(message). */
static void reject_with(JSValue reject, const char *message) {
    JSValue err = JS_NewError(ctx);
    JS_SetPropertyStr(ctx, err, "message", JS_NewString(ctx, message));
    JS_Call(ctx, reject, JS_UNDEFINED, 1, &err);
    JS_FreeValue(ctx, err);
}

/* Create a Promise for a host handle. A negative handle means the host refused
 * the call; hitting the PENDING_MAX ceiling (or an allocation failure while
 * growing) means too many concurrent calls — both reject the promise immediately
 * (with a clear message) instead of stranding it. */
static JSValue make_promise_for_handle(int32_t handle) {
    JSValue funcs[2];
    JSValue promise = JS_NewPromiseCapability(ctx, funcs);
    if (JS_IsException(promise)) return JS_EXCEPTION;
    if (handle < 0) {
        reject_with(funcs[1], "host refused the call");
    } else if (!add_pending(handle, funcs[0], funcs[1])) {
        reject_with(funcs[1], "too many concurrent host calls in flight");
    }
    JS_FreeValue(ctx, funcs[0]);
    JS_FreeValue(ctx, funcs[1]);
    return promise;
}

/* ── The single JS global: __hostCall(name, argString) -> Promise ── */

static JSValue js_host_call(JSContext *c, JSValueConst this_val,
                            int argc, JSValueConst *argv) {
    (void)this_val;
    const char *name = JS_ToCString(c, argc > 0 ? argv[0] : JS_UNDEFINED);
    if (!name) return JS_EXCEPTION;
    const char *arg = JS_ToCString(c, argc > 1 ? argv[1] : JS_UNDEFINED);
    if (!arg) { JS_FreeCString(c, name); return JS_EXCEPTION; }
    int32_t h = host_call(name, (int32_t)strlen(name), arg, (int32_t)strlen(arg));
    JS_FreeCString(c, name);
    JS_FreeCString(c, arg);
    return make_promise_for_handle(h);
}

/* ── Promise settlement callbacks ──────────────────────────────── */

static JSValue on_main_resolved(JSContext *c, JSValueConst this_val,
                                int argc, JSValueConst *argv) {
    (void)this_val;
    main_settled = 1;
    result_is_error = 0;
    if (argc > 0 && !JS_IsUndefined(argv[0])) {
        const char *s = JS_ToCString(c, argv[0]);
        if (s) { store_result(s, strlen(s)); JS_FreeCString(c, s); }
        else { store_result("null", 4); }
    } else {
        store_result("null", 4);
    }
    return JS_UNDEFINED;
}

static JSValue on_main_rejected(JSContext *c, JSValueConst this_val,
                                int argc, JSValueConst *argv) {
    (void)this_val;
    main_settled = 1;
    result_is_error = 1;
    if (argc > 0) {
        JSValue msg = JS_GetPropertyStr(c, argv[0], "message");
        const char *s = JS_ToCString(c, JS_IsUndefined(msg) ? argv[0] : msg);
        if (s) { store_error(s, strlen(s)); JS_FreeCString(c, s); }
        else { store_error("unknown error", 13); }
        JS_FreeValue(c, msg);
    } else {
        store_error("rejected", 8);
    }
    return JS_UNDEFINED;
}

/* ── eval_code: initialize and evaluate user JS ────────────────── */

__attribute__((export_name("eval_code")))
int32_t eval_code(const char *code, int32_t code_len) {
    /* Reset state, freeing anything left from a prior program on this instance
     * (harmless on a fresh instance — the current one-instance-per-Run model — but
     * makes the guest reuse-safe if instances are ever pooled). */
    reset_pending();
    if (rt) { JS_FreeValue(ctx, main_promise); main_promise = JS_UNINITIALIZED; }
    main_settled = 0;
    result_is_error = 0;
    free(result_buf); result_buf = NULL; result_len = 0;
    free(error_buf); error_buf = NULL; error_len = 0;

    if (!rt) {
        rt = JS_NewRuntime();
        /* Bound the guest heap and C stack so a runaway program surfaces as a JS
         * error the host can report, not a host OOM / wasm trap. */
        JS_SetMemoryLimit(rt, 256 * 1024 * 1024); /* 256 MiB */
        JS_SetMaxStackSize(rt, 2 * 1024 * 1024);  /* 2 MiB (wasm stack is 4 MiB) */
        ctx = JS_NewContext(rt);
    }

    /* Register the single generic host bridge. Tool functions are defined in JS
     * by the host prelude on top of __hostCall — the guest hardcodes no tools. */
    JSValue global = JS_GetGlobalObject(ctx);
    JS_SetPropertyStr(ctx, global, "__hostCall",
        JS_NewCFunction(ctx, js_host_call, "__hostCall", 2));
    JS_FreeValue(ctx, global);

    /* Wrap code in async IIFE */
    int32_t wrap_len = code_len + 80;
    char *wrap = malloc(wrap_len);
    snprintf(wrap, wrap_len, "(async function(){\n%.*s\n})()", (int)code_len, code);

    JSValue val = JS_Eval(ctx, wrap, strlen(wrap), "<input>", JS_EVAL_TYPE_GLOBAL);
    free(wrap);

    if (JS_IsException(val)) {
        JSValue exc = JS_GetException(ctx);
        const char *msg = JS_ToCString(ctx, exc);
        store_error(msg ? msg : "eval error", msg ? strlen(msg) : 10);
        if (msg) JS_FreeCString(ctx, msg);
        JS_FreeValue(ctx, exc);
        JS_FreeValue(ctx, val);
        result_is_error = 1;
        return STATUS_ERROR;
    }

    /* Attach .then/.catch to detect settlement */
    main_promise = val;
    JSValue then_fn = JS_NewCFunction(ctx, on_main_resolved, "resolve", 1);
    JSValue catch_fn = JS_NewCFunction(ctx, on_main_rejected, "reject", 1);
    JSValue then_result = JS_Invoke(ctx, main_promise, JS_NewAtom(ctx, "then"), 1, &then_fn);
    JSValue catch_result = JS_Invoke(ctx, then_result, JS_NewAtom(ctx, "catch"), 1, &catch_fn);
    JS_FreeValue(ctx, then_fn);
    JS_FreeValue(ctx, catch_fn);
    JS_FreeValue(ctx, then_result);
    JS_FreeValue(ctx, catch_result);

    /* Run initial microtasks */
    JSContext *pctx;
    while (JS_ExecutePendingJob(rt, &pctx) > 0) {}

    if (main_settled) return STATUS_DONE;
    if (pending_count > 0) return STATUS_HAS_PENDING;
    return STATUS_ERROR; /* No pending ops and not settled — deadlock */
}

/* ── resolve_handle: host pushes a completed value back ────────── */

__attribute__((export_name("resolve_handle")))
void resolve_handle(int32_t handle, const char *value, int32_t value_len,
                    int32_t is_error) {
    for (int i = 0; i < pending_count; i++) {
        if (pending[i].handle != handle) continue;

        if (is_error) {
            JSValue err = JS_NewError(ctx);
            JS_SetPropertyStr(ctx, err, "message",
                JS_NewStringLen(ctx, value, value_len));
            JS_Call(ctx, pending[i].reject, JS_UNDEFINED, 1, &err);
            JS_FreeValue(ctx, err);
        } else {
            JSValue val = JS_NewStringLen(ctx, value, value_len);
            JS_Call(ctx, pending[i].resolve, JS_UNDEFINED, 1, &val);
            JS_FreeValue(ctx, val);
        }

        /* Remove from pending */
        JS_FreeValue(ctx, pending[i].resolve);
        JS_FreeValue(ctx, pending[i].reject);
        pending[i] = pending[pending_count - 1];
        pending_count--;
        return;
    }
}

/* ── run_microtasks: drive the QuickJS job queue ───────────────── */

__attribute__((export_name("run_microtasks")))
int32_t run_microtasks(void) {
    JSContext *pctx;
    while (JS_ExecutePendingJob(rt, &pctx) > 0) {}

    if (main_settled) return STATUS_DONE;
    if (pending_count > 0) return STATUS_HAS_PENDING;
    return STATUS_ERROR;
}

/* ── cleanup ───────────────────────────────────────────────────── */

__attribute__((export_name("cleanup")))
void cleanup(void) {
    reset_pending();
    free(pending); pending = NULL; pending_cap = 0;
    if (!JS_IsUndefined(main_promise)) JS_FreeValue(ctx, main_promise);
    main_promise = JS_UNINITIALIZED;
    if (ctx) { JS_FreeContext(ctx); ctx = NULL; }
    if (rt) { JS_FreeRuntime(rt); rt = NULL; }
    free(result_buf); result_buf = NULL;
    free(error_buf); error_buf = NULL;
}
