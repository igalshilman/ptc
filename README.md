# quickjs-worker-go — a durable CodeAct AI agent

A Restate durable-execution agent on the **CodeAct** pattern: each round the LLM
writes a small **JavaScript program** that calls developer-registered Go **tools**;
the program runs in an embedded **QuickJS** interpreter (Rust/`rquickjs` → WASM,
driven by wazero), and every model call and tool call is a durable, journaled
Restate step — so the agent survives crashes and replays deterministically.

Built on **Go + wazero (WASM) + QuickJS (rquickjs) + restatedev/sdk-go + OpenAI**.
`cmd/agent` is the entry point; `agent/` is the reusable engine. See `CLAUDE.md` for
the full map and invariants, and `DESIGN.md` for the tool-abstraction rationale.

## How it works

- The agent is a **Virtual Object**: each object key is an independent, durable
  **session** whose transcript is persisted as object state. Handlers:
  `Ask {"message":...}` (drive a turn), `History {}` (shared read-only → transcript),
  `Reset {}` (clear).
- The **agent loop runs in Go** (a plain loop, NOT a `restate.Run`) inside `Ask`.
  Each round the model returns `{thought, code}`; the durable step is the model
  call. The `code` runs in QuickJS and ends by returning `{done:true, answer}` to
  finish, or any other value which is fed back as an observation for the next round
  (self-correction until it finishes or hits a round budget).
- The program calls **tools the developer registered in Go**. To the JS program
  each tool is a plain async function — nothing about Restate is visible.

```
Agent/<session>/Ask  →  RunAgent loop (plain Go, NOT a restate.Run)
  each round:
    ├─ model.Decide ── restate.Run ─▶ OpenAI ─▶ {code}                 (durable step)
    └─ Sandbox.RunProgram(code) ─▶ QuickJS runs the program
         await Promise.all([a(x), b(y)])            ← plain async JS, one batch
         └─ host_call ─▶ InvokeBatch: each call becomes a durable Future
              (leaf = in-process Run/Call/Timer │ seq = AgentTools/Exec sub-invocation)
              ─▶ one restate.Wait drives them in parallel              (durable steps)
       returns {done:true, answer} → done │ else → observation → next round
```

## Tools — one type, plus a sequence escape hatch

Every tool auto-reflects its argument **and** result JSON Schema (via
`invopop/jsonschema`), both surfaced to the model. The model can't tell the two
kinds apart; they differ only in how their durable Future is produced.

- **`NewTool[A, R](name, desc, func(restate.Context, A) (agent.Future[R], error))`**
  — a *leaf* tool: its body performs ONE non-blocking submission and returns the
  `Future`, built via `agent.Run` (side effect), `agent.Call`/`agent.CallObject`
  (service call), `agent.Timer` (durable timer), or `agent.Awakeable`. A
  `Promise.all` of leaf tools runs durably **in parallel**, in-process.
- **`NewSeqTool[A, R](name, desc, func(restate.Context, A) (R, error))`** — a
  *sequence* tool: an ordinary blocking, multi-step handler (service calls, timers,
  awakeables, data-dependent steps). It runs in its own `AgentTools/Exec`
  **sub-invocation**, so it may orchestrate freely and still parallelize with
  sibling tools — at the cost of one invocation hop.

Concurrency contract: `InvokeBatch` submits every call in a batch, drives them with
one `restate.Wait`, and reads results by index — so `Promise.all` parallelizes and
sequential `await`s serialize, deterministically. See `DESIGN.md` for the "why".

## Run

```bash
OPENAI_API_KEY=sk-...  go run ./cmd/agent      # serves the Agent Virtual Object on :9080
go test ./...                                  # engine, sandbox, loop, determinism, pooling, sessions
```

Env: `AGENT_MODEL` (default `gpt-5`), `AGENT_ADDR` (default `:9080`),
`OPENAI_BASE_URL` (any OpenAI-compatible endpoint), `OPENAI_API_KEY` (required;
`setup()` exits at boot if unset — use `dummy` for a keyless local endpoint).

