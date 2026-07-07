package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
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

// Invoker is the durable backend behind the JS tools. When a program running in
// the sandbox calls a tool (a plain async JS function), the call arrives here as
// (tool, arg) and the returned JSON becomes the JS promise's resolved value; a
// non-nil error becomes a rejected promise. Implementations:
//
//   - test doubles (agent_test.go): in-memory Go funcs, for the offline tests.
//   - restateInvoker (service.go): submits each call as a durable Future — leaf
//     tools in-process, seq tools as their own sub-invocation — and drives the
//     whole batch with one restate.Wait. Journaled and replay-safe; none of that
//     is visible to the JS program.
type Invoker interface {
	// Tools returns the registered tool specs, in a stable order. Names drive the
	// JS prelude; the full specs (incl. JSON Schema) are surfaced to the model.
	Tools() []ToolSpec
	// InvokeBatch runs one re-execution round's frontier of tool calls (always a
	// batch — size 1 for a sequential await, more for a Promise.all) and returns
	// their results in order. A result's non-nil Err becomes a rejected JS promise
	// on the next round. Implementations run the batch durably and, where possible,
	// in parallel (see restateInvoker).
	InvokeBatch(ctx context.Context, calls []ToolCall) []ToolResult
}

// Sandbox runs a single JS program with the registered tools exposed as plain
// async JS functions. The agent LOOP lives in Go (see loop.go): each round the
// model generates a program and the loop runs it here. A fresh QuickJS instance
// is used per program, so programs share no JS state across rounds — state flows
// back to the model as observations.
//
// Determinism: because a program is re-run verbatim on crash/replay, its
// randomness and clock must be replay-stable. seed and nowMillis (set via
// SetDeterminism from Restate's deterministic sources in production) freeze
// Math.random/Date.now/new Date(); callSeq gives each program in a run a distinct
// but replay-stable sub-seed.
type Sandbox struct {
	engine    *Engine
	inv       Invoker
	seed      int64
	nowMillis int64
	callSeq   int
	timeout   time.Duration // per-program wall-clock budget; 0 = unbounded
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

// SetProgramTimeout bounds a single program's wall-clock execution. On timeout
// the guest is interrupted and RunProgram returns an error (fed back to the
// model). 0 disables it. Note the budget spans any tool waits the program does,
// so pick a value comfortably above expected tool latency.
func (s *Sandbox) SetProgramTimeout(d time.Duration) { s.timeout = d }

// Tools exposes the registered tool specs (used to build the model prompt).
func (s *Sandbox) Tools() []ToolSpec { return s.inv.Tools() }

// RunProgram evaluates the model-generated program to completion, returning its
// `return` value encoded as JSON. A thrown JS error or a rejected tool promise
// surfaces as a non-nil error (which the loop feeds back to the model).
//
// Execution is by RE-EXECUTION (see engine.Run): the program is re-run from the top
// each round, with a journal of the tool results so far injected as JS. A journaled
// tool call returns its recorded result immediately; a new call is collected in the
// frontier and the program blocks, so the round returns the frontier for the host to
// run durably. This keeps the guest stateless — no live, suspended program.
func (s *Sandbox) RunProgram(ctx context.Context, program string) (string, error) {
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	seq := s.callSeq
	s.callSeq++
	// ONE seed per program, reused across every re-execution of it, so the program's
	// tool-call sequence is identical each round and journal-by-index matching holds.
	progSeed := int64(uint64(s.seed) ^ (uint64(seq)+1)*0x9e3779b97f4a7c15)

	det := s.determinismPrelude(progSeed)
	bridge := s.toolBridge()
	wrapper := wrapperPreJS + program + wrapperPostJS

	// assemble builds one round's script: determinism + this round's journal + the
	// tool bridge (names + __hostCall) + the wrapped program.
	assemble := func(journal []ToolResult) string {
		return det + "globalThis.__journal = " + journalJSON(journal) + ";\n" + bridge + wrapper
	}
	// resolve runs a frontier of tool calls durably (batched/parallel if supported),
	// then validates the JSON contract — naming the tool on failure so a misbehaving
	// tool yields a clear error rather than an opaque JS parse rejection on replay.
	resolve := func(ctx context.Context, calls []ToolCall) []ToolResult {
		results := s.inv.InvokeBatch(ctx, calls)
		for i := range results {
			if results[i].Err != nil {
				continue
			}
			if len(results[i].Value) == 0 {
				results[i].Value = json.RawMessage("null")
			} else if !json.Valid(results[i].Value) {
				name := "?"
				if i < len(calls) {
					name = calls[i].Tool
				}
				results[i] = ToolResult{Err: fmt.Errorf("tool %q returned invalid JSON", name)}
			}
		}
		return results
	}
	return s.engine.Run(ctx, assemble, resolve)
}

// journalJSON serializes the tool results gathered so far into the JS array the
// guest replays: each entry is {"v":<result>} on success or {"err":"<msg>"} on
// error, matched to tool calls by ORDER. A tool result that isn't valid JSON becomes
// a clear error entry (rather than an opaque JS parse failure at replay).
func journalJSON(journal []ToolResult) string {
	type entry struct {
		V   json.RawMessage `json:"v,omitempty"`
		Err *string         `json:"err,omitempty"`
	}
	entries := make([]entry, len(journal))
	for i, r := range journal {
		switch {
		case r.Err != nil:
			m := r.Err.Error()
			entries[i] = entry{Err: &m}
		default:
			v := r.Value
			if len(v) == 0 {
				v = json.RawMessage("null")
			}
			if !json.Valid(v) {
				m := fmt.Sprintf("tool returned invalid JSON: %s", string(v))
				entries[i] = entry{Err: &m}
			} else {
				entries[i] = entry{V: v}
			}
		}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// The injected JS lives in these raw-string constants — real JS, not escaped Go
// fragments — so it reads and reviews as JS and is decoupled from the assembly
// logic. Values (seed, clock, tool names, journal, program) are substituted at
// assembly time (see RunProgram): none of the templates contain a literal '%'.

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

// bridgeJS is the __hostCall bridge: it REPLAYS globalThis.__journal by call order
// (a known call resolves with its recorded result / rejects for a recorded error; a
// new call is pushed to globalThis.__frontier and returns a never-resolving promise
// so the program blocks), and defines each name in globalThis.__toolNames as a plain
// async function over it. Both globals are injected just before this runs.
const bridgeJS = `;(function () {
  var __journal = globalThis.__journal || [];
  var __i = 0;
  var __frontier = [];
  globalThis.__frontier = __frontier;
  globalThis.__hostCall = function (name, arg) {
    var idx = __i++;
    if (idx < __journal.length) {
      var e = __journal[idx];
      return ('err' in e) ? Promise.reject(new Error(e.err)) : Promise.resolve(e.v);
    }
    __frontier.push({ name: name, arg: arg });
    return new Promise(function () {}); // never resolves this run
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

// toolBridge injects this run's tool names, then the (static) __hostCall bridge
// (bridgeJS). Requires globalThis.__journal to be set before it runs — RunProgram
// injects the journal just above it.
func (s *Sandbox) toolBridge() string {
	specs := s.inv.Tools()
	names := make([]string, len(specs))
	for i, sp := range specs {
		names[i] = sp.Name
	}
	namesJSON, _ := json.Marshal(names)
	return "globalThis.__toolNames = " + string(namesJSON) + ";\n" + bridgeJS
}

// ToolCall / ToolResult are the (tool, arg) form the Invoker sees, one per
// pending JS promise in a batch.
type ToolCall struct {
	Tool string
	Arg  json.RawMessage
}

type ToolResult struct {
	Value json.RawMessage
	Err   error
}
