package main

import (
	"encoding/json"
	"time"

	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

// The two Restate PRIMITIVES that aren't just handler calls, exposed as static tools
// alongside the handlers the agent auto-discovers from the Admin API (see main.go /
// services.go):
//
//   - sleep      → a LEAF tool (durable timer): one non-blocking submission returning
//                  a durable Future the batch awaits.
//   - awakeable  → a LEAF tool that CREATES the awaitable and BLOCKS on it in one call
//                  (returning the resolved value). Not split into create-then-await: an
//                  awaitable can't be rehydrated from a bare id later, and a create-only
//                  call has no future to await — so create+await in one leaf call is what
//                  stays replay-safe (on replay it is recreated at the same journal
//                  position → same id).
//
// Resolving/rejecting an awaitable by id are ordinary Restate handlers now (the
// Awakeables service in services.go), discovered as tools rather than defined here —
// so this example needs no seq tools / AgentTools/Exec at all.

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
		`create a promise and BLOCK until someone resolves or rejects it (e.g. a human approval or an external event). Its id is logged so an external caller can complete it (via the discovered Awakeables_resolve / Awakeables_reject tools); resolves to the JSON value it was completed with. arg {"label": string}`,
		func(ctx restate.Context, a awakeableArgs) (agent.Future[json.RawMessage], error) {
			fut, id := agent.Awakeable[json.RawMessage](ctx)
			// Surface the id so an external caller (or another session) can complete it.
			// ctx.Log() is replay-aware, so this prints once, not on replay.
			ctx.Log().Info("awakeable created — awaiting external completion", "id", id, "label", a.Label)
			return fut, nil
		})
}
