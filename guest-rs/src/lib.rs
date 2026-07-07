//! QuickJS WASM guest (Rust / rquickjs) — async JS executor with ONE generic
//! `host_call` import. Driven by the wazero host in `../agent`; the ABI and status
//! codes below are the contract that `../agent/engine.go` + `guest.go` depend on.
//!
//! Built for REUSE: a persistent `Runtime` with a FRESH `Context` per `eval_code`
//! (so a pooled instance can't leak globals between programs), a growable `Vec`
//! pending list (no fixed cap), and a full reset each run. `Vec` + RAII keep the
//! promise/handle bookkeeping leak-free.
//!
//! Determinism (clock/rand) is NOT here — the host injects it in the JS prelude, so
//! it survives instance reuse (see agent/sandbox.go determinismPrelude).

use std::cell::RefCell;

use rquickjs::{Context, Ctx, Function, Persistent, Promise, Runtime, Value};

// The single generic host import: register a named call, return a handle (or -1).
#[link(wasm_import_module = "env")]
extern "C" {
    fn host_call(name: *const u8, name_len: i32, arg: *const u8, arg_len: i32) -> i32;
}

// Status codes — must match the Go host (engine.go).
const STATUS_DONE: i32 = 0;
const STATUS_HAS_PENDING: i32 = 1;
const STATUS_ERROR: i32 = 2;

// High safety ceiling on concurrently in-flight calls: the Vec grows freely up to
// here (far above any legitimate Promise.all fan-out), then rejects cleanly.
const PENDING_MAX: usize = 1 << 20;

struct PendingOp {
    handle: i32,
    resolve: Persistent<Function<'static>>,
    reject: Persistent<Function<'static>>,
}

#[derive(Default)]
struct ResultBuf {
    settled: bool,
    is_error: bool,
    result: Vec<u8>,
    error: Vec<u8>,
}

thread_local! {
    static RT: RefCell<Option<Runtime>> = const { RefCell::new(None) };
    static CTX: RefCell<Option<Context>> = const { RefCell::new(None) };
    static PENDING: RefCell<Vec<PendingOp>> = const { RefCell::new(Vec::new()) };
    static RES: RefCell<ResultBuf> = RefCell::new(ResultBuf { settled: false, is_error: false, result: Vec::new(), error: Vec::new() });
}

// ── Memory exports ──────────────────────────────────────────────

#[no_mangle]
pub extern "C" fn guest_alloc(size: i32) -> *mut u8 {
    // A zero-length request gets a non-null dangling pointer (never read/written by
    // the host), so it allocates nothing and there's nothing to leak — symmetric
    // with guest_dealloc's size==0 no-op.
    if size <= 0 {
        return std::ptr::NonNull::<u8>::dangling().as_ptr();
    }
    let mut buf = Vec::<u8>::with_capacity(size as usize);
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr
}

#[no_mangle]
pub extern "C" fn guest_dealloc(ptr: *mut u8, size: i32) {
    if !ptr.is_null() && size > 0 {
        unsafe { drop(Vec::from_raw_parts(ptr, 0, size as usize)) };
    }
}

// ── Result / error exports (pointers stay valid until the next eval_code) ──

#[no_mangle]
pub extern "C" fn get_result_ptr() -> *const u8 {
    RES.with(|r| r.borrow().result.as_ptr())
}
#[no_mangle]
pub extern "C" fn get_result_len() -> i32 {
    RES.with(|r| r.borrow().result.len() as i32)
}
#[no_mangle]
pub extern "C" fn get_result_is_error() -> i32 {
    RES.with(|r| i32::from(r.borrow().is_error))
}
#[no_mangle]
pub extern "C" fn get_error_ptr() -> *const u8 {
    RES.with(|r| r.borrow().error.as_ptr())
}
#[no_mangle]
pub extern "C" fn get_error_len() -> i32 {
    RES.with(|r| r.borrow().error.len() as i32)
}

// ── Pending-handle exports ──────────────────────────────────────

#[no_mangle]
pub extern "C" fn get_pending_count() -> i32 {
    PENDING.with(|p| p.borrow().len() as i32)
}

