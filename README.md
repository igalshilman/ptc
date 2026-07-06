# quickjs-worker-go — a durable CodeAct AI agent

A Restate durable-execution agent built on the **CodeAct** pattern:

- The agent is a **Virtual Object**: each object key is an independent, durable
  **session** whose conversation transcript is persisted as object state, so it
  remembers across calls. Session API: `Ask {"message":...}` (drive a turn),
  `History {}` (read-only shared handler → transcript), `Reset {}` (clear).
- The **agent loop runs in Go** (a plain loop, NOT a `restate.Run`), inside the
  `Ask` handler.
- Each round the model **always writes a JavaScript program** (a mini workflow)
  that manipulates the tools. The **LLM call is the only durable `restate.Run`**
  in the loop.
- The generated **program runs in QuickJS** and calls **tools the developer
  registered in Go**. Each tool call **traps back into Go** and runs as its own
  durable `restate.Run` — so once a tool succeeds its result is deterministically
  captured and never re-run on replay. Tools hold a live `restate.Context`
  (durable timers, service calls, state, …); to the JS program each is just a
  plain async function — nothing about Restate is visible.
- Each tool carries a **JSON Schema** for its argument, surfaced to the model in
  the system prompt. Two tool styles:
  - **`NewTool[Args]`** — a *context tool* that holds the live `restate.Context`
    (durable timers, service calls, state). Runs sequentially within a batch.
  - **`NewRunTool[Args]`** — a *run tool* whose body is one durable step (HTTP,
    compute). The agent wraps it in `restate.RunAsync`, so a `Promise.all` over
    run-tools executes **durably in parallel**.
  Both reflect their arg schema from a Go struct (via `invopop/jsonschema`).
- The program signals completion by returning `{done:true, answer}`; any other
  return value is fed back as an **observation** and the model writes the next
  program — so the loop self-corrects until it finishes or hits a round budget.

```
 Agent/<session>/Ask handler  →  RunAgent loop   (plain Go loop, NOT a restate.Run)
   each round:
     ├─ model.Decide ── restate.Run ─▶ OpenAI ─▶ Decision{ code }        (durable step)
     └─ Sandbox.RunProgram(code) ─▶ QuickJS (wazero) runs the program
             │  await calc(...) / await http_get(...)      ← plain async JS
             └─ traps to Go ─▶ tool.Handler(restate.Context) ── restate.Run ─▶ side effect   (durable step)
        program returns {done:true, answer}  → loop ends
        program returns anything else        → observation → next round
```

> **No hardcoded tools.** The guest (`guest/guest.c`) exposes a single generic
> bridge — `__hostCall(name, argJSON)` over the `env.host_call` import — and the
> sandbox prelude defines each registered tool as a plain JS function on top of
> it. The guest hardcodes no tool names, so the only callable functions are the
> ones the developer registers.

## Project layout

**Entry point: `cmd/agent`** — that's the one `main`. Infra vs. user code is split:

```
cmd/agent/            package main — THE entry point (user code)
  main.go               setup() wires client+tools+loop; main() binds the handler & serves
  tools.go              the developer's durable tools (compute / http_get / wait)
agent/                package agent — the reusable durable-CodeAct engine (infra)
  engine.go             wazero driver for the guest; registers the single env.host_call import
  guest.go              cached guest exports + memory helpers
  wasm.go               //go:embed quickjs_guest.wasm
  sandbox.go            runs one JS program; tool prelude over __hostCall; Invoker/BatchInvoker
  loop.go               RunAgent (the Go loop) + Model/Decision/Conversation + BuildSystemPrompt
  tool.go               Tool, NewTool, NewRunTool (typed handlers + reflected JSON Schema)
  service.go            Config, Service, NewService, Definition; the Ask/History/Reset handlers,
                        restateInvoker (durable dispatch), openAIModel (durable model call)
  quickjs_guest.wasm    the embedded guest
guest/                guest.c + Dockerfile.build (C source; not a Go package)
```

The user writes tools + `setup()`; everything else is the reusable `agent` package.
`Config`/`NewService`/`Service.Definition` is the whole public surface `cmd/agent` uses.

## Run
```bash
OPENAI_API_KEY=sk-...  go run ./cmd/agent      # serves the Agent Virtual Object on :9080
go test ./...                                  # engine, sandbox, loop, determinism, parallelism, sessions
```

