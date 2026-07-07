# CLAUDE.md — quickjs-worker-go

A **durable CodeAct AI agent**: an LLM writes small JavaScript programs each turn;
the programs run in an embedded QuickJS interpreter and call developer-registered
Go "tools"; every model call and tool call is a durable, journaled step in
[Restate](https://restate.dev), so the agent survives crashes and replays
deterministically. Built on **Go + wazero (WASM) + QuickJS + restatedev/sdk-go +
OpenAI**.

Module: `quickjsworker` (Go 1.25). This directory is the project root (`go.mod` is here).

---

Self-contained: all dependencies are published modules (see bottom) — no local
paths, no `replace`. `go build ./...` works from anywhere.

---

## Layout & entry point

```
quickjs-worker-go/          project root — run all `go` commands here
├── cmd/agent/              package main — THE entry point (USER code)
│   ├── main.go             setup() wires client+tools+loop; main() binds services & serves
│   └── tools.go            the developer's durable tools (compute / http_get / wait / delayed_fetch)
├── agent/                  package agent — reusable durable-CodeAct engine (INFRA)
│   ├── engine.go           wazero driver + instance pool + the re-execution loop (NO host import); determinism pin
│   ├── guest.go            cached guest exports (guest_alloc/dealloc/execute) + linear-memory helpers
│   ├── wasm.go             //go:embed quickjs_guest.wasm
│   ├── sandbox.go          assembles each round's script (determinism + journal + tool bridge + program) from JS raw-string constants; Invoker
│   ├── loop.go             RunAgent (the Go loop) + Model/Decision/Conversation/Turn + BuildSystemPrompt
│   ├── tool.go             Tool, NewTool (leaf→Future) / NewSeqTool (sequence); Future[R] + Run/Call/CallObject/Timer/Awakeable helpers; reflected arg+result schemas
│   ├── service.go          Config, Service, NewService, Definitions; Ask/History/Reset; execTool (AgentTools/Exec); restateInvoker (unified Wait driver); openAIModel
│   ├── quickjs_guest.wasm  the embedded guest (~656 KB, built from guest-rs/)
│   ├── agent_test.go       in-package tests + test doubles (~20 tests)
│   └── bench_test.go       instantiate/round/parallel benchmarks (the pool-decision evidence)
└── guest-rs/               the QuickJS guest: Rust/rquickjs → wasm32-wasip1 (`make guest-rs`)
```

- **Entry command:** `go run ./cmd/agent` (the module root is NOT runnable — no `main` there).
- **Public API** that `cmd/agent` uses from the `agent` package: `Config`,
  `NewService`, `Service.Definitions()`, `Service.Close()`, `Tool`, `NewTool`,
  `NewSeqTool`, `Future[R]`, and the future helpers `Run`/`Call`/`CallObject`/
  `Timer`/`Awakeable`. Everything else in `agent` is internal machinery.
- **To add a capability:** write a tool in `cmd/agent/tools.go` and register it in
  `setup()` in `cmd/agent/main.go`. Nothing in `agent/` needs to change.
  - A **leaf tool** (`NewTool`) is one durable op: its body returns a `Future[R]`
    from exactly one non-blocking `agent.Run`/`Call`/`Timer`/`Awakeable`.
  - A **seq tool** (`NewSeqTool`) is a blocking multi-step handler (full
    `restate.Context`); it runs in its own `AgentTools/Exec` sub-invocation.

---

## Build / test / run

```bash
go build ./...                              # builds agent + cmd/agent
go test ./...                               # agent package tests (engine/sandbox/loop/determinism/parallel/sessions)
OPENAI_API_KEY=sk-...  go run ./cmd/agent   # serves the "Agent" Virtual Object on :9080
```
There's also a `Makefile` (`make help` lists targets: build/test/vet/fmt/tidy/run/guest-rs).
`agent/quickjs_guest.wasm` is a COMMITTED prebuilt artifact (so `go build` needs
only Go); rebuild it with `make guest-rs` after editing `guest-rs/`.

Env vars: `AGENT_ADDR` (default `:9080`), `AGENT_MODEL` (default `gpt-5`; the
project's key also has gpt-5-mini/nano/gpt-5.1/gpt-5.2 — gpt-5 works via plain
Chat Completions, no special params), `OPENAI_API_KEY`
(REQUIRED — `setup()` exits at boot if unset; use `dummy` for a keyless local
endpoint), `OPENAI_BASE_URL` (optional; point at any OpenAI-compatible endpoint).

### Run end-to-end against a real Restate runtime (Docker)
```bash
OPENAI_API_KEY=sk-... go run ./cmd/agent &                   # agent on :9080
docker run -d --name restate -p 8080:8080 -p 9070:9070 \
  --add-host=host.docker.internal:host-gateway restatedev/restate:latest
curl -X POST http://localhost:9070/deployments \
  -H content-type:application/json -d '{"uri":"http://host.docker.internal:9080"}'
# talk to a session (object key = session id):
curl http://localhost:8080/Agent/s1/Ask -H content-type:application/json -d '{"message":"what is 6 times 7?"}'
curl -X POST http://localhost:8080/Agent/s1/History   # transcript (empty body — Void input)
curl -X POST http://localhost:8080/Agent/s1/Reset     # clear the session
```

### Rebuild the QuickJS guest (only if you edit guest-rs/)
```bash
make guest-rs   # cargo build (wasm32-wasip1) + copy to agent/quickjs_guest.wasm
```

### Sandbox gotchas (Claude Code environment)
If the shell runs sandboxed:
- **Go build cache** default (`~/Library/Caches/go-build`) may be unwritable →
  set `GOCACHE` to a writable dir, and use `GOSUMDB=off GOPROXY=off GOFLAGS=-mod=mod`
  to build offline from the module cache.
- **Docker / binding ports / real network (OpenAI, api.github.com)** need the
  Bash tool's `dangerouslyDisableSandbox: true` (Docker socket + egress are
  blocked in-sandbox; the earlier "permission denied" on the Docker socket was
  the sandbox, not a real perm issue).

