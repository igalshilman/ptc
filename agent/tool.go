package agent

import (
	"encoding/json"
	"time"

	"github.com/invopop/jsonschema"
	restate "github.com/restatedev/sdk-go"
)

// Tool is a developer-registered capability the agent can call. To the JS program
// every tool is just a plain async function; internally a tool is EITHER:
//
//   - a leaf op (NewTool): its body performs ONE non-blocking submission and
//     returns the resulting Future (via Run/Call/CallObject/Timer/Awakeable). A
//     batch of leaf tools fired from a single Promise.all runs durably IN PARALLEL,
//     in-process, driven by one restate.Wait.
//   - a sequence (NewSeqTool): an ordinary blocking, multi-step handler with the
//     full restate.Context. It runs in its OWN sub-invocation (via AgentTools/Exec)
//     so it may await/branch/orchestrate freely and STILL parallelize with sibling
//     tools — at the cost of one invocation hop.
//
// Params/Result are the JSON Schemas of the argument and return types, surfaced to
// the model. Exactly one of submit/seqHandler is set.
type Tool struct {
	Name        string
	Description string
	Params      json.RawMessage // arg schema, reflected from A
	Result      json.RawMessage // return schema, reflected from R

	// leaf: produce an in-flight Future on the parent context (non-blocking).
	submit func(ctx restate.Context, arg json.RawMessage) (anyFuture, error)
	// sequence: a blocking multi-step body, run as its own sub-invocation.
	seqHandler func(ctx restate.Context, arg json.RawMessage) (json.RawMessage, error)
}

// Future is an in-flight durable operation that will yield R. A leaf tool returns
// one; it can only be produced via Run/Call/CallObject/Timer/Awakeable, each a
// single non-blocking submission on the restate.Context. The agent submits every
// future in a Promise.all batch first, then drives them together with one
// restate.Wait — so leaf tools parallelize without the author writing any
// concurrency code. Its fields are unexported: a tool cannot fabricate a Future
// that isn't backed by a real durable submission.
type Future[R any] struct {
	sel restate.Future
	get func() (R, error)
}

// anyFuture is the type-erased handle the batch driver holds: something it can
// Wait on and later extract as JSON, regardless of R.
type anyFuture interface {
	selectable() restate.Future
	resultJSON() (json.RawMessage, error)
}

func (f Future[R]) selectable() restate.Future { return f.sel }

func (f Future[R]) resultJSON() (json.RawMessage, error) {
	v, err := f.get()
	if err != nil {
		return nil, err
	}
	return json.Marshal(v) // always valid JSON (invariant: no raw bytes from Run)
}

// Run submits a single durable side effect (HTTP, DB, compute) as a leaf future.
// The body gets a RunContext (no nested Restate ops); its result is journaled, so a
// replay returns the captured value instead of re-running the side effect.
func Run[R any](ctx restate.Context, fn func(restate.RunContext) (R, error), opts ...restate.RunOption) Future[R] {
	f := restate.RunAsync(ctx, fn, opts...)
	return Future[R]{sel: f, get: func() (R, error) { return f.Result() }}
}

// Call submits a request to another Restate service as a leaf future — a durable,
// parallelizable service call.
func Call[R any](ctx restate.Context, service, method string, input any, opts ...restate.RequestOption) Future[R] {
	f := restate.Service[R](ctx, service, method).RequestFuture(input, opts...)
	return Future[R]{sel: f, get: func() (R, error) { return f.Response() }}
}

// CallObject submits a request to a keyed Virtual Object handler as a leaf future.
func CallObject[R any](ctx restate.Context, service, key, method string, input any, opts ...restate.RequestOption) Future[R] {
	f := restate.Object[R](ctx, service, key, method).RequestFuture(input, opts...)
	return Future[R]{sel: f, get: func() (R, error) { return f.Response() }}
}

// Timer returns a leaf future that resolves to value after d elapses — a durable,
// NON-blocking timer, so several timers (and other tools) in a Promise.all all run
// concurrently under the one driver instead of serializing.
func Timer[R any](ctx restate.Context, d time.Duration, value R, opts ...restate.SleepOption) Future[R] {
	f := restate.After(ctx, d, opts...)
	return Future[R]{sel: f, get: func() (R, error) {
		if err := f.Done(); err != nil { // surface a mid-sleep cancellation
			var zero R
			return zero, err
		}
		return value, nil
	}}
}