#[no_mangle]
pub extern "C" fn get_pending_handles(out: *mut i32, capacity: i32) -> i32 {
    PENDING.with(|p| {
        let p = p.borrow();
        let n = p.len().min(capacity.max(0) as usize);
        let slice = unsafe { std::slice::from_raw_parts_mut(out, n) };
        for (i, op) in p.iter().take(n).enumerate() {
            slice[i] = op.handle;
        }
        n as i32
    })
}

// ── Result storage helpers ──────────────────────────────────────

fn store_resolved(s: &str) {
    RES.with(|r| {
        let mut r = r.borrow_mut();
        r.settled = true;
        r.is_error = false;
        r.result = s.as_bytes().to_vec();
    });
}
fn store_rejected(s: &str) {
    RES.with(|r| {
        let mut r = r.borrow_mut();
        r.settled = true;
        r.is_error = true;
        r.error = s.as_bytes().to_vec();
    });
}
fn store_fatal(s: &str) {
    // A guest-level failure (compile error / deadlock): STATUS_ERROR, error buffer.
    RES.with(|r| {
        let mut r = r.borrow_mut();
        r.is_error = true;
        r.error = s.as_bytes().to_vec();
    });
}

// ── The generic host bridge: __hostCall(name, arg) -> Promise ──

fn mk_error<'js>(ctx: &Ctx<'js>, msg: &str) -> rquickjs::Result<Value<'js>> {
    let mk: Function = ctx.globals().get("__mkErr")?;
    mk.call((msg.to_owned(),))
}

fn js_host_call<'js>(ctx: Ctx<'js>, name: String, arg: String) -> rquickjs::Result<Promise<'js>> {
    let handle = unsafe {
        host_call(
            name.as_ptr(),
            name.len() as i32,
            arg.as_ptr(),
            arg.len() as i32,
        )
    };
    let (promise, resolve, reject) = ctx.promise()?;
    if handle < 0 {
        reject.call::<_, ()>((mk_error(&ctx, "host refused the call")?,))?;
    } else if PENDING.with(|p| p.borrow().len()) >= PENDING_MAX {
        reject.call::<_, ()>((mk_error(&ctx, "too many concurrent host calls in flight")?,))?;
    } else {
        let r = Persistent::save(&ctx, resolve);
        let j = Persistent::save(&ctx, reject);
        PENDING.with(|p| p.borrow_mut().push(PendingOp { handle, resolve: r, reject: j }));
    }
    Ok(promise)
}

fn ensure_runtime() {
    RT.with(|rt| {
        if rt.borrow().is_none() {
            let r = Runtime::new().expect("JS runtime");
            r.set_memory_limit(256 * 1024 * 1024); // 256 MiB
            r.set_max_stack_size(2 * 1024 * 1024); // 2 MiB
            *rt.borrow_mut() = Some(r);
        }
    });
}

