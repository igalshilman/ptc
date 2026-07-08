package main

import (
	"encoding/json"
	"errors"
	"time"

	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

// The Restate durable PRIMITIVES, exposed as static tools alongside the handlers the
// agent auto-discovers from the Admin API (see main.go / services.go). Together the
// model can both call your annotated durable handlers AND compose raw primitives:
//
//   - sleep                 → a LEAF tool: one non-blocking submission returning a
//                             durable Future the batch awaits.
//   - awakeable             → a LEAF tool that CREATES the awaitable and BLOCKS on it in
//                             one call (returning the resolved value). Not split into
//                             create-then-await: an awaitable can't be rehydrated from a
//                             bare id later, and a create-only call has no future to
//                             await — so create+await in one leaf call is what stays
//                             replay-safe (on replay it is recreated at the same journal
//                             position → same id).
//   - resolve, reject       → SEQ tools: resolving/rejecting is a ctx command (not a Run
//                             side effect), so it runs on the full restate.Context. By
//                             id, it can signal an awaitable awaited by ANOTHER run.

// ---- sleep (leaf: durable timer) --------------------------------------------

type sleepArgs struct {
	Seconds float64 `json:"seconds" jsonschema:"description=how long to pause, in seconds"`
}
type sleepResult struct {
	SleptSeconds float64 `json:"slept_seconds"`
}

func sleepTool() agent.Tool {
	return agent.NewTool("sleep",
		`pause for N seconds; arg {"seconds": number}. Several sleeps in one Promise.all run concurrently; also useful in Promise.race([work, sleep]) to bound work with a timeout`,
		func(ctx restate.Context, a sleepArgs) (agent.Future[sleepResult], error) {
			d := time.Duration(a.Seconds * float64(time.Second))
			return agent.Timer(ctx, d, sleepResult{SleptSeconds: a.Seconds}, restate.WithName("sleep")), nil
		})
}

// ---- awakeable (leaf: create + await an external signal) --------------------

type awakeableArgs struct {
	Label string `json:"label,omitempty" jsonschema:"description=a human label for this awaitable, shown in the log next to its id"`
}

func awakeableTool() agent.Tool {
	return agent.NewTool("human_approval",
		`create a promise and BLOCK until someone resolves or rejects it (e.g. a human approval or an external event). Its id is logged so an external caller can resolve/reject it; resolves to the JSON value it was completed with. arg {"label": string}`,
		func(ctx restate.Context, a awakeableArgs) (agent.Future[json.RawMessage], error) {
			fut, id := agent.Awakeable[json.RawMessage](ctx)
			// Surface the id so an external caller (or another session's resolve tool) can
			// complete it. ctx.Log() is replay-aware, so this prints once, not on replay.
			ctx.Log().Info("awakeable created — awaiting external completion", "id", id, "label", a.Label)
			return fut, nil
		})
}

// ---- resolve / reject (seq: complete someone's awaitable by id) -------------

type resolveArgs struct {
	ID    string          `json:"id" jsonschema:"description=the awakeable id to resolve (from the awakeable tool's log)"`
	Value json.RawMessage `json:"value,omitempty" jsonschema:"description=the JSON value to resolve it with"`
}
type rejectArgs struct {
	ID     string `json:"id" jsonschema:"description=the awakeable id to reject"`
	Reason string `json:"reason,omitempty" jsonschema:"description=why it is being rejected"`
}
type okResult struct {
	OK bool `json:"ok"`
}

func resolveTool() agent.Tool {
	return agent.NewSeqTool("resolve",
		`resolve an awaitable by id with a JSON value (completing an awakeable awaited by another run): {id, value}`,
		func(ctx restate.Context, a resolveArgs) (okResult, error) {
			if a.ID == "" {
				return okResult{}, restate.TerminalErrorf("resolve needs an awakeable id")
			}
			value := a.Value
			if len(value) == 0 {
				value = json.RawMessage("null")
			}
			restate.ResolveAwakeable(ctx, a.ID, value)
			return okResult{OK: true}, nil
		})
}

func rejectTool() agent.Tool {
	return agent.NewSeqTool("reject",
		`reject an awaitable by id with a reason (failing an awakeable awaited by another run): {id, reason}`,
		func(ctx restate.Context, a rejectArgs) (okResult, error) {
			if a.ID == "" {
				return okResult{}, restate.TerminalErrorf("reject needs an awakeable id")
			}
			reason := a.Reason
			if reason == "" {
				reason = "rejected"
			}
			restate.RejectAwakeable(ctx, a.ID, errors.New(reason))
			return okResult{OK: true}, nil
		})
}