---

## How it works (architecture)

```
Agent/<session>/Ask handler  →  RunAgent loop   (plain Go loop, NOT a restate.Run)
  each round:
    ├─ model.Decide ── restate.Run ─▶ OpenAI ─▶ Decision{ code }        (durable step)
    └─ Sandbox.RunProgram(code) ─▶ engine RE-EXECUTES the program each sub-round:
            │  execute(assemble(journal)) ─▶ QuickJS runs the program from the top;
            │    journaled tool calls return their recorded result; the first NEW
            │    call(s) form the FRONTIER, then the program blocks and returns
            │  await Promise.all([toolA(x), toolB(y)])  ← plain async JS; one frontier
            └─ InvokeBatch(frontier): submit each as a durable Future
                 (leaf = in-process Run/Call/Timer │ seq = RequestFuture to
                 AgentTools/Exec) ─▶ one restate.Wait drives them in PARALLEL   (durable steps)
                 ─▶ append results to the journal, re-execute
       program returns {done:true, answer}  → loop ends
       program returns anything else        → observation → next round
```

- **The loop is Go** (`RunAgent` in loop.go), running directly inside the `Ask`
  handler — it is NOT wrapped in `restate.Run`.
- **The only durable steps** are (1) each model call (in `openAIModel.Decide`) and
  (2) each tool call. Running the JS program is pure recomputation — it is re-executed
  both across Restate replays AND, within a single turn, once per frontier round (the
  re-execution model, below).
- **The model always emits a JS program** as `{"thought","code"}`. The program
  (an async function body — write statements directly, do NOT wrap in a `function`)
  ends by returning `{done:true, answer}` to finish, or any other value which is
  fed back as an observation for the next round (self-correction).
