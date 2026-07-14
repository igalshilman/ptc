# DESIGN.md — the tool abstraction

Why the tool API is shaped the way it is. Read `CLAUDE.md` first for the mechanics;
this is the rationale, so a future change doesn't accidentally undo a hard-won
invariant.

## The problem

The model writes JS programs; the JS calls developer "tools" as plain async
functions. A program can fire several tools concurrently:

```js
const [a, b] = await Promise.all([toolA(x), toolB(y)]);
```

Both `__hostCall`s land in `globalThis.__outbox` as one step's worth of ops before Go
runs either (this is just microtask-quiescence, not special to `Promise.all` — `const
p=a(); const q=b(); await p; await q;` batches identically). So a step already encodes
the model's concurrency intent. The question is only: *how does Go execute those ops,
durably and replay-safely, while still supporting first-completion (`Promise.race`,
timeouts)?*

## The load-bearing constraint

A Restate invocation is **one thread of control over one strictly-ordered journal.**
Concurrency inside it is cooperative: you **submit** futures (each reserves a journal
slot, non-blocking) and then **one driver** — `restate.WaitFirst` — advances them all
and suspends the single thread until the first settles.

- **Submitting composes.** A single op = one submission = one future → it drops
  straight into the shared driver. N submissions, one `WaitFirst` → real parallelism.
- **Driving blocks.** A data-dependent sequence (`B(a)` where `a = await A()`) cannot
  be pre-submitted — `B`'s future doesn't exist until `A` has *resolved*, and
  resolving means *driving the journal*, which blocks the one thread.
- You **cannot** have two things driving one journal at once (goroutines would race
  the journal → replay diverges; Restate forbids it). And if one tool blocks-to-drive
  during a step, it monopolizes the thread → the step's other ops serialize.

So: **a single op runs in-process; a multi-step operation needs its own thread-of-control.**
Restate's unit of an independent thread-of-control + journal is *the invocation*.

## What falls out: one tool kind, plus "a handler is a tool"

A tool is exactly **one durable op**. `agent.NewTool[A, R]` takes a body that performs
ONE non-blocking submission and returns the resulting `Future[R]`:

| the op | helper | future from |
|---|---|---|
| side effect (HTTP, DB, compute) | `agent.Run` | `RunAsync` |
| call another service | `agent.Call` | `Service[R].RequestFuture` |
| call a keyed VO / Workflow handler | `agent.CallObject` | `Object[R].RequestFuture` |
| durable timer | `agent.Timer` | `After` |
| external completion (system id) | `agent.Awakeable` | `Awakeable[R]` |
| external completion (named) | `agent.Signal` | `Signal[R]` |

The framework holds the `Future`'s internals (its fields are unexported): a tool
**cannot fabricate a future that isn't backed by a real durable submission**, and its
body must NOT block before returning it (blocking would monopolize the thread and
serialize its siblings). Arg and result JSON Schemas are reflected from `A`/`R` and
surfaced to the model.

**A multi-step, blocking operation is NOT a special tool kind.** (An earlier design had
`NewSeqTool` — a blocking handler that ran in its own `AgentTools/Exec` sub-invocation;
it's gone.) The insight that replaced it: Restate already has a first-class unit for
"an independent thread-of-control that may block and branch freely" — **a handler.** So
model a multi-step operation as an ordinary Restate handler, and expose it to the agent
one of two ways:

1. a **leaf tool that `agent.Call`s it** — the call is a non-blocking future, and the
   handler blocks/orchestrates inside its own invocation; or
2. **annotate the handler** with `AgentToolAnnotation` (`"restate/agent"`) so
   **discovery** builds the leaf tool for you (see `discover.go`): the tool `Call`s /
   `CallObject`s the handler, wrapping a keyed handler's args as `{key, input}`.

Either way it collapses to the same thing the batch driver already understands — a
service-call future — so there is genuinely **one tool kind**, and multi-step work
parallelizes with sibling tools at the cost of one invocation hop. Such handlers are
**session-stateless** (pass state via args) and must **not** call back into their own
`Agent/<key>` (the parent holds that lock while awaiting them → deadlock; discovery
always skips the `Agent` object).

## The drive loop: submit a step, race, settle one, repeat

`restateInvoker` (service.go) is one uniform driver behind the live-coroutine engine
(`RunLive` in engine.go):

- **`Start(ops)`** — submit every op the guest emitted this step as an in-flight future
  (leaf in-process; a discovered/handler tool via `RequestFuture`). Non-blocking:
  reserves a journal slot each. An op that can't even be submitted (unknown tool / bad
  args) is a **fatal** condition — it panics, aborting the whole program rather than
  being demoted to a per-op rejection the JS could swallow.
- **`Next()`** — drive the whole in-flight set with ONE `restate.WaitFirst` and return
  the **first** to settle. A `WaitFirst` cancellation is returned as a fatal error (not
  fed back to the guest); if nothing is in flight yet the program isn't done — a
  JS-level deadlock — `Next` returns a clean terminal error rather than letting the
  empty `WaitFirst` panic.

