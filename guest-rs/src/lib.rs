//! QuickJS WASM guest (Rust / rquickjs) — a **live, host-driven JS coroutine**.
//!
//! The host runs the model's program ONCE and drives it to completion by settling
//! its promises as durable operations finish. There is **no wasm import** (the guest
//! never calls the host); instead the host calls these exports and reads a step blob
//! from each:
//!
//!   1. `start(script)` — the host assembles `script` = determinism prelude + bridge
//!      + program and evaluates it. Each tool call the program makes gets a deterministic
//!      integer HANDLE, records `{handle, name, arg}` in `globalThis.__outbox`, and
//!      returns a real Promise held in a `handle → {resolve, reject}` map. The program
//!      runs to synchronous quiescence and `start` returns the outbox as the first step.
//!   2. The host starts each op as a durable Restate future (handle → future), then
//!      races them with `WaitFirst`. When the FIRST completes it calls back in:
//!      `resolve(handle, jsonValue)` or `reject(handle, message)`, which settles that
//!      one promise (via the bridge's `__resolveJSON` / `__reject`), drives to
//!      quiescence again, and returns the next step (possibly with new ops).
//!   3. Repeat until a step reports the program settled: `{s:0, r}` (answer) or
//!      `{s:2, error}`. A running step is `{s:1, ops:[…]}`.
//!
//! # Why a live coroutine (vs. re-execution)
//!
//! Settling promises one-at-a-time, in completion order, is exactly how first-completion
//! (`Promise.race`/`any`/timeouts) works — the losers stay pending. It also mirrors
//! Restate's own `Select`: on replay the host re-runs `start` and feeds the journaled
//! completions back in the same order, so the program re-derives identically. The state
//! lives in the guest for the duration of one program; the host owns durability.
//!
//! Determinism: the host freezes clock/rand in the prelude and the handle counter is
//! deterministic, so the program's operation sequence is identical across replays.
//!
//! Modules: [`abi`] = the wire (exports + raw memory); [`engine`] = the coroutine.

mod abi;
mod engine;
