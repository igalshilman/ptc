//! The evaluator: a LIVE, host-driven JS coroutine. Unlike the previous stateless
//! one-shot model, the QuickJS runtime + context PERSIST across `start` / `resolve` /
//! `reject` for one program, so promises created in `start` can be settled later by
//! the host as durable operations complete. See the crate docs for the protocol.

use std::cell::RefCell;

use rquickjs::{Context, Function, Runtime};

/// Live per-program state. Held in a thread-local so it survives across the guest's
/// exported calls (start → resolve/reject → …) on one wasm instance. Re-created on
/// the next `start`; the host checks instances out exclusively, so only one program
/// is ever live per instance.
struct Guest {
    rt: Runtime,
    ctx: Context,
}

thread_local! {
    static GUEST: RefCell<Option<Guest>> = const { RefCell::new(None) };
}

// Read each step: the settled output the JS wrapper stored ({s:0,r} / {s:2,error}),
// or the drained outbox of new operations ({s:1,ops:[{handle,name,arg},…]}). The
// outbox is cleared as it is read, so each step returns only the ops emitted since
// the previous one.
const STEP_EXPR: &str = "globalThis.__output || (function () { var o = globalThis.__outbox || []; globalThis.__outbox = []; return JSON.stringify({ s: 1, ops: o }); })()";

/// start (re)initializes the live context, evaluates the assembled script
/// (determinism prelude + bridge + program) to synchronous quiescence, and returns
/// the first step blob. Any previous program's state is dropped.
pub fn start(script: &[u8]) -> Vec<u8> {
    let rt = match Runtime::new() {
        Ok(r) => r,
        Err(_) => return err_blob("failed to create JS runtime"),
    };
    rt.set_memory_limit(256 * 1024 * 1024); // 256 MiB
    rt.set_max_stack_size(2 * 1024 * 1024); // 2 MiB
    let ctx = match Context::full(&rt) {
        Ok(c) => c,
        Err(_) => return err_blob("failed to create JS context"),
    };

    // A syntax/eval error is a program-level failure.
    let compile_err: Option<String> = ctx.with(|ctx| match ctx.eval::<(), _>(script) {
        Ok(()) => None,
        Err(e) => Some(format!("{e}")),
    });

    // Install as the live guest (dropping any previous program's runtime/context).
    GUEST.with(|g| *g.borrow_mut() = Some(Guest { rt, ctx }));
    if let Some(e) = compile_err {
        GUEST.with(|g| *g.borrow_mut() = None);
        return err_blob(&e);
    }
    drive_and_read()
}

/// resolve settles the pending promise `handle` with the JSON value in `payload`
/// (parsed by the JS bridge), then drives to quiescence and returns the next step.
pub fn resolve(handle: i32, payload: &[u8]) -> Vec<u8> {
    settle("__resolveJSON", handle, payload)
}

/// reject settles the pending promise `handle` with an Error(payload-as-message).
pub fn reject(handle: i32, payload: &[u8]) -> Vec<u8> {
    settle("__reject", handle, payload)
}

fn settle(fname: &str, handle: i32, payload: &[u8]) -> Vec<u8> {
    let arg = String::from_utf8_lossy(payload).into_owned();
    let uninit = GUEST.with(|g| {
        let gb = g.borrow();
        let guest = match gb.as_ref() {
            Some(x) => x,
            None => return true,
        };
        guest.ctx.with(|ctx| {
            if let Ok(f) = ctx.globals().get::<_, Function>(fname) {
                let _ = f.call::<_, ()>((handle, arg.clone()));
            }
        });
        false
    });
    if uninit {
        return err_blob("settle called before start");
    }
    drive_and_read()
}

/// Drive the microtask/job queue to quiescence, then read the step blob. Continue
/// past a throwing job so the queue fully drains (the context is built with NDEBUG,
/// so teardown of any leftover pending promises is safe).
fn drive_and_read() -> Vec<u8> {
    GUEST.with(|g| {
        let gb = g.borrow();
        let guest = match gb.as_ref() {
            Some(x) => x,
            None => return err_blob("no live guest"),
        };
        loop {
            match guest.rt.execute_pending_job() {
                Ok(true) => continue,
                Ok(false) => break,
                Err(e) => {
                    // rquickjs 0.9 returns a JobException that OWNS the live context but
                    // was built WITHOUT a balancing JS_DupContext, so dropping it does an
                    // unbalanced JS_FreeContext (a refcount decrement). Enough throwing
                    // jobs (e.g. queueMicrotask(() => { throw }) in a loop) would drive
                    // the LIVE context's refcount to zero and free it mid-drain — a
                    // use-after-free that NDEBUG does NOT prevent (it only silences the
                    // teardown assert). Forget it to leak the phantom ref instead of
                    // decrementing, and keep draining so an unhandled throwing microtask
                    // stays contained (it is not fatal to the program).
                    std::mem::forget(e);
                    continue;
                }
            }
        }
        let out: String = guest.ctx.with(|ctx| {
            ctx.eval::<String, _>(STEP_EXPR).unwrap_or_else(|_| {
                String::from("{\"s\":2,\"error\":\"failed to read guest output\"}")
            })
        });
        out.into_bytes()
    })
}

/// Build a `{"s":2,"error":"..."}` blob with the message JSON-escaped (used for the
/// paths where JS could not produce the output itself).
fn err_blob(msg: &str) -> Vec<u8> {
    let mut s = String::from("{\"s\":2,\"error\":\"");
    for c in msg.chars() {
        match c {
            '"' => s.push_str("\\\""),
            '\\' => s.push_str("\\\\"),
            '\n' => s.push_str("\\n"),
            '\r' => s.push_str("\\r"),
            '\t' => s.push_str("\\t"),
            c if (c as u32) < 0x20 => s.push_str(&format!("\\u{:04x}", c as u32)),
            c => s.push(c),
        }
    }
    s.push_str("\"}");
    s.into_bytes()
}
