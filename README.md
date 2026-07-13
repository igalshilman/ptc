# quickjs-worker-go — a durable CodeAct AI agent

A [Restate](https://restate.dev) durable-execution agent built on the **CodeAct**
pattern: each round the LLM writes a small **JavaScript program** that calls
developer-registered Go **tools**; the program runs in an embedded **QuickJS**
interpreter (Rust/`rquickjs` → WASM, driven by [wazero](https://wazero.io)), and
every model call and tool call is a durable, journaled Restate step — so the agent
survives crashes and replays deterministically.

Built on **Go 1.25 + wazero (WASM) + QuickJS (rquickjs) + restatedev/sdk-go + OpenAI**.
No cgo. `go build ./...` works with only the Go toolchain (the QuickJS guest is a
committed prebuilt `.wasm`).

See [`CLAUDE.md`](./CLAUDE.md) for the full map and the invariants, and
[`DESIGN.md`](./DESIGN.md) for the tool-abstraction rationale.

## How it works

- The agent is a Restate **Virtual Object**: each object key is an independent, durable
  **session** whose transcript is persisted as object state. Handlers:
  `Ask {"message":...}` (drive a turn), `History {}` (shared read-only → transcript),
  `Reset {}` (clear).
- The **agent loop runs in Go** (a plain loop, NOT a `restate.Run`) inside `Ask`. Each
  round the model returns `{thought, code}`; the durable step is the model call. The
  `code` (an async function body) runs in QuickJS and ends by returning
  `{done:true, answer}` to finish, or any other value which is fed back as an
  observation for the next round (self-correction until it finishes or hits a round
  budget).
- The program calls **tools the developer registered in Go**. To the JS program each
  tool is a plain async function — nothing about Restate is visible.

```
Agent/<session>/Ask  →  RunAgent loop (plain Go, NOT a restate.Run)
  each round:
    ├─ model.Decide ── restate.Run ─▶ OpenAI ─▶ {code}                    (durable step)
    └─ Sandbox.RunProgram(code) ─▶ engine.RunLive drives it as a LIVE coroutine:
         out = guest.start(assemble(code))          ← determinism prelude + tool bridge
         loop over guest steps:
           {s:0} done   → return the program's answer
           {s:2} error  → program error (fed back to the model)
           {s:1} ops    → inv.Start(ops)            submit each as a durable Future
                          res = inv.Next()          restate.WaitFirst → FIRST wins
                          out = guest.resolve/reject(res)   settle ONE promise → next step
       returns {done:true, answer} → done │ else → observation → next round
```

Settling promises **one at a time, in completion order** is what makes `Promise.race`
and timeouts work; a `Promise.all` still parallelizes because every op in a step is
submitted (its journal slot reserved) before any is awaited.

## Tools — one kind, and "a handler is a tool"

There is a single tool constructor. A tool is exactly **one durable op**:
`agent.NewTool[A, R]` takes a body that performs ONE non-blocking submission and returns
the resulting `Future[R]`, built via one of:

| the op | helper |
|---|---|
| side effect (HTTP, DB, compute) | `agent.Run` |
| call another service | `agent.Call` |
| call a keyed VO / Workflow handler | `agent.CallObject` |
| durable timer | `agent.Timer` |
| external completion (system id) | `agent.Awakeable` |
| external completion (named) | `agent.Signal` |

Arg **and** result JSON Schemas are auto-reflected (via `invopop/jsonschema`) and
surfaced to the model. The `Future`'s internals are unexported, so a tool cannot
fabricate a future that isn't backed by a real durable submission, and its body must
not block before returning it.

A **multi-step, blocking operation** (data-dependent steps, its own timers or service
calls) needs no special tool type — it is just an ordinary Restate handler. Expose it
either by having a leaf tool `agent.Call` it, or by annotating it with
`AgentToolAnnotation` (`"restate/agent"`) so **discovery** builds the tool automatically.
The handler runs in its own invocation, where it may block/branch freely; to the batch
driver the call is just another service-call future. See [`DESIGN.md`](./DESIGN.md) for
why.

## Examples

Two runnable demos under `examples/`, meant to run as two separate Restate deployments:

- **`orchestrator`** — the **order-fulfillment agent** (a tiny `main()` that hands a tool
  set to `agent.Main`). It discovers back-office handlers via the Restate Admin API and
  adds two static tools — `sleep` (a durable timer) and `signal` (a named-signal
  human-approval step completed by an external caller).
- **`backoffice`** — a standalone Restate deployment of the handlers the agent drives:
  `Inventory` / `RiskCheck` / `Payments`, each annotated with `AgentToolAnnotation` so
  discovery turns it into a tool. It knows nothing about the agent — it's just an
  ordinary service deployment, which is the point: register both with Restate and the
  agent discovers it across the Admin API.

## Run

```bash
go build ./...                                          # builds the engine + both examples
go test ./...                                           # engine, sandbox, loop, determinism, pooling, sessions
OPENAI_API_KEY=sk-...  go run ./examples/orchestrator   # the agent, on :9080
                       go run ./examples/backoffice     # the discovered handlers, on :9081
```

A [Nix](https://nixos.org) dev shell with the full toolchain (Go, plus Rust with the
`wasm32-wasip1` target for `make guest-rs`) is provided — `nix develop` (pins nixpkgs
via `flake.lock`).

Env: `OPENAI_API_KEY` (required — boot fails if unset; use `dummy` for a keyless local
endpoint), `AGENT_MODEL` (default `gpt-5`), `AGENT_ADDR` (default `:9080`),
`OPENAI_BASE_URL` (any OpenAI-compatible endpoint). The orchestrator also reads
`RESTATE_ADMIN_URL` (default `http://localhost:9070`) for handler discovery; the
back-office reads `BACKOFFICE_ADDR` (default `:9081`).

### Against a real Restate runtime

```bash
OPENAI_API_KEY=sk-... go run ./examples/orchestrator &        # agent on :9080
                      go run ./examples/backoffice &          # back-office on :9081
docker run -d --name restate -p 8080:8080 -p 9070:9070 \
  --add-host=host.docker.internal:host-gateway restatedev/restate:latest
# register BOTH deployments so the agent can discover the back-office:
curl -X POST http://localhost:9070/deployments \
  -H content-type:application/json -d '{"uri":"http://host.docker.internal:9080"}'
curl -X POST http://localhost:9070/deployments \
  -H content-type:application/json -d '{"uri":"http://host.docker.internal:9081"}'
# talk to a session (object key = session id):
curl http://localhost:8080/Agent/s1/Ask -H content-type:application/json \
  -d '{"message":"fulfill order #42: 3x SKU-1, 1x SKU-9, total $1200"}'
curl -X POST http://localhost:8080/Agent/s1/History     # transcript (empty body — Void input)
curl -X POST http://localhost:8080/Agent/s1/Reset       # clear the session
```

## The guest

The QuickJS guest is Rust/`rquickjs` compiled to `wasm32-wasip1` (`guest-rs/`), embedded
via `//go:embed` as a **committed** artifact (`agent/quickjs_guest.wasm`, ~600 KB) so
`go build` needs only the Go toolchain. Rebuild it with `make guest-rs` after editing
`guest-rs/`.

### The host ↔ guest ↔ JS protocol

Three parties cooperate to run one program:

- the **Go host** (`agent/engine.go` `RunLive`, `agent/guest.go`) — owns durability;
- the **WASM guest** (`guest-rs/`, Rust/rquickjs) — owns the JS runtime;
- the **JS bridge** (`agent/sandbox.go`) — injected JS that turns tool calls into an
  outbox the host drains.

There is **no wasm import**: the guest never calls back into the host. All communication
is the host calling guest exports and reading a **step blob** each one returns.

**The exports (`guest-rs/src/abi.rs`).** The only `extern` surface:

| export | purpose |
|---|---|
| `guest_alloc(size) -> ptr` / `guest_dealloc(ptr, size)` | host writes inputs / frees outputs in linear memory |
| `start(scriptPtr, scriptLen) -> u64` | begin a program; return the first step |
| `resolve(handle, valuePtr, valueLen) -> u64` | settle promise `handle` with a JSON value; return the next step |
| `reject(handle, msgPtr, msgLen) -> u64` | reject promise `handle` with an error message; return the next step |

`start` / `resolve` / `reject` each return a `u64` that packs the result location as
`(ptr << 32) | len`. The host reads those `len` bytes of JSON (`guest.readBlob`), copies
them out, and `guest_dealloc`s the buffer. The blob is always one of three shapes (the
`s` discriminant matches `outDone`/`outCalls`/`outError` in `engine.go`):

| blob | meaning |
|---|---|
| `{"s":0,"r":<value>}` | program **returned** `<value>` — done |
| `{"s":1,"ops":[{handle,name,arg},…]}` | program is **still running**; these are the new tool calls it made this step |
| `{"s":2,"error":"…"}` | program **threw** (or a syntax/eval error) |

**The assembled script.** For each program the host concatenates (in `Sandbox.RunProgram`):
a **determinism prelude** (freezes `Math.random`, the `Date` constructor/`Date.now`,
`crypto`, `performance.now` to this program's seeded values) + the **tool bridge** + the
model's **program**, wrapped in an async IIFE whose `.then/.catch` stores the final
`{s:0,r}` / `{s:2,error}` into `globalThis.__output`.

**The JS bridge (`bridgeJS`).** Each registered tool name becomes a plain async function
`name(arg) → __hostCall(name, arg)`. `__hostCall`:

1. mints a deterministic integer `handle` (a per-program counter starting at 0),
2. creates a `Promise` and stashes its `{resolve, reject}` in `globalThis.__pending[handle]`,
3. pushes `{handle, name, arg}` onto `globalThis.__outbox`,
4. returns the promise.

So JS control flow (`await`, `Promise.all`, `Promise.race`) works normally; the only
side effect of "calling a tool" is an entry in the outbox.

**The step loop.** `start` evaluates the script and runs the microtask queue to
**synchronous quiescence** — every promise that *can* settle without the host has. The
guest then reads a step: if the wrapper already stored `__output`, that's `{s:0}`/`{s:2}`;
otherwise it drains `__outbox` and returns `{s:1, ops}` (clearing the outbox, so each step
carries only the ops emitted *since the last one*). The host loop then:

```
out = start(script)                         → {s:1, ops:[a,b]}   (e.g. a Promise.all)
  RunLive:
    inv.Start(ops)     submit a,b as durable Restate futures (journal slots reserved)
    res = inv.Next()   restate.WaitFirst → FIRST of a,b to settle wins
    out = resolve(res.handle, res.value)    (or reject) → drive to quiescence → next step
  … repeat: each step settles exactly ONE promise, in completion order …
out = resolve(...)                          → {s:0, r}            → RunProgram returns r
```

Settling **one promise per step, in completion order** is what makes `Promise.race`,
`Promise.any`, and timeouts work (the losers stay pending). A `Promise.all` still runs in
parallel because *all* of a step's ops are submitted — their journal slots reserved —
before any is awaited. The `handle → {resolve, reject}` map and the deterministic handle
counter are what let the host address "that specific promise" across the boundary.

**State & determinism.** A persistent `thread_local` QuickJS runtime holds one program's
state across the `start → resolve/reject → …` sequence, and is dropped/recreated on the
next `start` (instances are pooled and checked out exclusively, so only one program is
ever live per instance). On a Restate replay the host re-runs `start` from the top and
feeds the journaled completions back in the same `WaitFirst` order; because the clock,
randomness, and handle counter are all frozen/deterministic, the program re-derives the
identical op sequence. QuickJS is built with `NDEBUG` so its teardown sweep doesn't trip a
debug refcount assert, and the guest drains throwing microtasks fully (leaking a phantom
`JobException` ref) to avoid an unbalanced `JS_FreeContext`.

## Durability, determinism, safety

- **Replay:** on crash/replay the handler re-runs the Go loop from the top; each
  journaled model call and tool call returns its captured value instead of re-executing,
  and the host feeds the completions back in the same `WaitFirst` order — so the program
  re-derives identically. Deterministic give-ups (`ErrMaxRounds`) are surfaced as
  *terminal* errors so Restate never retries them forever; session state is persisted
  only on success.
- **Determinism:** `guest.start` re-runs the program verbatim, so its clock and
  randomness are frozen in the JS prelude (`Date` constructor, `Date.now`, `Math.random`,
  `crypto`, `performance.now`); the WASI clock/rand are pinned to constants as a
  backstop. One seed is minted per program (from `restate.Rand`) and the clock is
  captured once in a journaled step, so a program's operation sequence — and thus its
  deterministic handles — are identical across replays.
- **Pooling:** guest instances are reused (EXCLUSIVE checkout — one program per instance
  at a time), removing per-round allocation churn. A fresh `start` drops and recreates
  the QuickJS runtime, so nothing leaks between programs. An instance is retired after
  enough runs, if it grows past a memory high-water mark, or if a run trapped.
  `Engine.Close` drains in-flight runs before shutting the runtime down.
- **Safety:** a 256 MiB memory cap (wazero + the guest's own
  `set_memory_limit`/`set_max_stack_size`); the sole backstop against a runaway
  tool-calling loop is `maxProgramSteps` (each step settles one op, so this caps total
  ops — parallel width and sequential depth both count). Malformed model output and
  ordinary tool errors (rejected promises) are fed back as observations rather than being
  fatal; an unsubmittable op (unknown tool) and a Restate cancellation are fatal instead —
  the cancellation is *returned* (never swallowed as a per-tool failure), while a genuine
  guest trap propagates as a panic.

The design was adversarially reviewed; [`CLAUDE.md`](./CLAUDE.md) records the invariants
and known limitations.

## License

[MIT](./LICENSE).