// Awakeable returns a leaf future that resolves when the returned id is completed
// by an external caller (restate.ResolveAwakeable), plus that id to hand out.
func Awakeable[R any](ctx restate.Context, opts ...restate.AwakeableOption) (Future[R], string) {
	aw := restate.Awakeable[R](ctx, opts...)
	return Future[R]{sel: aw, get: func() (R, error) { return aw.Result() }}, aw.Id()
}

// NewTool registers a LEAF tool: its body performs ONE non-blocking submission and
// returns the resulting Future. The arg schema is reflected from A and the result
// schema from R (both surfaced to the model); raw args are unmarshaled into A. A
// batch of leaf tools fired via Promise.all executes durably IN PARALLEL.
func NewTool[A, R any](name, description string, fn func(ctx restate.Context, args A) (Future[R], error)) Tool {
	return Tool{
		Name:        name,
		Description: description,
		Params:      reflectSchema[A](),
		Result:      reflectSchema[R](),
		submit: func(ctx restate.Context, raw json.RawMessage) (anyFuture, error) {
			a, err := unmarshalArgs[A](name, raw)
			if err != nil {
				return nil, err
			}
			fut, err := fn(ctx, a)
			if err != nil {
				return nil, err
			}
			if fut.sel == nil {
				// A leaf tool must return a Future built by Run/Call/CallObject/Timer/
				// Awakeable. A zero-value Future{} would pass a nil restate.Future to the
				// batch driver → nil-panic → deterministic → retried forever. Reject it
				// cleanly instead.
				return nil, restate.TerminalErrorf("tool %q returned an empty Future (build it with agent.Run/Call/CallObject/Timer/Awakeable)", name)
			}
			return fut, nil
		},
	}
}

// NewSeqTool registers a SEQUENCE tool: an ordinary blocking, multi-step handler
// with the full restate.Context (service calls, durable timers, awakeables, nested
// runs, data-dependent steps). It runs in its OWN sub-invocation, so blocking is
// fine and it still parallelizes with sibling tools in a Promise.all batch — at the
// cost of one invocation hop. Because it runs on the keyless tool service it is
// session-stateless (pass what it needs as args) and must not call back into its
// own Agent session (the parent holds that key's lock while awaiting it).
func NewSeqTool[A, R any](name, description string, fn func(ctx restate.Context, args A) (R, error)) Tool {
	return Tool{
		Name:        name,
		Description: description,
		Params:      reflectSchema[A](),
		Result:      reflectSchema[R](),
		seqHandler: func(ctx restate.Context, raw json.RawMessage) (json.RawMessage, error) {
			a, err := unmarshalArgs[A](name, raw)
			if err != nil {
				return nil, err
			}
			out, err := fn(ctx, a)
			if err != nil {
				return nil, err
			}
			return json.Marshal(out)
		},
	}
}

func unmarshalArgs[A any](name string, raw json.RawMessage) (A, error) {
	var a A
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, restate.TerminalErrorf("bad args for tool %q: %v", name, err)
		}
	}
	return a, nil
}

// reflectSchema generates an inlined JSON Schema for A, mirroring how sdk-go itself
// uses invopop/jsonschema (a plain Reflector.Reflect; ExpandedStruct is avoided due
// to an upstream panic bug, so we inline via DoNotReference). A should be a struct
// type (a scalar/`any` yields a minimal schema, which is fine).
func reflectSchema[A any]() json.RawMessage {
	r := jsonschema.Reflector{DoNotReference: true}
	var zero A
	b, err := json.Marshal(r.Reflect(zero))
	if err != nil {
		return nil
	}
	return b
}

func toolSpecs(tools []Tool) []ToolSpec {
	specs := make([]ToolSpec, len(tools))
	for i, t := range tools {
		specs[i] = ToolSpec{Name: t.Name, Description: t.Description, Params: t.Params, Result: t.Result}
	}
	return specs
}
