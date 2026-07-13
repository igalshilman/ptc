package main

import (
	"encoding/json"
	"time"

	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

// The two Restate PRIMITIVES that aren't just handler calls, exposed as static tools
// alongside the handlers the agent auto-discovers from the Admin API (see main.go, and
// the separate ./examples/backoffice deployment):
//
//   - sleep   → a LEAF tool (durable timer): one non-blocking submission returning a
//               durable Future the batch awaits.
//   - signal  → a LEAF tool that creates a NAMED signal (the model chooses the name)
//               and BLOCKS on it in one call, returning the value it is completed with.
//               Create+await in one leaf call stays replay-safe (on replay the signal
//               is recreated at the same journal position → same name → reads its
//               resolution from the journal).
//
// Resolving/rejecting a signal by (invocation, name) are ordinary Restate handlers on
// the framework's own AgentSignals service (bound by the agent; see agent/service.go),
// discovered as tools rather than defined here — so this example defines no multi-step
// tools at all.

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

// ---- signal (leaf: create + await a named external signal) ------------------

type signalArgs struct {
	Name string `json:"name" jsonschema:"description=a name for this signal (you choose it); an external caller completes it by this name plus the invocation id"`
}

func signalTool() agent.Tool {
	return agent.NewTool("signal",
		`create a NAMED signal and BLOCK until an external caller completes it (e.g. a human approval or an external event). Choose a name; it resolves to the JSON value it is completed with. The invocation id + name are logged so an external caller can complete it via the resolve/reject tools. arg {"name": string}`,
		func(ctx restate.Context, a signalArgs) (agent.Future[json.RawMessage], error) {
			if a.Name == "" {
				return agent.Future[json.RawMessage]{}, restate.TerminalErrorf("signal needs a name")
			}
			fut := agent.Signal[json.RawMessage](ctx, a.Name)
			// Surface (invocation id, name) so an external caller can complete it.
			// ctx.Log() is replay-aware, so this prints once, not on replay.
			ctx.Log().Info("signal created — awaiting external completion",
				"invocation", ctx.Request().ID, "name", a.Name)
			return fut, nil
		})
}