- **Re-execution model (why the guest has no host import and no globals):** the guest
  is a STATELESS one-shot evaluator — `execute(script) -> outputBlob`, with NO wasm
  import. It never calls back into Go and keeps nothing between calls (a fresh QuickJS
  runtime per call, dropped at the end). Instead of a live, suspended program, the host
  RE-EXECUTES the program from the top each round (`Engine.Run`'s loop), injecting a
  `globalThis.__journal` of the tool results so far. The pure-JS `__hostCall` bridge
  (`bridgeJS` in sandbox.go) matches each call BY ORDER against the journal: a known
  call resolves with its recorded result, a NEW call is pushed to
  `globalThis.__frontier` and returns a never-resolving promise so the program blocks.
  The guest then returns `{s:0,answer}` (done), `{s:1,frontier}` (run these calls), or
  `{s:2,error}`. This mirrors Restate's own handler replay, one level down. Because the
  program is deterministic, its call sequence is identical every round, so
  journal-by-index matching is sound.
- **Generic tools (no hardcoded names):** the sandbox prelude defines each registered
  tool as a plain async JS function over `__hostCall`; the guest hardcodes no tool
  names. (Earlier versions hijacked a fixed `webSearch` import as a transport, and an
  intermediate live-guest design used an `env.host_call` import — both are gone.)
- **One tool type, minted two ways** (`agent.NewTool` / `agent.NewSeqTool`). The
  model can't tell them apart; the difference is only *how the future is produced*,
  and the batch driver treats every future identically. The design rationale (why
  "a tool is a single durable op", and why a sequence needs its own invocation) is
  in `DESIGN.md`.
  - `NewTool[A,R](name, desc, func(restate.Context, A) (agent.Future[R], error))` —
    a **leaf** tool: its body performs ONE non-blocking submission and returns the
    `Future[R]`, built ONLY via `agent.Run` (side effect → `RunAsync`), `agent.Call`
    /`agent.CallObject` (service call → `RequestFuture`), `agent.Timer` (durable
    timer → `After`), or `agent.Awakeable`. A `Promise.all` of leaf tools runs
    durably IN PARALLEL, in-process (submit-all → one `restate.Wait` → read by index).
  - `NewSeqTool[A,R](name, desc, func(restate.Context, A) (R, error))` — a
    **sequence** tool: an ordinary blocking, multi-step handler with the full
    `restate.Context` (data-dependent steps, nested runs, timers, service calls,
    awakeables). It runs in its own sub-invocation via the keyless `AgentTools/Exec`
    handler, so it may block/orchestrate freely AND still parallelize with sibling
    tools — at the cost of one invocation hop. Seq tools are **session-stateless**
    (pass state via args) and must **not** call back into their own `Agent/<key>`
    (the parent holds that lock while awaiting them → deadlock).
  Both auto-reflect the arg JSON Schema from `A` **and** the result schema from `R`
  (honoring `json:`/`jsonschema:` tags) and surface both to the model.
- **Sessions:** the service is a Restate **Virtual Object** keyed by session id.
  `Ask` loads the transcript from state (`restate.Get`), runs the loop continuing
  from prior context, and persists it (`restate.Set`) on success only.
  `History` (shared read handler) returns the transcript; `Reset` clears it.

---

## Invariants to preserve (don't regress these)

- **Determinism / replay:** a program is re-run verbatim — both on Restate replay AND
  on every frontier round of the re-execution loop — so its clock & randomness MUST be
  frozen (sandbox.go's `detPreludeJS` overrides `Math.random`, the `Date` constructor,
  `Date.now`, `crypto`, `performance.now`; engine.go pins the WASI clock/rand via wazero
  `WithWalltime`/`WithRandSource` as a backstop). ONE seed is minted per program and
  reused across all its re-executions, so its tool-call sequence is identical every
  round — which is what makes journal-by-index matching sound. In production the seed =
  `restate.Rand(ctx).Int64()` and the clock is captured once in a journaled
  `restate.Run`. `TestReplayDeterminism` / `TestDeterminism` guard this.
- **Parallel tools:** `restateInvoker.InvokeBatch` must SUBMIT every call first
  (each `submit` is non-blocking — a leaf `Run`/`Call`/`Timer` or a seq
  `RequestFuture` to `AgentTools/Exec`), THEN drive them all with one `restate.Wait`,
  THEN read results BY INDEX (not completion order). Blocking inside the submission
  loop — or reading in yield order — would serialize or misorder them. This
  submit-all → one-Wait → read-by-index shape is required, and it's why a leaf
  tool's body must NOT block before returning its `Future` (it must only submit).
- **Cancellation is fatal, not a tool failure:** `InvokeBatch` captures the
  `TerminalError` that `restate.Wait` yields on a Restate cancellation and
  `panic(fatalError{...})`; `Engine.Run`'s recover preserves it verbatim and
  `RunAgent` returns any *terminal* `RunProgram` error immediately (does NOT feed it
  back). Otherwise a cancellation would be demoted to per-tool rejected promises a
  defensive program could swallow, "succeeding" a cancelled turn and persisting
  state. Ordinary tool failures arrive as *non-terminal* program errors and are still
  fed back for self-correction. A leaf tool that returns a zero-value `Future{}` is
  rejected at `submit` with a terminal error (never a nil-panic-then-retry-forever).
- **Pooled instances, EXCLUSIVE checkout:** guest instances are reused across Runs
  from a pool (`Engine.free`), not instantiated per Run — this removed ~10 MB/Run of
  allocation churn (now ~82 KB/Run). The guest is single-threaded and NOT reentrant,
  so reuse is safe ONLY because each instance is checked out by exactly one Run at a
  time (`acquire`/`release`): never hand one instance to two goroutines
  (`TestConcurrentRunsAreIsolated`, run with `-race`, guards this). Reuse needs NO
  reset because the guest is STATELESS — `execute` creates and drops a fresh QuickJS
  runtime per call, so nothing leaks between programs OR between the re-execution
  rounds of one program (`TestPoolReuseIsolation`); the host `guest_dealloc`s its
  per-run buffers so they don't accumulate. An instance is RETIRED (not returned to the
  pool) if the Run trapped/timed out, served `maxRunsPerInstance`, or grew past
  `instanceMemHighWater`. New instances are instantiated with `context.Background()`
  so their lifetime isn't tied to a Run's (cancellable) ctx; only the guest CALLS
  carry the per-Run ctx (timeout + cancellation).
