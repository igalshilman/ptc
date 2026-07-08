package main

import (
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
//
// The signal-completion tools (resolve/reject) are NOT defined here — they're handlers
// on the framework's own AgentTools service (see agent/service.go), discovered the same
// way. In real use you'd annotate handlers across your own services and point the agent
// at them; these keep the demo self-contained.

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
