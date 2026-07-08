package main

import (
	"encoding/json"
	"errors"

	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

// Two demo target services, co-deployed with the agent so the standalone orchestrator
// has something to discover: a plain Service (Echo) and a keyed Virtual Object
// (Counter). Their handlers are ANNOTATED via restate.WithMetadata(agent.AgentToolAnnotation,
// …) — that opt-in marker is what discovery filters on — so they become tools (Echo/echo
// via agent.Call, Counter/* via agent.CallObject with a {key, input} arg). In real use you
// would annotate handlers across your own services and point the agent at them; these
// keep the demo runnable. The Agent/AgentTools handlers are NOT annotated, so they are
// never exposed to the model.

type echoIn struct {
	Message string `json:"message"`
}
type echoOut struct {
	Echo string `json:"echo"`
}

func echoService() restate.ServiceDefinition {
	return restate.NewService("Echo").
		Handler("echo", restate.NewServiceHandler(
			func(ctx restate.Context, in echoIn) (echoOut, error) {
				return echoOut{Echo: "you said: " + in.Message}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "echo")))
}

type addIn struct {
	Amount int `json:"amount"`
}
type countOut struct {
	Count int `json:"count"`
}

type resolveIn struct {
	ID    string          `json:"id" jsonschema:"description=the awakeable id to complete (from the awakeable tool's log)"`
	Value json.RawMessage `json:"value,omitempty" jsonschema:"description=the JSON value to resolve it with"`
}
type rejectIn struct {
	ID     string `json:"id" jsonschema:"description=the awakeable id to reject"`
	Reason string `json:"reason,omitempty" jsonschema:"description=why it is being rejected"`
}
type okOut struct {
	OK bool `json:"ok"`
}

// awakeablesService resolves/rejects awaitables by id. These are ORDINARY handlers,
// not seq tools: each has the full restate.Context, so it can issue the ctx-level
// ResolveAwakeable / RejectAwakeable command, and calling it (a durable service call)
// gives the caller a future — so discovery exposes them as plain leaf tools with no
// AgentTools/Exec sub-invocation. By id they can complete an awaitable awaited by
// ANOTHER session.
func awakeablesService() restate.ServiceDefinition {
	return restate.NewService("Awakeables").
		Handler("resolve", restate.NewServiceHandler(
			func(ctx restate.Context, in resolveIn) (okOut, error) {
				if in.ID == "" {
					return okOut{}, restate.TerminalErrorf("resolve needs an awakeable id")
				}
				v := in.Value
				if len(v) == 0 {
					v = json.RawMessage("null")
				}
				restate.ResolveAwakeable(ctx, in.ID, v)
				return okOut{OK: true}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "resolve"))).
		Handler("reject", restate.NewServiceHandler(
			func(ctx restate.Context, in rejectIn) (okOut, error) {
				if in.ID == "" {
					return okOut{}, restate.TerminalErrorf("reject needs an awakeable id")
				}
				reason := in.Reason
				if reason == "" {
					reason = "rejected"
				}
				restate.RejectAwakeable(ctx, in.ID, errors.New(reason))
				return okOut{OK: true}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "reject")))
}

// Counter is a keyed Virtual Object: each key holds its own durable count.
func counterService() restate.ServiceDefinition {
	const countKey = "count"
	return restate.NewObject("Counter").
		Handler("add", restate.NewObjectHandler(
			func(ctx restate.ObjectContext, in addIn) (countOut, error) {
				n, err := restate.Get[int](ctx, countKey)
				if err != nil {
					return countOut{}, err
				}
				n += in.Amount
				restate.Set(ctx, countKey, n)
				return countOut{Count: n}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "counter_add"))).
		Handler("get", restate.NewObjectSharedHandler(
			func(ctx restate.ObjectSharedContext, _ restate.Void) (countOut, error) {
				n, err := restate.Get[int](ctx, countKey)
				return countOut{Count: n}, err
			}, restate.WithMetadata(agent.AgentToolAnnotation, "counter_get")))
}