- **No infinite retries:** deterministic give-ups (`ErrMaxRounds`) are returned as
  *terminal* errors by `Ask` (`restate.ToTerminalError`). A non-terminal error
  would make Restate retry the same deterministic failure forever.
- **State only on success:** `Ask` does `restate.Set(history)` only after the loop
  succeeds, so a failed turn leaves the session unchanged.
- **Safety:** wazero runtime built with `WithCloseOnContextDone(true)` +
  `WithMemoryLimitPages` (256 MiB); per-program timeout via `Sandbox.SetProgramTimeout`
  (a runaway `while(true)` is interrupted → error fed back). The guest also sets
  `set_memory_limit` (256 MiB) / `set_max_stack_size` (2 MiB) on its runtime. The
  frontier is an ordinary JS array with no arbitrary cap on legitimate `Promise.all`
  fan-out (`TestLargeFrontier` runs a 5000-call frontier); the re-execution loop is
  bounded by `maxProgramRounds` (4096 sequential rounds) as a backstop. Guest traps are
  recovered into errors in `Engine.Run` (call1 panics → recover → error), never crashing
  the goroutine. QuickJS is compiled with `NDEBUG` (guest-rs/.cargo/config.toml) so its
  teardown sweep frees the arena instead of tripping a debug refcount assert on an
  unhandled throwing microtask (`TestThrowingMicrotaskContained`).
- **math/rand/v2:** `restate.Rand(ctx)` returns a `*math/rand/v2.Rand` → use
  `.Int64()`, NOT `.Int63()`.
