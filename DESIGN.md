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

Both `__hostCall`s become pending in **one batch** before Go runs either (this is
just microtask-quiescence, not special to `Promise.all` — `const p=a(); const q=b();
await p; await q;` batches identically). So the batch already encodes the model's
concurrency intent. The question is only: *how does Go execute a batch, durably and
replay-safely?*

## The load-bearing constraint

A Restate invocation is **one thread of control over one strictly-ordered journal.**
Concurrency inside it is cooperative: you **submit** futures (each reserves a journal
slot, non-blocking) and then **one driver** — `restate.Wait` — advances them all and
suspends the single thread until they settle.

- **Submitting composes.** A single op = one submission = one future → it drops
  straight into the shared driver. N submissions, one `Wait` → real parallelism.
- **Driving blocks.** A data-dependent sequence (`B(a)` where `a = await A()`) cannot
  be pre-submitted — `B`'s future doesn't exist until `A` has *resolved*, and
  resolving means *driving the journal*, which blocks the one thread.
- You **cannot** have two things driving one journal at once (goroutines would race
  the journal → replay diverges; Restate forbids it). And if one tool blocks-to-drive
  during the batch, it monopolizes the thread → the batch serializes.

So: **a single op runs in-process; a sequence needs its own thread-of-control.**
Restate's unit of an independent thread-of-control + journal is *the invocation*.

## The two shapes that fall out

Both are surfaced to the model identically (async JS functions with arg + result
schemas); they differ only in **how their future is minted**, and the batch driver
treats every future the same.

| | mints its future via | cost | may block/orchestrate |
|---|---|---|---|
| **leaf** `NewTool` → `Future[R]` | in-process `Run`/`Call`/`CallObject`/`Timer`/`Awakeable` | cheap | ❌ one submission only |
| **seq** `NewSeqTool` → `(R,error)` | `RequestFuture` → `AgentTools/Exec` | one invocation hop | ✅ its own journal |

`InvokeBatch` is one uniform driver: submit every call (leaf in-process, seq via
`RequestFuture`) collecting an `anyFuture` each → one `restate.Wait` → read by index.
A seq tool's sub-invocation is *also* just a `ResponseFuture` in that same `Wait`.

## The concurrency contract (what the model gets)

| JS the model wrote | batches | execution |
|---|---|---|
| `Promise.all([a(x), b(y)])` | 1 | both submitted before any await → **parallel** |
| `const p=a(x); const q=b(y); await p; await q;` | 1 | identical → **parallel** |
| `await a(x); await b(y);` | 2 | `b` submitted only after `a` settles → **serial** |
| `const r = await a(x); b(r);` | 2 | data-dependent → **serial** |

Replay-safe because: the JS program is pure recompute (same batch, same order on
replay); each submission reserves a journal slot in deterministic loop order; `Wait`
yields in completion order but we **discard** that and read results by index.

## Why not the alternatives (considered, rejected)

- **Keep `NewTool`/`NewRunTool` (context-serial vs run-parallel).** Leaks Restate
  plumbing (`Context` vs `RunContext`) as the tool-author's choice; `Promise.all` of
  two context-tools silently serialized. The leaf/seq split is *semantic* ("one op vs
  a sequence"), which an author can answer correctly without knowing Restate.
- **"A tool returns a `restate.Future`" as the author-facing signature.** Right
  instinct — everything durable in sdk-go *is* a `Future` — but it's a footgun: a
  future must come from a *non-blocking* submission, so any multi-step tool would have
  to block (silently serializing the batch) or hand-roll a sub-invocation. So the
  framework holds the future; the leaf author returns one only via helpers, and the
  seq author writes plain blocking Go.
- **Author-side combinators (`Wait`/`Select` inside a tool).** sdk-go's combinators
  *drive* (block); there is no lazy `All`/`join` that returns an unresolved composite.
  So the combinator lives where it can afford to drive: the framework (one `Wait` over
  the batch) and the JS (`Promise.all`). Fan-out/merge that must be atomic → a seq tool.
- **Everything is a sub-invocation (all tools as handlers).** Uniform and truly
  parallel, but pays an invocation hop for *every* leaf call, makes all tools
  session-stateless, and risks same-session reentrancy deadlock. We pay that cost only
  for seq tools that actually need it.
- **A turnstile (linearize the batch in-process, à la sdk-python).** Correct and
  simple, but gives up wall-clock parallelism. Go's `RunAsync`/`RequestFuture` reserve
  journal slots at submission, so we get *true* parallelism in-process for leaves — no
  turnstile needed (nothing drives the journal concurrently).

## Backlog

- **Durable handles across rounds.** A leaf tool that returns an awakeable id and is
  awaited in a *later* agent round (the loop would serialize/rehydrate the handle).
  Separable feature; not the core tool abstraction.
- **Structured tool errors.** Deliver `{code,message,terminal}` to JS via a prelude
  `.catch` (no guest.c change needed — `resolve_handle` already sets the string as
  `err.message`).
