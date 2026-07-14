// Command backoffice is a STANDALONE Restate deployment of a small order-fulfillment
// back-office — Inventory / RiskCheck / Payments (see services.go) — with each handler
// ANNOTATED for agent discovery (restate.WithMetadata(agent.AgentToolAnnotation, …)).
//
// It is an ordinary Restate service deployment: it knows nothing about the agent and
// imports the agent package only for the annotation-key constant. Run it alongside the
// orchestrator agent and register BOTH deployments with your Restate runtime; the agent
// then discovers these handlers as tools (by their annotated names) and orchestrates
// them. Splitting it out of the agent mirrors real use — the services an agent drives
// live in their own deployment, discovered over the Admin API, not co-deployed.
//
// It is served via agent.Deploy, so it makes the SAME dev-listener vs. Restate Cloud
// tunnel choice as the agent (RESTATE_DEV) — the whole demo is local or all-tunnel
// together.
//
//	RESTATE_DEV=1 BACKOFFICE_ADDR=:9081  go run ./examples/backoffice   # listens on :9081
package main

import (
	"context"
	"log"
	"os"

	"github.com/restatedev/sdk-go/server"

	"restatedev/agent"
)

func main() {
	addr := os.Getenv("BACKOFFICE_ADDR")
	if addr == "" {
		addr = ":9081"
	}
	srv := server.NewRestate().
		Bind(inventoryService()).
		Bind(riskCheckService()).
		Bind(paymentsService())
	if err := agent.Deploy(context.Background(), srv, addr, "backoffice"); err != nil {
		log.Fatalf("backoffice: %v", err)
	}
}
