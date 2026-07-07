//! QuickJS-driving business logic: guest state, program evaluation, the
//! `host_call → JS Promise → resolve-later` loop, and settlement. Pure Rust — the C
//! ABI lives in [`crate::abi`]. See the crate docs for the (single-threaded)
//! concurrency model.

use std::cell::RefCell;

use rquickjs::{Context, Ctx, Function, Persistent, Promise, Runtime, Value};

use crate::abi;

// Status codes returned to the host — must match agent/engine.go.
const STATUS_DONE: i32 = 0;
const STATUS_HAS_PENDING: i32 = 1;
const STATUS_ERROR: i32 = 2;

/// One in-flight host call: the host's handle plus the JS promise's resolve/reject.
struct PendingOp {
    handle: i32,
    resolve: Persistent<Function<'static>>,
    reject: Persistent<Function<'static>>,
}

/// The settled top-level value (or a guest-level error), read back by the host.
#[derive(Default)]
struct ResultBuf {
    settled: bool,
    is_error: bool,
    result: Vec<u8>,
    error: Vec<u8>,
}

/// All guest state, held in ONE `thread_local` (see crate docs: single-threaded
/// wasm, host-serialized). The `Runtime` persists across runs (pooled reuse); a
/// fresh `Context` is installed per `eval` for isolation. `pending`/`result` are
/// mutated by native callbacks WHILE JS runs — safe because we never hold a borrow
/// of `GUEST` across JS execution: `context()`/`runtime()` clone the (refcounted)
/// handles out first, then we drive JS, so the callbacks can `borrow_mut` freely.
struct Guest {
    runtime: Runtime,
    context: Context,
    pending: Vec<PendingOp>,
    result: ResultBuf,
}

thread_local! {
    static GUEST: RefCell<Option<Guest>> = const { RefCell::new(None) };
}

fn new_guest() -> Guest {
    let runtime = Runtime::new().expect("JS runtime");
    runtime.set_memory_limit(256 * 1024 * 1024); // 256 MiB
    runtime.set_max_stack_size(2 * 1024 * 1024); // 2 MiB
    let context = Context::full(&runtime).expect("JS context");
    Guest { runtime, context, pending: Vec::new(), result: ResultBuf::default() }
}

/// Clone the current `Context`/`Runtime` handle out — releasing the `GUEST` borrow
/// so JS can run without one held (the reentrant host_call/settlement callbacks
/// need to `borrow_mut` `GUEST`). Cheap: both are refcounted handles.
fn context() -> Context {
    GUEST.with(|g| g.borrow().as_ref().expect("eval not started").context.clone())
}
fn runtime() -> Runtime {
    GUEST.with(|g| g.borrow().as_ref().expect("eval not started").runtime.clone())
}

// ── result storage (short borrows; called from native callbacks while JS runs) ──

fn store_resolved(s: &str) {
    GUEST.with(|g| {
        if let Some(x) = g.borrow_mut().as_mut() {
            x.result = ResultBuf { settled: true, is_error: false, result: s.as_bytes().to_vec(), error: Vec::new() };
        }
    });
}
fn store_rejected(s: &str) {
    GUEST.with(|g| {
        if let Some(x) = g.borrow_mut().as_mut() {
            x.result = ResultBuf { settled: true, is_error: true, result: Vec::new(), error: s.as_bytes().to_vec() };
        }
    });
}
/// A guest-level failure (compile error / deadlock): STATUS_ERROR + error buffer,
/// not "settled".
fn store_fatal(s: &str) {
    GUEST.with(|g| {
        if let Some(x) = g.borrow_mut().as_mut() {
            x.result.is_error = true;
            x.result.error = s.as_bytes().to_vec();
        }
    });
}

fn mk_error<'js>(ctx: &Ctx<'js>, msg: &str) -> rquickjs::Result<Value<'js>> {
    let mk: Function = ctx.globals().get("__mkErr")?;
    mk.call((msg.to_owned(),))
}

/// `__hostCall(name, arg)`: register a host call and hand JS a promise the host
/// later settles via [`resolve`]. The pending list grows freely — no fixed cap; the
/// runtime memory limit (and the host's wazero page cap) is the backstop.
fn js_host_call<'js>(ctx: Ctx<'js>, name: String, arg: String) -> rquickjs::Result<Promise<'js>> {
    let handle = abi::host_call(&name, &arg);
    let (promise, resolve, reject) = ctx.promise()?;
    if handle < 0 {
        reject.call::<_, ()>((mk_error(&ctx, "host refused the call")?,))?;
    } else {
        let op = PendingOp {
            handle,
            resolve: Persistent::save(&ctx, resolve),
            reject: Persistent::save(&ctx, reject),
        };
        GUEST.with(|g| {
            if let Some(x) = g.borrow_mut().as_mut() {
                x.pending.push(op);
            }
        });
    }
    Ok(promise)
}

