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
//	BACKOFFICE_ADDR=:9081  go run ./examples/backoffice   # serves on :9081
package main

import (
	"context"
	"log"
	"os"

	"github.com/restatedev/sdk-go/server"
)

func main() {
	addr := os.Getenv("BACKOFFICE_ADDR")
	if addr == "" {
		addr = ":9081"
	}
	log.Printf("order-fulfillment back-office listening on %s", addr)
	srv := server.NewRestate().
		Bind(inventoryService()).
		Bind(riskCheckService()).
		Bind(paymentsService())
	if err := srv.Start(context.Background(), addr); err != nil {
		log.Fatalf("backoffice: %v", err)
	}
}
