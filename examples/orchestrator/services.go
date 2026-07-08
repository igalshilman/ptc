package main

import (
	restate "github.com/restatedev/sdk-go"
)

// Two demo target services, co-deployed with the agent so the standalone orchestrator
// has something to discover: a plain Service (Echo) and a keyed Virtual Object
// (Counter). Discovery (lazy, per Ask) turns their handlers into tools — Echo/echo via
// agent.Call, Counter/* via agent.CallObject with a {key, input} arg. In real use you
// would instead point the agent at OTHER deployments; these keep the demo runnable.

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
			}))
}

type addIn struct {
	Amount int `json:"amount"`
}
type countOut struct {
	Count int `json:"count"`
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
			})).
		Handler("get", restate.NewObjectSharedHandler(
			func(ctx restate.ObjectSharedContext, _ restate.Void) (countOut, error) {
				n, err := restate.Get[int](ctx, countKey)
				return countOut{Count: n}, err
			}))
}
