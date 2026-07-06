package agent

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
	restate "github.com/restatedev/sdk-go"
)

// Tool is a developer-registered capability the agent can call. Build one with
// NewTool or NewRunTool; the JS program calls it as a plain async function.
// Params is the JSON Schema for its argument object, surfaced to the model.
type Tool struct {
	Name        string
	Description string
	Params      json.RawMessage

	// Exactly one of these is set. contextHandler is a durable/context tool: it
	// holds the live restate.Context (can restate.Run, set timers, call services)
	// and runs SEQUENTIALLY within a batch. runHandler is a "run tool": one durable
	// step the agent wraps in restate.RunAsync, so a batch of them runs in PARALLEL.
	contextHandler func(ctx restate.Context, arg json.RawMessage) (json.RawMessage, error)
	runHandler     func(rc restate.RunContext, arg json.RawMessage) (json.RawMessage, error)
}

// NewTool builds a durable/context tool from a typed handler: the argument JSON
// Schema is reflected from A (honoring `json:`/`jsonschema:` tags) and surfaced to
// the model, and the raw args are unmarshaled into A before the handler runs. Use
// this when the tool needs the full restate.Context (timers, service calls,
// state). Such tools run sequentially within a Promise.all batch.
func NewTool[A any](name, description string, fn func(ctx restate.Context, args A) (any, error)) Tool {
	return Tool{
		Name:        name,
		Description: description,
		Params:      reflectSchema[A](),
		contextHandler: func(ctx restate.Context, raw json.RawMessage) (json.RawMessage, error) {
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

// NewRunTool builds a "run tool": its body is a single durable step (a plain side
// effect — HTTP, DB, compute) that gets a RunContext (no nested restate ops). The
// agent wraps it in restate.RunAsync, so when a program fires several run-tools
// via Promise.all they execute durably IN PARALLEL. For tools needing the full
// Context (timers/calls), use NewTool instead.
func NewRunTool[A any](name, description string, fn func(rc restate.RunContext, args A) (any, error)) Tool {
	return Tool{
		Name:        name,
		Description: description,
		Params:      reflectSchema[A](),
		runHandler: func(rc restate.RunContext, raw json.RawMessage) (json.RawMessage, error) {
			a, err := unmarshalArgs[A](name, raw)
			if err != nil {
				return nil, err
			}
			out, err := fn(rc, a)
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

// reflectSchema generates an inlined JSON Schema for A, mirroring how sdk-go
// itself uses invopop/jsonschema (a plain Reflector.Reflect; ExpandedStruct is
// avoided due to an upstream panic bug, so we inline via DoNotReference). A
// should be a struct type.
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
		specs[i] = ToolSpec{Name: t.Name, Description: t.Description, Params: t.Params}
	}
	return specs
}
