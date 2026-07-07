package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	// Invoke runs one tool call. A non-nil error becomes a rejected JS promise.
	Invoke(ctx context.Context, tool string, arg json.RawMessage) (json.RawMessage, error)
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

// RunProgram injects the tool prelude and evaluates the model-generated program
// to completion, returning its `return` value encoded as JSON. A thrown JS error
// or a rejected tool promise surfaces as a non-nil error (which the loop feeds
// back to the model as an observation).
//
// The program is an async function body: it may `await` tools and `return` a
// value. We wrap it so the return is JSON.stringify'd — the raw guest stringifies
// the top-level result with JS_ToCString, which would turn objects into
// "[object Object]". Programs therefore return plain values, not JSON strings.
func (s *Sandbox) RunProgram(ctx context.Context, program string) (string, error) {
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	seq := s.callSeq
	s.callSeq++
	progSeed := int64(uint64(s.seed) ^ (uint64(seq)+1)*0x9e3779b97f4a7c15)

	// Determinism is entirely JS-level now (determinismPrelude, below), carrying
	// this run's progSeed/now — so it works even when the engine reuses a pooled
	// instance whose WASI clock/rand were fixed at instantiation.
	wrapped := "const __ret = await (async function () {\n" + program +
		"\n})();\nreturn JSON.stringify(__ret === undefined ? null : __ret);"
	src := s.determinismPrelude(progSeed) + s.prelude() + "\n" + wrapped
	return s.engine.Run(ctx, src, s)
}

// determinismPrelude makes a program's clock and randomness replay-stable, entirely
// at the JS level so it survives instance REUSE (a pooled instance's WASI clock/rand
// are bound at instantiation and cannot be re-seeded per run — see Engine.Run). It
// overrides, per program, using this run's seed and frozen now:
//   - Math.random (a seeded LCG),
//   - the Date CONSTRUCTOR (not just Date.now — `new Date()` otherwise reads the
//     host clock, not Date.now) so no-arg construction and Date() return the frozen
//     now, while explicit-arg construction/parsing delegate to the real Date,
//   - crypto.getRandomValues and performance.now if present.
func (s *Sandbox) determinismPrelude(seed int64) string {
	return fmt.Sprintf(";(function(){\n"+
		"  var __s = %d >>> 0;\n"+
		"  Math.random = function(){ __s = (Math.imul(__s, 1664525) + 1013904223) >>> 0; return __s / 4294967296; };\n"+
		"  var __now = %d;\n"+
		"  var RealDate = Date;\n"+
		"  function FakeDate() {\n"+
		"    if (!(this instanceof FakeDate)) return new RealDate(__now).toString();\n"+
		"    return arguments.length ? new RealDate(...arguments) : new RealDate(__now);\n"+
		"  }\n"+
		"  FakeDate.prototype = RealDate.prototype;\n"+
		// Redirect .constructor to FakeDate too, else `new (new Date()).constructor()`
		// reaches the native Date and reads the (fixed constant) host clock instead of
		// __now. Scoped to this run's fresh context, so no cross-run mutation leaks.
		"  try { RealDate.prototype.constructor = FakeDate; } catch (e) {}\n"+
		"  FakeDate.now = function(){ return __now; };\n"+
		"  FakeDate.parse = RealDate.parse;\n"+
		"  FakeDate.UTC = RealDate.UTC;\n"+
		"  globalThis.Date = FakeDate;\n"+
		// Best-effort: some builds mark these read-only/non-configurable, so guard
		// against a throw (Math.random + Date above are the load-bearing overrides).
		"  try { if (globalThis.crypto && globalThis.crypto.getRandomValues) {\n"+
		"    globalThis.crypto.getRandomValues = function(a){ for (var i = 0; i < a.length; i++) a[i] = (Math.random() * 256) | 0; return a; };\n"+
		"  } } catch (e) {}\n"+
		"  try { if (globalThis.performance && globalThis.performance.now) {\n"+
		"    globalThis.performance.now = function(){ return 0; };\n"+
		"  } } catch (e) {}\n"+
		"})();\n", uint32(seed), s.nowMillis)
}

// prelude generates the JS injected before the program: it defines each
// registered tool as a plain async function over the guest's __hostCall bridge.
// To the program these look like ordinary promises — no trace of Restate or wasm.
func (s *Sandbox) prelude() string {
	specs := s.inv.Tools()
	toolNames := make([]string, len(specs))
	for i, sp := range specs {
		toolNames[i] = sp.Name
	}
	names, _ := json.Marshal(toolNames)
	var b strings.Builder
	b.WriteString(";(function(){\n")
	b.WriteString("  function __invoke(name, arg) {\n")
	b.WriteString("    return __hostCall(name, JSON.stringify(arg === undefined ? null : arg))\n")
	b.WriteString(`      .then(function(s) { return (s === null || s === undefined || s === "") ? null : JSON.parse(s); });` + "\n")
	b.WriteString("  }\n")
	b.WriteString("  var tools = {};\n")
	b.WriteString("  (" + string(names) + ").forEach(function(n) {\n")
	b.WriteString("    var f = function(arg) { return __invoke(n, arg); };\n")
	b.WriteString("    tools[n] = f; globalThis[n] = f;\n")
	b.WriteString("  });\n")
	b.WriteString("  globalThis.tools = tools;\n")
	b.WriteString("})();\n")
	return b.String()
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

// BatchInvoker, if implemented by an Invoker, resolves a whole batch of pending
// tool calls together. This is what enables durable-PARALLEL execution: when a
// program fires several tools via Promise.all they arrive here as one batch, and
// a BatchInvoker (see restateInvoker) can run the parallel-capable ones
// concurrently via restate.RunAsync. Invokers that don't implement it fall back
// to one-at-a-time Invoke.
type BatchInvoker interface {
	InvokeBatch(ctx context.Context, calls []ToolCall) []ToolResult
}

// Resolve implements Resolver: each pending call already carries its tool name
// and argument (from __hostCall), so we invoke the batch (concurrently if the
// Invoker supports it) and map results back to handles.
func (s *Sandbox) Resolve(ctx context.Context, calls []HostCall) []HostResult {
	toolCalls := make([]ToolCall, len(calls))
	for i, c := range calls {
		toolCalls[i] = ToolCall{Tool: c.Tool, Arg: json.RawMessage(c.Arg)}
	}

	// Invoke — as one batch if the Invoker supports it, else one at a time.
	results := make([]ToolResult, len(calls))
	if b, ok := s.inv.(BatchInvoker); ok {
		results = b.InvokeBatch(ctx, toolCalls)
	} else {
		for i, tc := range toolCalls {
			v, err := s.inv.Invoke(ctx, tc.Tool, tc.Arg)
			results[i] = ToolResult{Value: v, Err: err}
		}
	}

	// Map results back to guest handles, validating the JSON contract.
	out := make([]HostResult, len(calls))
	for i, c := range calls {
		switch {
		case results[i].Err != nil:
			out[i] = HostResult{Handle: c.Handle, Value: results[i].Err.Error(), IsError: true}
		default:
			v := results[i].Value
			if len(v) == 0 {
				v = json.RawMessage("null")
			}
			// The tool contract is JSON; validate so a misbehaving tool yields a
			// clear, tool-named error instead of an opaque JS parse rejection.
			if !json.Valid(v) {
				out[i] = HostResult{Handle: c.Handle, Value: fmt.Sprintf("tool %q returned invalid JSON", toolCalls[i].Tool), IsError: true}
			} else {
				out[i] = HostResult{Handle: c.Handle, Value: string(v)}
			}
		}
	}
	return out
}
