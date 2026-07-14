# Programmatic Tool Calling with Restate

A durable CodeAct agent built on [Restate](https://restate.dev). On each round,
the model writes a small JavaScript program. That program runs in an embedded
QuickJS runtime and calls Go tools or discovered Restate handlers as ordinary
async functions.

Model calls and tool calls are journaled by Restate. If the process crashes, the
agent replays the JavaScript while reusing the recorded results instead of calling
the model or repeating side effects.

The project provides:

- durable, keyed conversation sessions;
- parallel tools through `Promise.all` and first-completion control flow through
  `Promise.race`;
- typed Go tools with reflected argument and result schemas;
- language-agnostic tool discovery through Restate handler metadata;
- a Rust/QuickJS WASM guest embedded in a pure-Go binary, with no cgo.

> **Project status:** the current implementation passes the offline test suite,
> race detector, `go vet`, and `go build`. An earlier architecture was exercised
> end to end with Restate and OpenAI; the current live-coroutine and discovery path
> has not yet been re-verified end to end. See [Current limits](#current-limits).

## Quick start

### Prerequisites

- Go 1.25 or newer
- an OpenAI API key, or an OpenAI-compatible endpoint
- a Restate Cloud environment (the examples connect to it through a tunnel)

The committed QuickJS WASM guest means normal builds and tests need only Go:

```bash
go build ./...
go test ./...
```

To run the examples, deploy both through the tunnel. Set the tunnel environment
(see [Deploying through a tunnel](#deploying-through-a-tunnel)), then start them —
`make run` starts the back-office, then the agent:

```bash
OPENAI_API_KEY=sk-... make run
```

Each example connects **out** to Restate Cloud and registers itself, so there is no URL
to register and no local port. Invoke the agent through your environment's Restate ingress
(see the Restate Cloud docs for the ingress URL and auth). The `Agent` virtual object,
keyed by session id, exposes three handlers:

```text
POST  <ingress>/Agent/<session>/Ask       {"message":"fulfill order #42: 3x SKU-1, 1x SKU-9, total $1200"}
POST  <ingress>/Agent/<session>/History
POST  <ingress>/Agent/<session>/Reset
```

Both deployments must be connected. Without the back-office, the agent still runs but
cannot discover the example Inventory, RiskCheck, or Payments handlers.

### Development shell

The repository includes a Nix shell with Go, Rust, and the `wasm32-wasip1`
toolchain:

```bash
nix develop
```

Rust is needed only when changing the guest. Rebuild the committed artifact with:

```bash
make guest-rs
```

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `OPENAI_API_KEY` | required | OpenAI credential; use `dummy` for a keyless local endpoint |
| `OPENAI_BASE_URL` | OpenAI | OpenAI-compatible API endpoint |
| `AGENT_MODEL` | `gpt-5` | model used by the orchestrator |
| `RESTATE_ADMIN_URL` | Admin API | Restate Admin API used for handler discovery |

Both deployments connect to Restate Cloud through an outbound tunnel; that needs a few
more variables — see [Deploying through a tunnel](#deploying-through-a-tunnel).

## How one turn runs

The agent is a Restate Virtual Object named `Agent`. Each object key is an
independent session whose transcript is stored as object state.

```text
Agent/<session>/Ask
  |
  +-- model.Decide             Restate Run -> {thought, code}
  |
  +-- Sandbox.RunProgram       execute code once in QuickJS
        |
        +-- tool calls         submit durable Restate futures
        +-- first completion   settle one JS promise
        +-- repeat             until the program returns
  |
  +-- {done:true, answer}      persist transcript and finish
      any other result         observation for the next model round
```

The Go agent loop itself is not wrapped in `restate.Run`. Durability lives at the
boundaries: the model call and each tool operation. JavaScript execution is pure,
deterministic recomputation during replay.

A generated program looks like this:

```js
const [stock, risk] = await Promise.all([
  reserve_stock({ key: "SKU-1", input: { qty: 3 } }),
  risk_score({ customer: "c-17", amount: 1200 }),
]);

if (!stock.ok) {
  return { done: true, answer: "Not enough stock" };
}

return { done: false, stock, risk };
```

The returned observation is shown to the model on the next round. A program ends
the turn by returning `{done: true, answer: ...}`.

## Tools

There is one tool abstraction: a tool submits one durable operation and returns
its unresolved `agent.Future[R]`.

```go
type chargeArgs struct {
    Customer string  `json:"customer"`
    Amount   float64 `json:"amount"`
}

type chargeResult struct {
    TxnID string `json:"txn_id"`
}

func chargeTool() agent.Tool {
    return agent.NewTool(
        "charge_payment",
        "Charge a customer",
        func(ctx restate.Context, in chargeArgs) (agent.Future[chargeResult], error) {
            return agent.Call[chargeResult](ctx, "Payments", "charge", in), nil
        },
    )
}
```

Argument and result JSON Schemas are reflected from the Go types and included in
the model's system prompt.

| Operation | Helper |
|---|---|
| journaled side effect such as HTTP, DB, or compute | `agent.Run` |
| call a Restate service | `agent.Call` |
| call a keyed Virtual Object or Workflow | `agent.CallObject` |
| durable timer | `agent.Timer` |
| external completion with a generated id | `agent.Awakeable` |
| external completion with a chosen name | `agent.Signal` |

A tool body must return without waiting. If an operation needs several durable
steps or data-dependent branching, implement it as a normal Restate handler and
have the tool call that handler. This gives the operation its own invocation and
journal. [DESIGN.md](./DESIGN.md) explains the constraint in detail.

### Concurrency semantics

| JavaScript | Behavior |
|---|---|
| `await a(); await b();` | serial |
| `await Promise.all([a(), b()])` | both operations are submitted before either is awaited |
| `await Promise.race([a(), b()])` | the first settled promise resumes the program |

Race ties are deterministic: pending futures are passed to Restate in ascending
JS-handle order, and the pinned SDK preserves that order when several futures are
already complete.

A `Promise.race` loser is not cancelled. Its durable operation may continue after
the JavaScript program has moved on, so a timeout means "stop waiting", not "undo
or cancel the work".

## Handler discovery

Any Restate handler can opt into discovery with metadata named `restate/agent`.
The metadata value becomes the tool name. The agent reads the handler's input and
output schemas from the Restate Admin API and builds a callable tool for the
current turn.

In Go:

```go
restate.NewServiceHandler(
    score,
    restate.WithMetadata(agent.AgentToolAnnotation, "risk_score"),
)
```

Discovery is language-agnostic. The same tool can be implemented in TypeScript:

```ts
import * as restate from "@restatedev/restate-sdk";

const riskCheck = restate.service({
  name: "RiskCheck",
  handlers: {
    score: restate.handlers.handler(
      { metadata: { "restate/agent": "risk_score" } },
      async (_ctx: restate.Context, input: { customer: string; amount: number }) => {
        const flagged = input.amount >= 1000;
        return {
          score: Math.floor(input.amount / 20),
          flagged,
          reason: flagged ? "needs human approval" : "within limit",
        };
      }
    ),
  },
});

restate.endpoint().bind(riskCheck).listen(9081);
```

Register that deployment with the same Restate runtime and it appears as
`risk_score`; no Go wiring is required. Add input and output serde, for example
with `@restatedev/restate-sdk-zod`, if you want discovery to provide JSON Schemas
to the model.

Keyed handlers are exposed with arguments shaped as
`{"key": "object-key", "input": <handler input>}`. Discovery runs inside a
journaled step, so a replay sees the same tool set as the original invocation.

## QuickJS execution

The guest under [`guest-rs/`](./guest-rs) uses Rust and `rquickjs`, compiled to
`wasm32-wasip1` and embedded as [`agent/quickjs_guest.wasm`](./agent/quickjs_guest.wasm).
It has no host imports. The Go host drives it through three exports:

| Export | Purpose |
|---|---|
| `start(script)` | create a fresh QuickJS context and run to quiescence |
| `resolve(handle, value)` | resolve one pending tool promise and continue |
| `reject(handle, message)` | reject one pending tool promise and continue |

Each call returns one JSON step:

| Step | Meaning |
|---|---|
| `{"s":0,"r":...}` | program returned |
| `{"s":1,"ops":[...]}` | program is waiting and emitted tool operations |
| `{"s":2,"error":"..."}` | program failed |

The injected JavaScript bridge turns each tool into a promise, assigns it a
deterministic handle, and writes the operation to an outbox. Go submits the
outbox, waits for one durable future, then calls `resolve` or `reject` for that
handle. This preserves normal `await`, `Promise.all`, and `Promise.race` behavior
without giving the WASM guest direct access to Restate.

Guest instances are pooled, but each `start` creates a fresh QuickJS runtime and
context, so JavaScript globals do not leak between programs. See
[`CLAUDE.md`](./CLAUDE.md) for the complete ABI, pooling rules, and replay
invariants.

## Safety and replay

- Model responses and tool results are journaled; JavaScript is re-executed with
  those recorded results after a crash.
- `Date`, `Math.random`, `crypto`, and `performance.now` are replaced with
  replay-stable values for each program.
- QuickJS has deterministic compute, memory, and stack limits. Infinite loops are
  interrupted and returned to the model as program errors.
- Non-JSON-serializable return values are reported as recoverable program errors.
- Instances are checked out exclusively and retired after enough runs, excessive
  memory growth, or a trap.
- Session state is updated only after a successful agent turn.

## Current limits

- A single emitted tool frontier is not yet capped before submission. The
  settlement loop has a step limit, but an oversized `Promise.all` can submit many
  durable operations before that limit is reached.
- `Promise.race` does not cancel losing operations.
- Tool handles cannot be carried from one model round into a later round.
- The full live-coroutine and discovery path still needs a current end-to-end test
  against a real Restate runtime and model endpoint.
- The example Inventory, RiskCheck, and Payments services use toy business logic.

## Repository guide

- [`agent/`](./agent): public Go API, durable service, loop, sandbox, and engine
- [`guest-rs/`](./guest-rs): QuickJS WASM guest
- [`examples/orchestrator/`](./examples/orchestrator): order-fulfillment agent
- [`examples/backoffice/`](./examples/backoffice): discovered Restate handlers
- [`DESIGN.md`](./DESIGN.md): tool and concurrency rationale
- [`CLAUDE.md`](./CLAUDE.md): detailed code map, invariants, and implementation status

## Deploying through a tunnel

Each deployment connects **outbound** to Restate Cloud through
[`github.com/restatedev/sdk-go/x/tunnel`](https://github.com/restatedev/sdk-go/releases/tag/x/tunnel/v0.1.0),
so it needs no inbound listener or public URL. `agent.Deploy(ctx, srv, tunnelName)` sets
only the tunnel **name** from its argument (the examples pass `agent` and `backoffice`, so
the two can be co-deployed without colliding); the tunnel reads everything else from the
environment (the variables the Restate operator injects for in-process tunnel mode).

Set the tunnel environment, then run either example:

```bash
export RESTATE_TUNNEL=1
export RESTATE_INPROC_ENVIRONMENT_ID=env_...              # Restate Cloud environment id
export RESTATE_AUTH_TOKEN=...                             # Restate Cloud API token
export RESTATE_INPROC_SIGNING_PUBLIC_KEY=publickeyv1_...  # environment signing public key
export RESTATE_TUNNEL_SERVERS_SRV=tunnel.us.restate.cloud # regional tunnel servers (SRV)

OPENAI_API_KEY=sk-... go run ./examples/orchestrator     # tunnel name: agent
OPENAI_API_KEY=sk-... go run ./examples/backoffice       # tunnel name: backoffice
```

| Variable | Purpose |
|---|---|
| `RESTATE_TUNNEL` | selects in-process tunnel mode |
| `RESTATE_INPROC_ENVIRONMENT_ID` | Restate Cloud environment id (`env_…`) |
| `RESTATE_AUTH_TOKEN` | Restate Cloud API token (or `RESTATE_INPROC_AUTH_TOKEN_FILE` to read it from a file) |
| `RESTATE_INPROC_SIGNING_PUBLIC_KEY` | environment signing public key (`publickeyv1_…`) |
| `RESTATE_TUNNEL_SERVERS_SRV` | regional tunnel servers, e.g. `tunnel.us.restate.cloud` (or `RESTATE_INPROC_CLOUD_REGION`) |
| `RESTATE_INPROC_TUNNEL_NAME` | fallback tunnel name — used only when a deployment passes an empty name; the examples set theirs in code |

Values come from your Restate Cloud environment. `x/tunnel` is a preview (0.x) module; its
API and variables may change in a minor release.

## License

[MIT](./LICENSE)
