package main

import (
	"encoding/json"
	"errors"
	"fmt"

	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

// A small order-fulfillment back-office, co-deployed with the agent. Each handler is
// ANNOTATED with restate.WithMetadata(agent.AgentToolAnnotation, "<toolName>") — that
// opt-in marker is what discovery filters on — so the agent discovers them as tools
// (by the annotated name) and orchestrates them via generated code, in parallel and
// durably, with no manual wiring:
//
//   - Inventory (keyed Virtual Object)  reserve_stock   — reserve units of a SKU
//   - RiskCheck (Service)               risk_score      — score an order for review
//   - Payments  (Service)               charge_payment  — charge a customer
//   - Signals   (Service)               resolve/reject  — complete a human-approval signal
//
// In real use you'd annotate handlers across your own services and point the agent at
// them; these keep the demo self-contained. Agent/AgentTools are unannotated and never
// exposed.

// ---- Inventory: keyed Virtual Object, one stock count per SKU ---------------

type reserveIn struct {
	Qty int `json:"qty" jsonschema:"description=how many units to reserve"`
}
type reserveOut struct {
	SKU       string `json:"sku"`
	Reserved  int    `json:"reserved"`
	Remaining int    `json:"remaining"`
	OK        bool   `json:"ok"`
}

func inventoryService() restate.ServiceDefinition {
	const stockKey, seededKey, initialStock = "stock", "seeded", 100
	return restate.NewObject("Inventory").
		Handler("reserve", restate.NewObjectHandler(
			func(ctx restate.ObjectContext, in reserveIn) (reserveOut, error) {
				sku := restate.Key(ctx)
				seeded, err := restate.Get[bool](ctx, seededKey)
				if err != nil {
					return reserveOut{}, err
				}
				if !seeded { // first touch of this SKU: seed initial stock
					restate.Set(ctx, stockKey, initialStock)
					restate.Set(ctx, seededKey, true)
				}
				remaining, err := restate.Get[int](ctx, stockKey)
				if err != nil {
					return reserveOut{}, err
				}
				ok := in.Qty > 0 && in.Qty <= remaining
				reserved := 0
				if ok {
					reserved = in.Qty
					remaining -= reserved
					restate.Set(ctx, stockKey, remaining)
				}
				return reserveOut{SKU: sku, Reserved: reserved, Remaining: remaining, OK: ok}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "reserve_stock")))
}

// ---- RiskCheck: score an order, flag large ones for review -----------------

type scoreIn struct {
	Customer string  `json:"customer" jsonschema:"description=the customer id"`
	Amount   float64 `json:"amount" jsonschema:"description=the order total in dollars"`
}
type scoreOut struct {
	Score   int    `json:"score"`
	Flagged bool   `json:"flagged"`
	Reason  string `json:"reason"`
}

func riskCheckService() restate.ServiceDefinition {
	return restate.NewService("RiskCheck").
		Handler("score", restate.NewServiceHandler(
			func(ctx restate.Context, in scoreIn) (scoreOut, error) {
				score := int(in.Amount / 20) // toy heuristic: larger orders score higher
				flagged := in.Amount >= 1000
				reason := "within auto-approval limit"
				if flagged {
					reason = "order total >= $1000 — needs human approval"
				}
				return scoreOut{Score: score, Flagged: flagged, Reason: reason}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "risk_score")))
}

// ---- Payments: charge a customer -------------------------------------------

type chargeIn struct {
	Customer string  `json:"customer" jsonschema:"description=the customer to charge"`
	Amount   float64 `json:"amount" jsonschema:"description=the amount to charge in dollars"`
}
type chargeOut struct {
	TxnID   string  `json:"txn_id"`
	Charged float64 `json:"charged"`
}

func paymentsService() restate.ServiceDefinition {
	return restate.NewService("Payments").
		Handler("charge", restate.NewServiceHandler(
			func(ctx restate.Context, in chargeIn) (chargeOut, error) {
				if in.Amount <= 0 {
					return chargeOut{}, restate.TerminalErrorf("amount must be positive")
				}
				n := restate.Rand(ctx).Int64()
				if n < 0 {
					n = -n
				}
				txn := fmt.Sprintf("txn-%06d", n%1_000_000)
				ctx.Log().Info("charged", "customer", in.Customer, "amount", in.Amount, "txn", txn)
				return chargeOut{TxnID: txn, Charged: in.Amount}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "charge_payment")))
}

// ---- Signals: complete a named human-approval signal by (invocation, name) --

type resolveIn struct {
	Invocation string          `json:"invocation" jsonschema:"description=the invocation id awaiting the signal (from the signal tool's log)"`
	Name       string          `json:"name" jsonschema:"description=the signal name to complete"`
	Value      json.RawMessage `json:"value,omitempty" jsonschema:"description=the JSON value to resolve it with"`
}
type rejectIn struct {
	Invocation string `json:"invocation" jsonschema:"description=the invocation id awaiting the signal"`
	Name       string `json:"name" jsonschema:"description=the signal name to reject"`
	Reason     string `json:"reason,omitempty" jsonschema:"description=why it is being rejected"`
}
type okOut struct {
	OK bool `json:"ok"`
}

// signalsService resolves/rejects NAMED signals on a target invocation. Ordinary
// handlers (full restate.Context, so they can issue the ctx-level ResolveSignal /
// RejectSignal command), discovered as plain leaf tools — no AgentTools/Exec. By
// (invocation, name) they complete a signal awaited by ANOTHER session (the one
// blocked in the `signal` tool waiting for approval).
func signalsService() restate.ServiceDefinition {
	return restate.NewService("Signals").
		Handler("resolve", restate.NewServiceHandler(
			func(ctx restate.Context, in resolveIn) (okOut, error) {
				if in.Invocation == "" || in.Name == "" {
					return okOut{}, restate.TerminalErrorf("resolve needs an invocation id and signal name")
				}
				v := in.Value
				if len(v) == 0 {
					v = json.RawMessage("null")
				}
				restate.ResolveSignal(ctx, in.Invocation, in.Name, v)
				return okOut{OK: true}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "resolve"))).
		Handler("reject", restate.NewServiceHandler(
			func(ctx restate.Context, in rejectIn) (okOut, error) {
				if in.Invocation == "" || in.Name == "" {
					return okOut{}, restate.TerminalErrorf("reject needs an invocation id and signal name")
				}
				reason := in.Reason
				if reason == "" {
					reason = "rejected"
				}
				restate.RejectSignal(ctx, in.Invocation, in.Name, errors.New(reason))
				return okOut{OK: true}, nil
			}, restate.WithMetadata(agent.AgentToolAnnotation, "reject")))
}
