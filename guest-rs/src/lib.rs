//! QuickJS WASM guest (Rust / rquickjs) — an async-JS executor exposed to the Go
//! host in `../agent` over a small C ABI. Two modules, cleanly separated:
//!
//!   - [`abi`]    — the cross-language boundary ONLY: the one wasm import
//!                  (`env.host_call`) and every wasm export, plus raw linear-memory
//!                  marshaling. No business logic; each export is a thin wrapper.
//!   - [`engine`] — the QuickJS-driving logic: guest state, program evaluation, the
//!                  `host_call → JS Promise → resolve-later` loop, and settlement.
//!
//! # Concurrency — why `thread_local` here is not a thread-safety claim
//!
//! `wasm32-wasip1` has NO threads: this module runs single-threaded. The
//! `thread_local` in [`engine`] is therefore NOT guarding against concurrent
//! threads — it is simply the safe, `unsafe`-free way to hold the `!Send` rquickjs
//! handles as per-INSTANCE global state (each wazero instance has its own linear
//! memory, hence its own copy).
//!
//! Safety across concurrent host requests is the HOST's responsibility, not the
//! guest's: the Go pool checks out one wasm instance per goroutine at a time (see
//! `agent/engine.go` `acquire`/`release`), so this state is never touched by two
//! goroutines at once. Determinism (clock/rand) is injected by the host in JS (see
//! `agent/sandbox.go`), so it survives instance reuse.

mod abi;
mod engine;
