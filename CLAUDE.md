# CLAUDE.md — quickjs-worker-go

A **durable CodeAct AI agent**: an LLM writes small JavaScript programs each turn;
the programs run in an embedded QuickJS interpreter and call developer-registered
Go "tools"; every model call and tool call is a durable, journaled step in
[Restate](https://restate.dev), so the agent survives crashes and replays
deterministically. Built on **Go + wazero (WASM) + QuickJS + restatedev/sdk-go +
OpenAI**.

Module: `restatedev` (Go 1.25) — the core library imports as `restatedev/agent`. This
directory is the project root (`go.mod` is here).

---

Self-contained: all dependencies are published modules (see bottom) — no local
paths, no `replace`. `go build ./...` works from anywhere.

---

## Layout & entry points

```
quickjs-worker-go/          project root — run all `go` commands here
├── examples/               runnable demo binaries (USER code) — each is a package main,
│   │                        meant to run as SEPARATE Restate deployments
│   ├── orchestrator/        an order-fulfillment agent that discovers back-office handlers
│   │   ├── main.go           ClientFromEnv → NewService{Discover + static tools} → Deploy(Definitions())
│   │   └── tools.go          the `sleep` (Timer) and `signal` (named external signal) tools
│   └── backoffice/          standalone deployment of the handlers the agent discovers
│       ├── main.go           hands its service definitions to agent.Deploy (tunnel)
│       └── services.go       Inventory / RiskCheck / Payments — annotated for discovery
├── agent/                  package agent — reusable durable-CodeAct engine (INFRA)
│   ├── engine.go            wazero driver + instance pool + RunLive (the live-coroutine drive loop); WASI clock/rand pin; //go:embed of the guest; the host↔guest wire types (guestStep/ToolCall/StepResult)
│   ├── guest.go             cached guest exports (guest_alloc/dealloc/start/resolve/reject) + linear-memory helpers
│   ├── sandbox.go           assembles each program's script (determinism prelude + live tool bridge + program) from JS raw-string constants; ToolSpec + Invoker interface
│   ├── loop.go              RunAgent (the Go loop) + Model/Decision/Conversation/Turn + BuildSystemPrompt
│   ├── tool.go              Tool, NewTool (leaf→Future); Future[R] + Run/Call/CallObject/Timer/Awakeable/Signal helpers; reflected arg+result schemas
│   ├── discover.go          Admin-API handler discovery: DiscoverConfig, AgentToolAnnotation, DiscoverTools, toolFromDescriptor
│   ├── service.go           Config, Service, NewService, Definitions; Ask/History/Reset; AgentSignals resolve/reject; restateInvoker (Start/Next, WaitFirst driver); openAIModel
│   ├── serve.go             shared example conveniences: ClientFromEnv (env→client) + Deploy (bind defs → outbound x/tunnel to Restate Cloud)
│   ├── quickjs_guest.wasm   the embedded guest (~600 KB, built from guest-rs/)
│   ├── agent_test.go        in-package tests + test doubles (~30 tests)
│   └── bench_test.go        instantiate/round/parallel benchmarks (the pool-decision evidence)
└── guest-rs/               the QuickJS guest: Rust/rquickjs → wasm32-wasip1 (`make guest-rs`)
```

- **Entry commands:** `go run ./examples/orchestrator` (the agent) and, as a separate
  deployment, `go run ./examples/backoffice` (the handlers it discovers) — the module
  root and `agent/` are NOT runnable (no `main`). The orchestrator is a tiny `main()`
  that builds a tool set, `NewService`s it, and hands `svc.Definitions()` to `Deploy`
  (see serve.go); the back-office hands its own definitions to `Deploy` the same way.
- **Public API** the example uses from the `agent` package:
  - lifecycle: `Config`, `NewService`, `Service.Definitions()`, `Service.Close()`, and
    the example conveniences `ClientFromEnv` / `Deploy` (serve.go);
  - tools: `Tool`, `NewTool`, `Future[R]`, and the future helpers `Run` / `Call` /
    `CallObject` / `Timer` / `Awakeable` / `Signal`;
  - discovery: `DiscoverConfig`, `AgentToolAnnotation`, `DiscoverTools`.
  Everything else in `agent` is internal machinery.
- **To add a capability:**
  - A **leaf tool** (`NewTool`) is one durable op: its body performs exactly one
    non-blocking submission and returns the resulting `Future[R]`, built via
    `agent.Run` / `Call` / `CallObject` / `Timer` / `Awakeable` / `Signal`. Register it
    in the example's `Tools`.
  - A **multi-step, blocking operation is NOT a special tool kind** (the old
    `NewSeqTool` is gone). Model it as an ordinary Restate handler and either have a
    leaf tool `agent.Call` it, or annotate the handler with `AgentToolAnnotation` so
    discovery turns it into a tool automatically. The handler runs in its own
    invocation, where it may block/branch freely; to the batch driver the call is just
    another future.

---

## Build / test / run

```bash
go build ./...                                  # builds agent + both examples
go test ./...                                   # agent package tests (engine/sandbox/loop/determinism/parallel/race/sessions)
OPENAI_API_KEY=sk-...  go run ./examples/orchestrator  # the "Agent" Virtual Object on :9080
                       go run ./examples/backoffice    # the discovered handlers on :9081
```
There's also a `Makefile` (`make help` lists targets: build/test/vet/fmt/tidy/run/guest-rs).
`agent/quickjs_guest.wasm` is a COMMITTED prebuilt artifact (so `go build` needs
only Go); rebuild it with `make guest-rs` after editing `guest-rs/`.

Env vars: `AGENT_MODEL` (default `gpt-5`; the project's key also has
gpt-5-mini/nano/gpt-5.1/gpt-5.2 — gpt-5 works via plain Chat Completions, no special
params), `OPENAI_API_KEY` (REQUIRED — `ClientFromEnv` fails at boot if unset; use `dummy`
for a keyless local endpoint), `OPENAI_BASE_URL` (optional; any OpenAI-compatible
endpoint). The orchestrator also reads `RESTATE_ADMIN_URL` (+ `RESTATE_AUTH_TOKEN` as the
Admin-API bearer) for handler discovery.

**Deploy (`agent.Deploy(ctx, tunnelName, defs...)` in serve.go, used by BOTH examples):**
always connects OUTBOUND to Restate Cloud via `github.com/restatedev/sdk-go/x/tunnel` — no
inbound listener or public URL, no local-listener mode. It sets ONLY the tunnel name
(`tunnel.WithTunnelName(tunnelName)` when non-empty — the examples pass `agent` /
`backoffice` so co-deployed services don't collide; empty → the tunnel reads
`RESTATE_INPROC_TUNNEL_NAME`). Everything else the tunnel reads itself from the
operator-injected env: `RESTATE_INPROC_ENVIRONMENT_ID`, `RESTATE_INPROC_SIGNING_PUBLIC_KEY`,
`RESTATE_AUTH_TOKEN` (or `RESTATE_INPROC_AUTH_TOKEN_FILE`), and `RESTATE_TUNNEL_SERVERS_SRV`
(or `RESTATE_INPROC_CLOUD_REGION`). See the README's "Deploying through a tunnel".

### Run end-to-end (Restate Cloud, via the tunnel)
```bash
# set the tunnel env (see README "Deploying through a tunnel"), then:
OPENAI_API_KEY=sk-... go run ./examples/orchestrator &   # tunnels out; self-registers
OPENAI_API_KEY=sk-... go run ./examples/backoffice &     # tunnels out; self-registers
# invoke via your Restate Cloud environment's ingress (object key = session id):
#   POST <ingress>/Agent/<session>/Ask     {"message":"fulfill order #42: …"}
#   POST <ingress>/Agent/<session>/History
#   POST <ingress>/Agent/<session>/Reset
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
    └─ Sandbox.RunProgram(code) ─▶ engine.RunLive drives the program as a LIVE coroutine:
            │  out = guest.start(assemble(code))   ← determinism prelude + tool bridge + program
            │  loop over guest steps:
            │    {s:0}  done   → return the program's answer
            │    {s:2}  error  → program error (fed back to the model)
            │    {s:1}  ops    → inv.Start(new ops)      submit each as a durable Future
            │                    res = inv.Next()        restate.WaitFirst → FIRST completion wins
            │                    out = guest.resolve/reject(res)   settle that ONE promise → next step
       program returns {done:true, answer}  → loop ends
       program returns anything else        → observation → next round
```

- **The loop is Go** (`RunAgent` in loop.go), running directly inside the `Ask`
  handler — it is NOT wrapped in `restate.Run`.
- **The only durable steps** are (1) each model call (in `openAIModel.Decide`) and
  (2) each tool call. Running the JS program is pure computation; on a Restate replay
  the whole Go loop re-runs, `guest.start` re-runs the program from the top, and each
  journaled model/tool step returns its captured value instead of re-executing.
- **The model always emits a JS program** as `{"thought","code"}`. The program
  (an async function body — write statements directly, do NOT wrap in a `function`)
  ends by returning `{done:true, answer}` to finish, or any other value which is
  fed back as an observation for the next round (self-correction).
- **Live-coroutine model (the guest keeps state for the life of ONE program):** the
  guest exports `start(script)` / `resolve(handle, json)` / `reject(handle, msg)`, each
  returning a step blob `{s:0 answer | s:1 ops | s:2 error}`; there is **NO wasm
  import** — the guest never calls back into Go. `start` runs the program to
  synchronous quiescence (draining the microtask queue); the JS `__hostCall` bridge
  (`bridgeJS` in sandbox.go) gives each tool call a deterministic integer handle,
  stashes the promise's resolvers in `globalThis.__pending`, records `{handle,name,arg}`
  in `globalThis.__outbox`, and returns the promise. Each step returns the ops pushed to
  `__outbox` since the last step (then clears it). The host submits those ops as durable
  futures, `restate.WaitFirst`s the whole in-flight set, and settles the **first**
  completion back into the guest via `__resolveJSON` / `__reject` — which resumes the
  program until the next quiescence. Settling one-at-a-time in completion order is what
  makes `Promise.race` / timeouts work; a `Promise.all` still parallelizes because every
  op in a step is submitted (its journal slot reserved) before any is awaited. State
  lives in a `thread_local` QuickJS runtime inside the instance for the duration of one
  program and is dropped/recreated on the next `start`. (Earlier designs: a fixed
  `webSearch`/`env.host_call` import, and a stateless *re-execution* guest with a
  `__journal` + `__frontier` — both gone; the current guest is a live coroutine, which
  is what enables first-completion semantics.)
- **Generic tools (no hardcoded names):** the sandbox prelude reads `globalThis.__toolNames`
  and defines each registered tool as a plain async JS function over `__hostCall`; the
  guest hardcodes no tool names. The only callable functions are the ones you register.
- **One tool type** (`agent.NewTool`) — a **leaf**: its body performs ONE non-blocking
  submission and returns a `Future[R]`, built ONLY via `agent.Run` (side effect →
  `RunAsync`), `agent.Call` / `agent.CallObject` (service call → `RequestFuture`),
  `agent.Timer` (durable timer → `After`), `agent.Awakeable`, or `agent.Signal` (a
  named signal on the invocation). A leaf tool's body must NOT block before returning
  its `Future` — it must only submit. `NewTool` auto-reflects the arg JSON Schema from
  `A` **and** the result schema from `R` (honoring `json:`/`jsonschema:` tags) and
  surfaces both to the model. The rationale — why "a tool is a single durable op", and
  why a multi-step operation belongs in its own handler rather than a special tool kind
  — is in `DESIGN.md`.
- **Multi-step operations = ordinary Restate handlers the tool calls.** There is no
  seq-tool. Write a normal blocking handler (data-dependent steps, nested runs, timers,
  service calls) and expose it either by having a leaf tool `agent.Call` it, or by
  annotating it with `AgentToolAnnotation` so **discovery** builds the tool. It runs in
  its own invocation, so it may block/orchestrate freely AND still parallelize with
  sibling tools — at the cost of one invocation hop. Such handlers are
  **session-stateless** and must **not** call back into their own `Agent/<key>` (the
  parent holds that lock while awaiting them → deadlock; discovery always skips the
  `Agent` object for this reason).
- **Handler discovery (`discover.go`):** handlers whose metadata carries
  `AgentToolAnnotation` (`"restate/agent"`) are pulled from the Restate Admin API and
  turned into leaf tools that `Call`/`CallObject` them (a keyed Virtual Object / Workflow
  handler takes `{key, input}`; a plain Service handler takes the input directly). Two
  entry points: `DiscoverTools(ctx, cfg)` fetches once at STARTUP (for already-registered
  services); `Config.Discover` discovers lazily **per Ask** and JOURNALS the descriptors
  (via a named `restate.Run`), so the tool set — and thus the system prompt and dispatch
  table — is identical across replays. Prefer `Config.Discover` (the orchestrator example
  uses it) when the target services register independently of the agent — a separate
  deployment, or one co-deployed but not visible until after startup — since it is
  order-independent and picks up services registered between turns. The annotation value,
  if non-empty, becomes the tool name (sanitized to a JS identifier). `DiscoverConfig`
  carries the `AdminURL` and an optional `AuthToken` sent as `Authorization: Bearer` —
  REQUIRED against Restate Cloud, whose Admin API (`https://<env>.env.<region>.restate.cloud:9070`)
  rejects unauthenticated requests; the orchestrator passes `RESTATE_AUTH_TOKEN`.
- **Named signals (`AgentSignals`):** `agent.Signal[R](ctx, name)` blocks a leaf tool on
  a named signal on the current invocation. The framework binds a keyless `AgentSignals`
  service whose `resolve` / `reject` handlers complete a signal by `(invocation, name)`;
  both are annotated for discovery, so a discovering agent gets them as tools — that's how
  an external caller completes a named signal (e.g. a human-in-the-loop approval). The
  orchestrator registers the `signal` tool but its fulfillment flow no longer blocks on
  approval (kept simple for a live demo); the capability is still available.
- **Sessions:** the service is a Restate **Virtual Object** keyed by session id.
  `Ask` loads the transcript from state (`restate.Get`), runs the loop continuing
  from prior context, and persists it (`restate.Set`) on success only.
  `History` (shared read handler) returns the transcript; `Reset` clears it.

---

## Invariants to preserve (don't regress these)

- **Determinism / replay:** on a Restate replay `guest.start` re-runs the program from
  the top and the host feeds the journaled completions back through `WaitFirst` (driven in
  ascending-handle order so even a `Promise.race` winner is reproduced — see the Parallel &
  race bullet), so the program re-derives identically — hence its clock & randomness MUST be
  frozen (sandbox.go's `detPreludeJS` overrides `Math.random`, the `Date` constructor,
  `Date.now`, `crypto`, `performance.now`; engine.go pins the WASI clock/rand via wazero
  `WithWalltime`/`WithRandSource` as a backstop). ONE seed is minted per program (from
  `restate.Rand(ctx).Int64()`; xor-mixed with a per-program `callSeq`) and the clock is
  captured once in a journaled `restate.Run`, so a program's operation sequence — and
  thus its deterministic handles — are identical across replays. `TestReplayDeterminism`
  / `TestDeterminism` / `TestDateFreezeNotEscapableViaConstructor` guard this.
- **Parallel & race via one driver:** `restateInvoker` implements `Start` (submit each
  op of a step as an in-flight durable Future — non-blocking, reserving a journal slot)
  and `Next` (drive the whole in-flight set with ONE `restate.WaitFirst`, returning the
  FIRST to settle). Submitting the whole step before awaiting is what makes a
  `Promise.all` batch run in PARALLEL; returning first-completion is what makes
  `Promise.race`/timeouts work (`TestParallelTools`, `TestPromiseAllFrontier`,
  `TestPromiseRaceFirstWins`, `TestLargeFrontier`). `Next` passes the futures to
  `WaitFirst` in ASCENDING-HANDLE order (`sort.Ints`) so the `Promise.race` winner is
  replay-stable when ≥2 are complete at one poll — `WaitFirst` breaks the tie by input
  order, and handles are deterministic. (This needs the SDK's ordered `WaitIterator`;
  ≤ v1.0.0 tie-broke by Go map order and sorting didn't help — `go.mod` pins the fixed
  `sdk-go` ≥ v1.0.1 (v1.0.2 pinned). See `DESIGN.md`.) An op that can't even be submitted
  (unknown tool / bad args) is a FATAL condition: `Start` **panics**, aborting the whole
  program — it is NOT demoted to a per-op rejection the JS could swallow. A leaf tool that
  returns a zero-value `Future{}` is likewise rejected at `submit` with a terminal error
  (never a nil-panic-then-retry-forever). If the guest quiesces still-running with nothing
  in flight (a JS-level deadlock, e.g. `await new Promise(()=>{})`), `Next` returns a clean
  terminal error instead of calling `restate.WaitFirst` with zero futures (which panics).
  `Reset` clears leftover in-flight ops between programs, since the guest resets its handle
  counter each `start`.
- **Cancellation is fatal, not a tool failure:** when `restate.WaitFirst` yields a
  cancellation, `Next` returns it as a fatal error and `RunLive` returns it up to the `Ask`
  handler (it is NOT fed back to the guest as a per-tool rejection a defensive program
  could swallow, which would "succeed" a cancelled turn and persist state). A genuine guest
  **trap** is different: it surfaces as a panic in `call1` (guest.go) and — since the
  `RunLive` recover is removed (as of `288bccf`, "one should not catch panics") — that
  panic propagates to the SDK handler. Either way the deferred `release` still runs during
  the unwind with `healthy == false`, so a trapped instance is retired, not pooled.
  Ordinary tool failures instead arrive as rejected JS promises → a *non-terminal* program
  error, which `RunAgent` DOES feed back for self-correction; a *terminal* `RunProgram`
  error is returned immediately and not persisted.
- **Pooled instances, EXCLUSIVE checkout:** guest instances are reused across Runs
  from a pool (`Engine.free`), not instantiated per Run — this removed the per-round
  allocation churn (measured in `bench_test.go`). The guest is single-threaded and NOT
  reentrant, so reuse is safe ONLY because each instance is checked out by exactly one
  Run at a time (`acquire`/`release`): never hand one instance to two goroutines
  (`TestConcurrentRunsAreIsolated`, run with `-race`, guards this). Reuse needs no reset
  because a fresh `start` drops and recreates the guest's QuickJS runtime, so nothing
  leaks between programs (`TestPoolReuseIsolation`); the host `guest_dealloc`s its
  per-call buffers so they don't accumulate. An instance is RETIRED (not returned to the
  pool) if the Run trapped/timed out, served `maxRunsPerInstance` (256), or grew past
  `instanceMemHighWater` (64 MiB). New instances are instantiated with
  `context.Background()` so their lifetime isn't tied to a Run's (cancellable) ctx; only
  the guest CALLS carry the per-Run ctx (cancellation).
