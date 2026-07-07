# Spike: rquickjs QuickJS guest on `wasm32-wasip1`

A de-risking spike (not production) to decide whether the QuickJS guest could move
from hand-written C to Rust (`rquickjs`) — see `../../DESIGN.md` and the guest review.
It answers the three unknowns that gated that decision.

## Result: all three unknowns retired ✓

| Unknown | Finding |
|---|---|
| **Does `rquickjs` build for `wasm32-wasip1`?** | **Yes.** `rquickjs 0.9.0` (vendored QuickJS) compiled cleanly; ~4.5 min first build. |
| **Does it load + run under wazero (wasip1 core module)?** | **Yes.** wazero instantiated it and `eval_answer()` returned `42` (QuickJS actually evaluating `6 * 7`). It needs **no `_initialize`** — instantiate with no start function. |
| **Binary size?** | **638,973 bytes (~625 KB)** with `opt-level="z"` + LTO + `strip` + `panic="abort"` — **smaller** than the current C guest (999,410 B / ~976 KB). |
| **Hermetic/offline build?** | Needed only crates.io (deps). **No WASI-SDK download** happened — the Rust toolchain's own wasi sysroot compiled the QuickJS C. For fully air-gapped, vendor the crates (`cargo vendor`); no external SDK fetch to worry about. |

Toolchain used: `rustc 1.93`, target `wasm32-wasip1` (pre-installed), wazero v1.9.0.

## Reproduce

```bash
cargo build --release --target wasm32-wasip1
ls -l target/wasm32-wasip1/release/rquickjs_guest.wasm   # ~625 KB, exports eval_answer + memory
```

Then load it in Go with the same wazero setup as `agent/engine.go`
(`wasi_snapshot_preview1.MustInstantiate`, instantiate with **no** start function),
call `eval_answer` → `42`.

## What this does NOT cover (next steps if we adopt Rust)

- The real generic ABI (`host_call` / `eval_code` / `resolve_handle` /
  `get_pending_*` / `run_microtasks`) — this spike only exports one `eval_answer`.
- The `host_call` → JS `Promise` → resolve-later loop via
  `Runtime::execute_pending_job()` + a persistent `Runtime` / fresh `Context` per run.
- The `Vec`-backed pending list + JS-level determinism prelude (all natural in Rust;
  see `DESIGN.md`).

## Bottom line

Feasible and attractive on the merits (smaller binary, `Vec` + RAII kills the C
guest's manual free/leak surface). But per the benchmarks (`agent/bench_test.go`), a
guest round is ~1 ms vs an LLM call of seconds, so there's **no urgent reason** to
rewrite: do it only if we adopt instance pooling or otherwise invest in the guest as
a lasting component.
