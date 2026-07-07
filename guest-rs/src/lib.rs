//! QuickJS WASM guest (Rust / rquickjs) — a **stateless, one-shot JS evaluator**.
//!
//! It exposes ONE operation: `execute(script)` runs an assembled script (built by
//! the host in `../agent`) to synchronous quiescence and returns an output blob.
//! There is **no host import** and **no persistent state** — the guest never calls
//! back into the host and holds nothing between calls.
//!
//! # The replay model (why there are no globals)
//!
//! This mirrors Restate's own durable execution, one level down. The host does NOT
//! keep a live, suspended program; instead it **re-executes the program from the
//! start each round**, feeding in a *journal* of the tool results gathered so far:
//!
//!   1. Host assembles `script` = determinism prelude + tool/journal prelude + the
//!      program, with the journal injected as `globalThis.__journal`.
//!   2. `execute(script)`: fresh QuickJS runtime; run the program + drain microtasks.
//!      Each tool call the program makes is matched by ORDER to `__journal`: an
//!      already-known call resolves immediately with its recorded result; a NEW call
//!      is pushed to `globalThis.__frontier` and returns a promise that never
//!      resolves this run. So the program advances to the first new call, then blocks.
//!   3. The guest reads the JS-produced output and returns it, then drops everything.
//!      Output is one of: `{s:0,answer}` (done), `{s:1,frontier:[{name,arg},…]}`
//!      (needs these calls run), `{s:2,error}`.
//!   4. Host runs the frontier durably (in parallel — the frontier IS the batch),
//!      appends the results to the journal, and calls `execute` again.
//!
//! Because the program is deterministic (the host freezes clock/rand in the
//! prelude), its tool-call sequence is identical across re-executions, so matching
//! the journal by position is sound — exactly like Restate replaying a handler.
//!
//! Modules: [`abi`] = the wire (exports + raw memory); [`engine`] = the evaluator.

mod abi;
mod engine;
