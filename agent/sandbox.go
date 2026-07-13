package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// ToolSpec describes a tool to the model: its name, a human description, and the
// JSON Schemas for its single argument object (Params) and its return value
// (Result). Either schema may be nil.
type ToolSpec struct {
	Name        string
	Description string
	Params      json.RawMessage
	Result      json.RawMessage
}

// Invoker is the durable backend behind the JS tools. As the program runs it starts
// operations (tool calls) the host completes durably; each op carries a stable
// handle. Implementations:
//
//   - test doubles (agent_test.go): in-memory Go funcs, for the offline tests.
//   - restateInvoker (service.go): submits each op as a durable Future and races them
//     with restate.WaitFirst. Journaled and replay-safe; none of that is visible to
//     the JS. (A multi-step operation is an ordinary handler a leaf tool Calls, so it
//     too is just one of these futures.)
type Invoker interface {
	// Tools returns the registered tool specs, in a stable order. Names drive the
	// JS bridge; the full specs (incl. JSON Schema) are surfaced to the model.
	Tools() []ToolSpec
	// Reset discards any in-flight/leftover ops from a previous program. The guest
	// resets its handle counter to 0 on each start(), so the host must clear its
	// handle-keyed state too — otherwise a prior program's abandoned op (e.g. a
	// Promise.race loser) would alias a later program's reused handle.
	Reset()
	// Start submits new ops as in-flight durable operations, keyed by ToolCall.Handle.
	// Non-blocking.
	Start(calls []ToolCall)
	// Next drives the in-flight ops until the FIRST one settles and returns it (a
	// StepResult whose Err is set iff that op failed). A non-nil second return is
	// invocation-FATAL (e.g. a Restate cancellation), distinct from a per-op failure.
	Next() (StepResult, error)
}

// Sandbox runs a single JS program with the registered tools exposed as plain async
// JS functions. The agent LOOP lives in Go (see loop.go): each round the model
// generates a program and the loop runs it here as a live coroutine (see engine.go).
//
// Determinism: on crash/replay the guest re-runs the program from the top and the
// host feeds the journaled completions back in the same order, so randomness and the
// clock must be replay-stable. seed and nowMillis (set via SetDeterminism from
// Restate's deterministic sources in production) freeze Math.random/Date.now/new
// Date(); callSeq gives each program in a run a distinct but replay-stable sub-seed.
type Sandbox struct {
	engine    *Engine
	inv       Invoker
	seed      int64
	nowMillis int64
	callSeq   int
}

// NewSandbox binds an engine to an invoker.
func NewSandbox(engine *Engine, inv Invoker) *Sandbox { return &Sandbox{engine: engine, inv: inv} }

// SetDeterminism sets a replay-stable RNG seed and a frozen wall clock (in ms).
// In production these come from restate.Rand(ctx) and a once-captured clock, so
// they are identical across replays; the demo/tests use fixed defaults.
func (s *Sandbox) SetDeterminism(seed, nowMillis int64) {
	s.seed = seed
	s.nowMillis = nowMillis
}

// Tools exposes the registered tool specs (used to build the model prompt).
func (s *Sandbox) Tools() []ToolSpec { return s.inv.Tools() }

// RunProgram evaluates the model-generated program to completion, returning its
// `return` value encoded as JSON. A thrown JS error or a rejected tool promise
// surfaces as a non-nil error (which the loop feeds back to the model).
//
// The program runs as a LIVE coroutine (see engine.RunLive): it is started once and
// driven by the host, which settles each tool-call promise as its durable operation
// completes. Only the tool operations are durable; running the JS is pure.
func (s *Sandbox) RunProgram(ctx context.Context, program string) (string, error) {
	seq := s.callSeq
	s.callSeq++
	// ONE seed per program (reused if it re-runs on replay), so its clock, randomness,
	// and thus its operation sequence are identical across replays.
	progSeed := int64(uint64(s.seed) ^ (uint64(seq)+1)*0x9e3779b97f4a7c15)

	script := s.determinismPrelude(progSeed) + s.toolBridge() + wrapperPreJS + program + wrapperPostJS
	return s.engine.RunLive(ctx, script, s.inv)
}

// The injected JS lives in these raw-string constants — real JS, not escaped Go
// fragments — so it reads and reviews as JS and is decoupled from the assembly
// logic. Values (seed, clock, tool names, program) are substituted at assembly time
// (see RunProgram): none of the templates contain a literal '%'.