fn setup_globals(ctx: &Ctx) -> rquickjs::Result<()> {
    // __mkErr first — host_call/resolve build Error objects with it.
    ctx.eval::<(), _>(r#"globalThis.__mkErr = function(m){ return new Error(String(m)); };"#)?;
    let g = ctx.globals();
    g.set("__hostCall", Function::new(ctx.clone(), js_host_call)?)?;
    g.set("__resolveMain", Function::new(ctx.clone(), |s: String| store_resolved(&s))?)?;
    g.set("__rejectMain", Function::new(ctx.clone(), |s: String| store_rejected(&s))?)?;
    Ok(())
}

/// Drive the job/microtask queue to quiescence, then report status. Continues PAST
/// a throwing job (which returns `Err`) so none are left in the shared-Runtime
/// queue to run against the next run's freed context on a pooled instance.
fn drain_and_status() -> i32 {
    let rt = runtime();
    loop {
        match rt.execute_pending_job() {
            Ok(true) | Err(_) => continue,
            Ok(false) => break,
        }
    }
    let (settled, has_pending) = GUEST.with(|g| {
        let g = g.borrow();
        let x = g.as_ref().unwrap();
        (x.result.settled, !x.pending.is_empty())
    });
    if settled {
        STATUS_DONE
    } else if has_pending {
        STATUS_HAS_PENDING
    } else {
        store_fatal("no pending ops and promise not settled (deadlock)");
        STATUS_ERROR
    }
}

// ── public API called by the ABI layer ─────────────────────────────────────────

/// Reset state, install a FRESH `Context` on the persistent `Runtime` (isolation
/// between programs on a reused instance), and evaluate the program — wrapped in an
/// async IIFE whose settlement pipes to the native `__resolveMain`/`__rejectMain`.
pub fn eval(code: &[u8]) -> i32 {
    let ctx = GUEST.with(|g| {
        let mut slot = g.borrow_mut();
        let guest = slot.get_or_insert_with(new_guest);
        guest.pending.clear();
        guest.result = ResultBuf::default();
        guest.context = Context::full(&guest.runtime).expect("JS context");
        guest.context.clone()
    });

    let src = match std::str::from_utf8(code) {
        Ok(s) => s,
        Err(_) => {
            store_fatal("code is not valid UTF-8");
            return STATUS_ERROR;
        }
    };
    let wrapped = format!(
        "(async function(){{\n{src}\n}})().then(__resolveMain, function(e){{ __rejectMain((e && e.message !== undefined) ? String(e.message) : String(e)); }});"
    );

    let compile_err = ctx.with(|ctx| {
        if let Err(e) = setup_globals(&ctx) {
            return Some(format!("setup: {e}"));
        }
        match ctx.eval::<Value, _>(wrapped.as_bytes()) {
            Ok(_) => None,
            Err(e) => Some(format!("{e}")),
        }
    });
    if let Some(msg) = compile_err {
        store_fatal(&msg);
        return STATUS_ERROR;
    }
    drain_and_status()
}

/// Drive the queue after the host has resolved some handles.
pub fn run_microtasks() -> i32 {
    drain_and_status()
}

/// Settle the JS promise for a host handle with `value` (as a fulfilled string, or
/// a rejected `Error` when `is_error`).
pub fn resolve(handle: i32, value: &str, is_error: bool) {
    let op = GUEST.with(|g| {
        let mut g = g.borrow_mut();
        let pending = &mut g.as_mut()?.pending;
        pending.iter().position(|o| o.handle == handle).map(|i| pending.swap_remove(i))
    });
    let Some(op) = op else { return };

    context().with(|ctx| {
        let (Ok(resolve), Ok(reject)) = (op.resolve.restore(&ctx), op.reject.restore(&ctx)) else {
            return;
        };
        if is_error {
            if let Ok(err) = mk_error(&ctx, value) {
                let _ = reject.call::<_, ()>((err,));
            }
        } else {
            let _ = resolve.call::<_, ()>((value.to_owned(),));
        }
    });
}

pub fn pending_count() -> i32 {
    GUEST.with(|g| g.borrow().as_ref().map_or(0, |x| x.pending.len() as i32))
}

/// Write pending handles into `out`, returning how many were written.
pub fn fill_pending_handles(out: &mut [i32]) -> usize {
    GUEST.with(|g| {
        let g = g.borrow();
        let Some(x) = g.as_ref() else { return 0 };
        let n = x.pending.len().min(out.len());
        for (slot, op) in out.iter_mut().zip(x.pending.iter()).take(n) {
            *slot = op.handle;
        }
        n
    })
}

pub fn result_ptr() -> *const u8 {
    GUEST.with(|g| g.borrow().as_ref().map_or(std::ptr::null(), |x| x.result.result.as_ptr()))
}
pub fn result_len() -> i32 {
    GUEST.with(|g| g.borrow().as_ref().map_or(0, |x| x.result.result.len() as i32))
}
pub fn result_is_error() -> i32 {
    GUEST.with(|g| g.borrow().as_ref().map_or(0, |x| i32::from(x.result.is_error)))
}
pub fn error_ptr() -> *const u8 {
    GUEST.with(|g| g.borrow().as_ref().map_or(std::ptr::null(), |x| x.result.error.as_ptr()))
}
pub fn error_len() -> i32 {
    GUEST.with(|g| g.borrow().as_ref().map_or(0, |x| x.result.error.len() as i32))
}

/// Drop the runtime + context (e.g. when the host evicts a pooled instance).
pub fn cleanup() {
    GUEST.with(|g| *g.borrow_mut() = None);
}