fn setup_globals(ctx: &Ctx) -> rquickjs::Result<()> {
    // __mkErr first — js_host_call/resolve_handle build Error objects with it.
    ctx.eval::<(), _>(r#"globalThis.__mkErr = function(m){ return new Error(String(m)); };"#)?;
    let g = ctx.globals();
    g.set("__hostCall", Function::new(ctx.clone(), js_host_call)?)?;
    g.set("__resolveMain", Function::new(ctx.clone(), |s: String| store_resolved(&s))?)?;
    g.set("__rejectMain", Function::new(ctx.clone(), |s: String| store_rejected(&s))?)?;
    Ok(())
}

// Drive the microtask/job queue to quiescence, then report status.
fn drain_and_status() -> i32 {
    RT.with(|rt| {
        if let Some(rt) = rt.borrow().as_ref() {
            // Drain to quiescence. Keep going PAST a job that throws (Err) — the job
            // queue lives on the persistent Runtime (shared across contexts), so a
            // job left queued here would later run against the NEXT run's freed
            // context (use-after-free) on a pooled instance. Each call dequeues one
            // job, so this terminates when the queue is empty (Ok(false)).
            loop {
                match rt.execute_pending_job() {
                    Ok(true) | Err(_) => continue,
                    Ok(false) => break,
                }
            }
        }
    });
    if RES.with(|r| r.borrow().settled) {
        STATUS_DONE
    } else if PENDING.with(|p| !p.borrow().is_empty()) {
        STATUS_HAS_PENDING
    } else {
        store_fatal("no pending ops and promise not settled (deadlock)");
        STATUS_ERROR
    }
}

// ── eval_code: reset, fresh context, evaluate the wrapped program ──

#[no_mangle]
pub extern "C" fn eval_code(code: *const u8, code_len: i32) -> i32 {
    // Full reset (free anything from a prior program on this instance).
    PENDING.with(|p| p.borrow_mut().clear());
    RES.with(|r| *r.borrow_mut() = ResultBuf::default());
    ensure_runtime();

    // Fresh Context per run — isolation between programs on a reused instance.
    RT.with(|rt| {
        let rt = rt.borrow();
        let ctx = Context::full(rt.as_ref().unwrap()).expect("JS context");
        CTX.with(|c| *c.borrow_mut() = Some(ctx));
    });

    let src = match std::str::from_utf8(unsafe {
        std::slice::from_raw_parts(code, code_len.max(0) as usize)
    }) {
        Ok(s) => s.to_owned(),
        Err(_) => {
            store_fatal("code is not valid UTF-8");
            return STATUS_ERROR;
        }
    };

    // Wrap in an async IIFE and pipe its settlement to the native callbacks. The
    // reject side extracts .message so a thrown Error reads cleanly on the host.
    let wrapped = format!(
        "(async function(){{\n{src}\n}})().then(__resolveMain, function(e){{ __rejectMain((e && e.message !== undefined) ? String(e.message) : String(e)); }});"
    );

    let compile_err: Option<String> = CTX.with(|c| {
        let c = c.borrow();
        let ctx = c.as_ref().unwrap();
        ctx.with(|ctx| {
            if let Err(e) = setup_globals(&ctx) {
                return Some(format!("setup: {e}"));
            }
            match ctx.eval::<Value, _>(wrapped.as_bytes()) {
                Ok(_) => None,
                Err(e) => Some(format!("{e}")),
            }
        })
    });
    if let Some(msg) = compile_err {
        store_fatal(&msg);
        return STATUS_ERROR;
    }
    drain_and_status()
}

// ── resolve_handle: host pushes a completed value back into the JS promise ──

#[no_mangle]
pub extern "C" fn resolve_handle(handle: i32, value: *const u8, value_len: i32, is_error: i32) {
    let val = String::from_utf8_lossy(unsafe {
        std::slice::from_raw_parts(value, value_len.max(0) as usize)
    })
    .into_owned();

    let op = PENDING.with(|p| {
        let mut p = p.borrow_mut();
        p.iter()
            .position(|o| o.handle == handle)
            .map(|idx| p.swap_remove(idx))
    });
    let op = match op {
        Some(o) => o,
        None => return,
    };

    CTX.with(|c| {
        let c = c.borrow();
        let ctx = match c.as_ref() {
            Some(x) => x,
            None => return,
        };
        ctx.with(|ctx| {
            let (Ok(resolve), Ok(reject)) = (op.resolve.restore(&ctx), op.reject.restore(&ctx))
            else {
                return;
            };
            if is_error != 0 {
                if let Ok(err) = mk_error(&ctx, &val) {
                    let _ = reject.call::<_, ()>((err,));
                }
            } else {
                let _ = resolve.call::<_, ()>((val,));
            }
        });
    });
}

// ── run_microtasks: drive the job queue after resolutions ──

#[no_mangle]
pub extern "C" fn run_microtasks() -> i32 {
    drain_and_status()
}

// ── cleanup: drop the context and runtime (e.g. when evicting a pooled instance) ──

#[no_mangle]
pub extern "C" fn cleanup() {
    PENDING.with(|p| p.borrow_mut().clear());
    RES.with(|r| *r.borrow_mut() = ResultBuf::default());
    CTX.with(|c| *c.borrow_mut() = None);
    RT.with(|rt| *rt.borrow_mut() = None);
}