- **No infinite retries:** deterministic give-ups (`ErrMaxRounds`) are returned as
  *terminal* errors by `Ask` (`restate.ToTerminalError`). A non-terminal error
  would make Restate retry the same deterministic failure forever.
- **State only on success:** `Ask` does `restate.Set(history)` only after the loop
  succeeds, so a failed turn leaves the session unchanged.
- **Safety:** wazero runtime built with `WithCloseOnContextDone(true)` +
  `WithMemoryLimitPages` (256 MiB); the guest also sets `set_memory_limit` (256 MiB) /
  `set_max_stack_size` (2 MiB) on its runtime. There is NO per-program wall-clock timeout
  anymore (the old `SetProgramTimeout` is gone); `WithCloseOnContextDone` only lets an
  invocation cancellation interrupt a stuck guest. The sole backstop against a runaway
  tool-calling loop is `maxProgramSteps` (65536) — in the live model each step settles ONE
  op, so this caps the TOTAL ops (parallel width + sequential depth both count) a single
  program may complete; it is set well above any realistic `Promise.all` fan-out
  (`TestLargeFrontier` runs a 5000-op program). QuickJS is compiled with `NDEBUG`
  (guest-rs/.cargo/config.toml) so its teardown sweep frees the arena instead of tripping
  a debug refcount assert; the guest also drains throwing microtasks fully and
  `std::mem::forget`s a phantom `JobException` ref to avoid an unbalanced
  `JS_FreeContext` (`TestThrowingMicrotaskContained`, `TestManyThrowingMicrotasksContained`).
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
  use `json.Marshal(out)`, which is always valid. `Next` also guards against a tool
  returning non-JSON (`json.Valid`) and surfaces it as a per-op error.