// detPreludeJS freezes clock/randomness at the JS level (so it survives instance
// reuse — the WASI clock/rand are bound at instantiation). %d/%d = seed, now.
// Overrides Math.random (seeded LCG); the Date CONSTRUCTOR — not just Date.now,
// since `new Date()` reads the host clock — so no-arg construction/`Date()` return
// the frozen now while explicit-arg construction delegates to the real Date; and
// crypto.getRandomValues / performance.now if present.
const detPreludeJS = `;(function () {
  var __s = %d >>> 0;
  Math.random = function () { __s = (Math.imul(__s, 1664525) + 1013904223) >>> 0; return __s / 4294967296; };
  var __now = %d;
  var RealDate = Date;
  function FakeDate() {
    if (!(this instanceof FakeDate)) return new RealDate(__now).toString();
    return arguments.length ? new RealDate(...arguments) : new RealDate(__now);
  }
  FakeDate.prototype = RealDate.prototype;
  try { RealDate.prototype.constructor = FakeDate; } catch (e) {}
  FakeDate.now = function () { return __now; };
  FakeDate.parse = RealDate.parse;
  FakeDate.UTC = RealDate.UTC;
  globalThis.Date = FakeDate;
  try { if (globalThis.crypto && globalThis.crypto.getRandomValues) {
    globalThis.crypto.getRandomValues = function (a) { for (var i = 0; i < a.length; i++) a[i] = (Math.random() * 256) | 0; return a; };
  } } catch (e) {}
  try { if (globalThis.performance && globalThis.performance.now) {
    globalThis.performance.now = function () { return 0; };
  } } catch (e) {}
})();
`

// bridgeJS is the live host-call bridge. Each tool call gets a deterministic integer
// handle, stashes its promise's {res, rej} in globalThis.__pending, records
// {handle,name,arg} in globalThis.__outbox (the host drains it each step), and returns
// the promise. The host settles a promise later via __resolveJSON(handle, jsonText) or
// __reject(handle, msg). Each name in globalThis.__toolNames becomes a plain async
// function over __hostCall; __toolNames is injected just before this runs.
const bridgeJS = `;(function () {
  globalThis.__pending = globalThis.__pending || {};
  globalThis.__outbox = globalThis.__outbox || [];
  if (globalThis.__nextHandle === undefined) globalThis.__nextHandle = 0;
  globalThis.__hostCall = function (name, arg) {
    var handle = globalThis.__nextHandle++;
    var p = new Promise(function (res, rej) { globalThis.__pending[handle] = { res: res, rej: rej }; });
    globalThis.__outbox.push({ handle: handle, name: name, arg: arg === undefined ? null : arg });
    return p;
  };
  globalThis.__resolveJSON = function (handle, jsonText) {
    var e = globalThis.__pending[handle];
    if (e) { delete globalThis.__pending[handle]; e.res(JSON.parse(jsonText)); }
  };
  globalThis.__reject = function (handle, msg) {
    var e = globalThis.__pending[handle];
    if (e) { delete globalThis.__pending[handle]; e.rej(new Error(msg)); }
  };
  var tools = {};
  (globalThis.__toolNames || []).forEach(function (n) {
    var f = function (arg) { return globalThis.__hostCall(n, arg === undefined ? null : arg); };
    tools[n] = f; globalThis[n] = f;
  });
  globalThis.tools = tools;
})();
`

// The program is wrapped in an async IIFE; on settle it publishes the output blob
// the guest reads — its return value ({s:0}) or a thrown error ({s:2}).
const (
	wrapperPreJS  = ";(async function () {\n"
	wrapperPostJS = "\n})().then(" +
		"function (r) { globalThis.__output = JSON.stringify({ s: 0, r: r === undefined ? null : r }); }, " +
		"function (e) { globalThis.__output = JSON.stringify({ s: 2, error: (e && e.message !== undefined) ? String(e.message) : String(e) }); });\n"
)

// determinismPrelude fills detPreludeJS with this run's seed and frozen clock.
func (s *Sandbox) determinismPrelude(seed int64) string {
	return fmt.Sprintf(detPreludeJS, uint32(seed), s.nowMillis)
}

// toolBridge injects this run's tool names, then the (static) live host-call bridge
// (bridgeJS).
func (s *Sandbox) toolBridge() string {
	specs := s.inv.Tools()
	names := make([]string, len(specs))
	for i, sp := range specs {
		names[i] = sp.Name
	}
	namesJSON, _ := json.Marshal(names)
	return "globalThis.__toolNames = " + string(namesJSON) + ";\n" + bridgeJS
}

// ToolCall is one operation the program started: a stable, deterministic handle plus
// the tool name and its JSON argument. It is also the wire shape the guest emits (see
// guestStep.Ops), so it unmarshals directly from the guest's step blob.
type ToolCall struct {
	Handle int             `json:"handle"`
	Tool   string          `json:"name"`
	Arg    json.RawMessage `json:"arg"`
}

// StepResult is the settlement of one operation that the Invoker's Next returns: the
// handle it belongs to and either a JSON value (Value) or, when the op failed, an Err
// (delivered to JS as a rejected promise).
type StepResult struct {
	Handle int
	Value  json.RawMessage // valid JSON on success (empty → treated as null)
	Err    error           // non-nil iff the op failed
}
