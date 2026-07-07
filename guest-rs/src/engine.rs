//! The evaluator: run one assembled script to synchronous quiescence and return the
//! output the JS produced. STATELESS — a fresh QuickJS runtime is created and
//! dropped within each call; nothing persists between calls (the host owns the
//! journal and re-invokes with it). See the crate docs for the replay model.

use rquickjs::{Context, Runtime};

// The host reads this JSON blob. The JS prelude sets `globalThis.__output` when the
// program settles ({s:0,answer} or {s:2,error}); otherwise the program blocked on
// new tool calls and we report the collected frontier ({s:1,frontier}); if it
// neither settled nor made a new call, it deadlocked.
const OUTPUT_EXPR: &str = "globalThis.__output || ((globalThis.__frontier && globalThis.__frontier.length) \
    ? JSON.stringify({s:1, frontier: globalThis.__frontier}) \
    : JSON.stringify({s:2, error: 'no pending ops and promise not settled (deadlock)'}))";

/// Evaluate `script` (determinism prelude + tool/journal prelude + the program,
/// assembled by the host) to synchronous quiescence, and return the output blob.
pub fn execute_script(script: &[u8]) -> Vec<u8> {
    let runtime = match Runtime::new() {
        Ok(r) => r,
        Err(_) => return err_blob("failed to create JS runtime"),
    };
    runtime.set_memory_limit(256 * 1024 * 1024); // 256 MiB
    runtime.set_max_stack_size(2 * 1024 * 1024); // 2 MiB
    let context = match Context::full(&runtime) {
        Ok(c) => c,
        Err(_) => return err_blob("failed to create JS context"),
    };

    // Evaluate the script. A syntax/eval error is a program-level failure.
    let compile_err: Option<String> = context.with(|ctx| match ctx.eval::<(), _>(script) {
        Ok(()) => None,
        Err(e) => Some(format!("{e}")),
    });
    if let Some(e) = compile_err {
        return err_blob(&e);
    }

    // Drive the microtask/job queue to quiescence: journaled tool calls resolve
    // immediately and the program advances; a new (unjournaled) call gets a promise
    // that never resolves this run, so the program blocks after making it. Continue
    // past a throwing job so the queue fully drains (the runtime is dropped after).
    loop {
        match runtime.execute_pending_job() {
            Ok(true) | Err(_) => continue,
            Ok(false) => break,
        }
    }

    // Read the output the JS assembled. (Determinism + isolation are inherent: the
    // runtime is brand new, so there is nothing to leak or reset.)
    let out: String = context.with(|ctx| {
        ctx.eval::<String, _>(OUTPUT_EXPR)
            .unwrap_or_else(|_| String::from("{\"s\":2,\"error\":\"failed to read guest output\"}"))
    });
    out.into_bytes()
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