- **Model context is bounded** (loop.go): each observation is clipped to
  `maxObservationChars` (8 KB) before it enters the transcript, and each model
  call only sends the most-recent `maxTranscriptChars` (120 KB) via `windowTurns`.
  The FULL transcript is still persisted as session state; only what's sent to the
  model is bounded, so a huge tool output or a long session can't exceed the
  model's context window. Raise these consts for a larger-context model. (Both are
  deterministic → replay-safe.)
- **Never return a `json.RawMessage` from a `restate.Run`/`RunAsync` unless the
  bytes are guaranteed valid JSON.** Restate journals the Run output via
  `json.Marshal`, and `json.RawMessage.MarshalJSON` *validates* — non-JSON bytes
  panic ("failed to marshal Run output"). The model reply is journaled as a
  `string` (in `openAIModel.Decide`) and parsed leniently afterward; tool outputs
  use `json.Marshal(out)`, which is always valid.

---

## Status & known limitations

- **Verified end-to-end live** (Docker + real OpenAI `gpt-5`): the model reasons,
  self-corrects on a bad program, calls durable tools (`compute` local, `http_get` real
  network, a `delayed_fetch` seq tool as its own sub-invocation), runs parallel tools,
  persists session history, and a repeated `Idempotency-Key` runs the model once.
  NOTE: that live run predated the re-execution rewrite (same external behavior, new
  internal model); the re-execution build passes the full offline suite (~29 tests,
  incl. `-race`), `go vet ./...`, and gofmt clean, but has not been re-verified live.
- **The whole design was adversarially audited** (a 4-dimension review + verify
  pass); all confirmed findings are fixed (infinite-retry, runaway hang, memory
  cap, self-correction, tool-JSON validation, pending overflow).
- **Guest is Rust (`guest-rs/`, rquickjs) and instances are pooled.** The guest is
  `guest-rs/` (`rquickjs` → `wasm32-wasip1`, ~656 KB); it replaced an earlier C guest.
  Its ABI is now a single stateless `execute(script) -> {s:0 answer | s:1 frontier |
  s:2 error}` export (plus `guest_alloc`/`guest_dealloc`), with NO host import — the
  re-execution model (see "How it works"). Pooling (reuse with exclusive checkout) cut
  per-round allocation **~10 MB → ~82 KB (≈120×)** — measured in `bench_test.go`;
  latency is unchanged in practice (~1 ms, ≪ the LLM call). Pooling was done to kill GC
  churn under concurrent sessions, which the benchmark had flagged as the one real driver.
- **The Rust guest was adversarially reviewed** (multiple dimensions + verify). Fixed:
  (1) the `Date` freeze is no longer escapable via `.constructor`
  (`TestDateFreezeNotEscapableViaConstructor`); (2) `lcgReader` is non-mutating (WASI
  rand stays reuse-stable); (3) `guest_alloc(0)` no longer leaks; (4) the microtask
  drain continues past a throwing job so the queue fully drains before the runtime is
  dropped (`TestThrowingMicrotaskContained`). The intermediate live-guest design's
  cross-run use-after-free (a job queue on a persistent Runtime) is gone by
  construction — the re-execution guest drops its runtime after every call.
- **Graceful shutdown:** `Engine.Close` stops accepting new Runs and waits (bounded by
  ctx) for in-flight Runs before closing the runtime, so it can't close an instance
  under an active guest call (`TestGracefulShutdownWaitsForInflight`).
- **`compute` tool** uses a toy `evalSimple` (single `a*b`/`a+b` only) — it's a
  demo tool; replace with real capabilities.
- **Committed** on the `rust-guest-and-pooling` feature branch (the re-execution
  rewrite is `aa28bc4`); not pushed.

## Dependencies (all published; no replace)
- `github.com/restatedev/sdk-go v1.0.0` (Restate Go SDK)
- `github.com/tetratelabs/wazero v1.9.0` (pure-Go WASM runtime; NO cgo — chosen
  over wasmer-go which is cgo + unmaintained)
- `github.com/openai/openai-go/v3 v3.41.0` (official, GA)
- `github.com/invopop/jsonschema v0.14.0` (tool arg schemas; same as sdk-go)