The host settles that one completion back into the guest (`resolve`/`reject`), which
resumes the program to its next quiescence, emitting the next step's ops. Contrast with
the earlier *re-execution* engine, which submitted a whole frontier, drove it with a
single `restate.Wait`, and read results **by index** — correct for `Promise.all` but it
could not express `Promise.race`, because every op had to settle before the program
advanced. First-completion settlement is the whole reason the guest became a live
coroutine.

## The concurrency contract (what the model gets)

| JS the model wrote | ops per step | execution |
|---|---|---|
| `Promise.all([a(x), b(y)])` | 2 in one step | both submitted before any settles → **parallel** |
| `const p=a(x); const q=b(y); await p; await q;` | 2 in one step | identical → **parallel** |
| `Promise.race([a(x), b(y)])` | 2 in one step | first completion resumes the program; loser left in flight → **race** |
| `await a(x); await b(y);` | 1, then 1 | `b` emitted only after `a` settles → **serial** |
| `const r = await a(x); b(r);` | 1, then 1 | data-dependent → **serial** |

Replay-safe because: `guest.start` re-runs the pure JS program (same ops, same
deterministic handles on replay); each submission reserves a journal slot in
deterministic loop order; and settling the winner drives the program identically. A
`Promise.race` loser is simply abandoned — its durable future is left in flight (no
cleanup, by design), and `Reset` clears the host's leftover handle state before the next
program.

The one subtle case is which op **wins a `Promise.race`** when two or more futures are
*already complete at the same `WaitFirst` poll* (which can happen on replay, where the
runtime front-loads journaled completions). `restate.WaitFirst` breaks that tie by the
order the futures are passed to it — so `restateInvoker.Next` passes them in
**ascending-handle order** (`sort.Ints`), and since handles are deterministic per
program the winner is reproduced across replays. This relies on the SDK's *ordered*
`WaitIterator` (a slice, not a map): earlier sdk-go (≤ v1.0.0) tie-broke by Go
map-iteration order, so the winner was not replay-stable and sorting `sels` didn't help
(the iterator re-copied into its own map); it is fixed as of
`sdk-go v1.0.1`, which `go.mod` pins. `Promise.all` never
depended on this (order-independent).

## Why not the alternatives (considered, rejected)

- **Keep a seq-tool kind (`NewSeqTool` → its own sub-invocation).** Two tool
  constructors the model can't tell apart, a bespoke `AgentTools/Exec` transport
  handler, and the framework owning something Restate already models perfectly: a
  handler. Folding "multi-step tool" into "a handler you call (or auto-discover)"
  deletes the second constructor and the transport, and reuses the ordinary
  service-call future the driver already handles.
- **Keep `NewTool`/`NewRunTool` (context-serial vs run-parallel).** Leaks Restate
  plumbing (`Context` vs `RunContext`) as the tool-author's choice; `Promise.all` of
  two context-tools silently serialized. One leaf kind whose future can only come from
  a non-blocking helper removes the footgun.
- **"A tool returns a `restate.Future`" as the author-facing signature.** Right
  instinct — everything durable in sdk-go *is* a `Future` — but a future must come from
  a *non-blocking* submission, so any multi-step tool would have to block (silently
  serializing the step) or hand-roll a sub-invocation. So the framework holds the
  future; the leaf author returns one only via helpers, and multi-step work lives in a
  handler.
- **Author-side combinators (`Wait`/`Select` inside a tool).** sdk-go's combinators
  *drive* (block); there is no lazy `All`/`join` that returns an unresolved composite.
  So the combinator lives where it can afford to drive: the framework (one `WaitFirst`
  over the step) and the JS (`Promise.all`/`Promise.race`). Fan-out/merge that must be
  atomic → a handler.
- **Everything is a sub-invocation (all tools as handlers).** Uniform and truly
  parallel, but pays an invocation hop for *every* leaf call, makes all tools
  session-stateless, and risks same-session reentrancy deadlock. We pay that cost only
  for the multi-step operations that actually need it.
- **A turnstile (linearize the batch in-process, à la sdk-python).** Correct and
  simple, but gives up wall-clock parallelism. Go's `RunAsync`/`RequestFuture` reserve
  journal slots at submission, so we get *true* parallelism in-process for leaves — no
  turnstile needed (nothing drives the journal concurrently).

## Backlog

- **Durable handles across rounds.** A leaf tool that returns an awakeable/signal id and
  is awaited in a *later* agent round (the loop would need to serialize/rehydrate the
  handle). Today a signal is awaited within the same program that opens it. Separable
  feature; not the core tool abstraction.
- **Structured tool errors.** Deliver `{code,message,terminal}` to JS via a prelude
  `.catch` (no guest change needed — the bridge already rejects a promise with the error
  string as `err.message`).