## Sessions (multi-turn memory)
The `Ask(session, message)` handler loads the session transcript from state
(`restate.Get`), runs the loop continuing from prior context, and persists it
back (`restate.Set`) on success — so follow-ups like "add 1 to that" build on the
previous answer. State is not written on failure, so a failed turn leaves the
session unchanged. `History` (a shared read-only handler) returns the transcript
and `Reset` clears it.

## Run against a real Restate runtime
The deps + `replace github.com/restatedev/sdk-go => ../../sdk-go` are already in
`go.mod`. Start the agent and Restate, then register:
```
OPENAI_API_KEY=sk-... go run ./cmd/agent          # serves on :9080
docker run -d --name restate -p 8080:8080 -p 9070:9070 \
  --add-host=host.docker.internal:host-gateway restatedev/restate:latest
curl -X POST http://localhost:9070/deployments \
  -H content-type:application/json -d '{"uri":"http://host.docker.internal:9080"}'
```
Talk to a session (object key = session id):
```
curl http://localhost:8080/Agent/sess1/Ask -H content-type:application/json -d '{"message":"..."}'
curl -X POST http://localhost:8080/Agent/sess1/History   # transcript (empty body — Void input)
curl -X POST http://localhost:8080/Agent/sess1/Reset     # clear the session
```
Set `OPENAI_BASE_URL` to point at any OpenAI-compatible endpoint; `AGENT_MODEL`
(default `gpt-4o-mini`) and `AGENT_ADDR` (default `:9080`) are also configurable.

> Verified live (Docker + real OpenAI): `Ask{"message":"multiply 21 by 2"}` → `{"answer":"42"}`;
> the model self-corrects on a bad program; history persists across invocations;
> a repeated `Idempotency-Key` runs the model once.

## Durability contract
On crash/replay the handler re-runs the Go loop from the top; each round's
`restate.Run` (the model call) and each tool's `restate.Run`/`restate.Sleep`
return their **journaled** values instead of re-executing, so the agent resumes
deterministically. Each program runs in a fresh QuickJS instance (no JS state
shared across rounds — state flows via observations).

### Determinism
Because a program is re-run verbatim on replay, its randomness and clock are
frozen to replay-stable values, so a program that does `Math.random()` /
`Date.now()` / `new Date()` to compute a tool argument produces the same argument
on every replay (no journal divergence):
- `Math.random` and `Date.now` are overridden in the sandbox prelude (seeded LCG
  + frozen clock);
- the WASI clock and random source are also frozen at the wazero level, so
  `new Date()` is stable too;
- in production the seed comes from `restate.Rand(ctx)` and the clock is captured
  once in a journaled `restate.Run` — both identical across replays.

### Robustness & safety (from an adversarial audit)
- **No infinite retries.** Deterministic give-ups (round-budget exhaustion →
  `ErrMaxRounds`) are surfaced as *terminal* errors by `Ask`, so Restate never
  retries a run that would just re-fail identically on replay.
- **Runaway-program guard.** The runtime is built with `WithCloseOnContextDone`
  and each program runs under a wall-clock budget (`SetProgramTimeout`), so a
  model-generated `while(true){}` is interrupted and reported (fed back to the
  model) instead of pinning a goroutine forever.
- **Memory cap.** Each guest instance is capped at `maxMemoryPages` (256 MiB) via
  wazero, a backstop against an allocate-until-OOM program.
- **Self-correction.** A malformed model decision (non-JSON / prose-wrapped) is
  leniently parsed and, if still bad, fed back as an observation (`ErrBadDecision`)
  rather than killing the run; an empty program is likewise fed back; a
  `{done:true}` with no usable answer is not accepted as completion.
- **Clear tool errors.** A tool returning invalid JSON produces a tool-named
  error, not an opaque JS parse rejection.

- **Concurrent-call cap.** The guest caps in-flight calls at `MAX_PENDING`
  (4096); on overflow — or when the host refuses a call — it **rejects the
  promise with a clear error** rather than stranding it, so a huge `Promise.all`
  fails cleanly instead of hanging. The guest also sets `JS_SetMemoryLimit`
  (256 MiB) and `JS_SetMaxStackSize`, backing the wazero-level memory cap inside
  the interpreter.
