package agent

import (
	"encoding/json"
	"time"

	"github.com/invopop/jsonschema"
	restate "github.com/restatedev/sdk-go"
)

// Tool is a developer-registered capability the agent can call. To the JS program a
// tool is just a plain async function; internally its body performs ONE non-blocking
// submission and returns the resulting Future (via Run / Call / CallObject / Timer /
// Awakeable / Signal). A batch of tools fired from a single Promise.all runs durably
// IN PARALLEL, in-process, driven by one restate.WaitFirst.
//
// A multi-step, blocking operation is NOT a special tool kind — model it as a Restate
// handler the tool CALLS (agent.Call / CallObject, or auto-discovered via DiscoverTools):
// the call is a non-blocking future, and the handler runs in its own invocation where
// it may block/branch freely.
//
// Params/Result are the JSON Schemas of the argument and return types, surfaced to the
// model.
type Tool struct {
	Name        string
	Description string
	Params      json.RawMessage // arg schema, reflected from A
	Result      json.RawMessage // return schema, reflected from R

	// submit produces an in-flight Future on the parent context (non-blocking).
	submit func(ctx restate.Context, arg json.RawMessage) (anyFuture, error)
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

// Signal returns a leaf future for a NAMED signal on this invocation. It resolves when
// an external caller completes the signal via restate.ResolveSignal/RejectSignal,
// addressing it by this invocation's id (ctx.Request().ID) and the given name. Unlike
// Awakeable, the name is caller-chosen rather than a system-generated id.
func Signal[R any](ctx restate.Context, name string, opts ...restate.SignalOption) Future[R] {
	f := restate.Signal[R](ctx, name, opts...)
	return Future[R]{sel: f, get: func() (R, error) { return f.Result() }}
}

// NewTool registers a tool: its body performs ONE non-blocking submission and returns
// the resulting Future. The arg schema is reflected from A and the result schema from R
// (both surfaced to the model); raw args are unmarshaled into A. A batch of tools fired
// via Promise.all executes durably IN PARALLEL.
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
				// A tool must return a Future built by Run/Call/CallObject/Timer/Awakeable/
				// Signal. A zero-value Future{} would pass a nil restate.Future to the batch
				// driver → nil-panic → deterministic → retried forever. Reject it cleanly.
				return nil, restate.TerminalErrorf("tool %q returned an empty Future (build it with agent.Run/Call/CallObject/Timer/Awakeable/Signal)", name)
			}
			return fut, nil
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