### Against a real Restate runtime
```bash
OPENAI_API_KEY=sk-... go run ./cmd/agent                       # :9080
docker run -d --name restate -p 8080:8080 -p 9070:9070 \
  --add-host=host.docker.internal:host-gateway restatedev/restate:latest
curl -X POST http://localhost:9070/deployments \
  -H content-type:application/json -d '{"uri":"http://host.docker.internal:9080"}'
# talk to a session (object key = session id):
curl http://localhost:8080/Agent/s1/Ask -H content-type:application/json -d '{"message":"what is 6 times 7?"}'
curl -X POST http://localhost:8080/Agent/s1/History     # transcript (empty body — Void input)
curl -X POST http://localhost:8080/Agent/s1/Reset       # clear the session
```
> Verified live (Docker + OpenAI gpt-5): compute via a leaf tool; a `delayed_fetch`
> seq tool running as its own sub-invocation; parallel tools; multi-turn session
> memory; and idempotent replay (a repeated `Idempotency-Key` runs the model once).

## The guest

The QuickJS guest is Rust/`rquickjs` compiled to `wasm32-wasip1` (`guest-rs/`),
embedded via `//go:embed` as a COMMITTED artifact so `go build` needs only the Go
toolchain. Rebuild it with `make guest-rs`. It's Restate-agnostic: it exposes ONE
generic bridge — `__hostCall(name, argJSON)` over the `env.host_call` import — and
the sandbox prelude defines each registered tool on top of it, so the guest
hardcodes no tool names; the only callable functions are the ones you register.

## Layout

```
cmd/agent/            package main — THE entry point (user code)
  main.go               setup() wires client+tools+loop; main() binds handlers & serves
  tools.go              the developer's tools (compute / http_get / wait / delayed_fetch)
agent/                package agent — the reusable durable-CodeAct engine (infra)
  engine.go             wazero driver + instance pool; the single env.host_call import
  guest.go              cached guest exports + linear-memory helpers
  wasm.go               //go:embed quickjs_guest.wasm
  sandbox.go            runs one JS program; tool prelude + JS-level determinism prelude
  loop.go               RunAgent (the Go loop) + Model/Decision + BuildSystemPrompt
  tool.go               Tool, NewTool (leaf → Future), NewSeqTool; Future + Run/Call/Timer/… helpers
  service.go            Config/Service/NewService/Definitions; Ask/History/Reset; restateInvoker; openAIModel
  quickjs_guest.wasm    the embedded guest (committed)
guest-rs/             the QuickJS guest — Rust/rquickjs → wasm32-wasip1
```

## Durability, determinism, safety

- **Replay:** on crash/replay the handler re-runs the Go loop from the top; each
  journaled model call and tool call returns its captured value instead of
  re-executing. Deterministic give-ups (`ErrMaxRounds`) are surfaced as *terminal*
  errors so Restate never retries them forever; session state is persisted only on
  success.
- **Determinism:** because a program is re-run verbatim on replay, its clock and
  randomness are frozen in the JS prelude (the `Date` constructor, `Date.now`,
  `Math.random`, and `crypto`) — done at the JS level so it survives guest-instance
  reuse; the WASI clock/rand are pinned to constants as a backstop. The seed comes
  from `restate.Rand(ctx)` and the clock is captured once in a journaled step.
- **Pooling:** guest instances are reused (EXCLUSIVE checkout + a FRESH `JSContext`
  per run for cross-session isolation), cutting per-round allocation ~10 MB → ~82 KB.
  `Engine.Close` drains in-flight runs before shutting the runtime down.
- **Safety:** per-program wall-clock timeout (`WithCloseOnContextDone` +
  `SetProgramTimeout`); a 256 MiB memory cap (wazero + `JS_SetMemoryLimit`); the
  pending-call list grows (no arbitrary cap) with a high graceful ceiling; malformed
  model output and tool errors are fed back as observations rather than being fatal.

The design was adversarially reviewed; `CLAUDE.md` records the invariants and the
known limitations.