---

## Status & known limitations

- **Verified end-to-end live once** (Docker + real OpenAI `gpt-5`), but on an EARLIER
  internal model (pre live-coroutine, pre-discovery): the model reasoned, self-corrected
  on a bad program, called durable tools, ran parallel tools, persisted session history,
  and a repeated `Idempotency-Key` ran the model once. The current live-coroutine +
  discovery build passes the full offline suite (~30 tests, incl. `-race`), `go vet
  ./...`, and gofmt clean, but has **not** been re-verified live end-to-end.
- **The design was adversarially audited** (multi-dimension review + verify); confirmed
  findings are fixed (infinite-retry, runaway loop → `maxProgramSteps`, memory cap,
  self-correction, tool-JSON validation, empty-Future rejection, throwing-microtask
  containment, `Date`-constructor escape, non-mutating WASI rand source).
- **Guest is Rust (`guest-rs/`, rquickjs) and instances are pooled.** ~600 KB,
  `wasm32-wasip1`; it replaced an earlier C guest. Its ABI is the LIVE-COROUTINE trio
  `start` / `resolve` / `reject` (plus `guest_alloc` / `guest_dealloc`), each returning a
  packed `(ptr<<32)|len` blob `{s:0 answer | s:1 ops | s:2 error}`, with NO host import.
  A persistent `thread_local` QuickJS runtime holds one program's state across the trio;
  it is dropped/recreated on each `start`. Pooling (reuse with exclusive checkout) removed
  the per-round allocation churn a concurrent-session benchmark had flagged as the one
  real driver; latency is unchanged in practice (~1 ms per guest call, ≪ the LLM call).
- **Graceful shutdown:** `Engine.Close` stops accepting new Runs and waits (bounded by
  ctx) for in-flight Runs before closing the runtime, so it can't close an instance
  under an active guest call (`TestGracefulShutdownWaitsForInflight`).
- **Demo tools are illustrative:** the back-office's `Inventory`/`RiskCheck`/`Payments`
  use toy heuristics (seed 100 stock/SKU, flag orders ≥ $1000) — replace with real
  capabilities.
- **Committed on `main`.**

## Dependencies (all published; no replace)
- `github.com/restatedev/sdk-go v1.0.2` (Restate Go SDK; the ordered WaitIterator that
  makes the `Promise.race` winner replay-stable landed in v1.0.1 — see the race caveat)
- `github.com/restatedev/sdk-go/x/tunnel v0.1.1` (preview; outbound Restate Cloud tunnel
  used by `agent.Deploy` to connect outbound to Restate Cloud)
- `github.com/tetratelabs/wazero v1.9.0` (pure-Go WASM runtime; NO cgo — chosen
  over wasmer-go which is cgo + unmaintained)
- `github.com/openai/openai-go/v3 v3.41.0` (official, GA)
- `github.com/invopop/jsonschema v0.14.0` (tool arg schemas; same as sdk-go)
