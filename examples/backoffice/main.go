// Command backoffice is a STANDALONE Restate deployment of a small order-fulfillment
// back-office — Inventory / RiskCheck / Payments (see services.go) — with each handler
// ANNOTATED for agent discovery (restate.WithMetadata(agent.AgentToolAnnotation, …)).
//
// It is an ordinary Restate service deployment: it knows nothing about the agent and
// imports the agent package only for the annotation-key constant. Run it alongside the
// orchestrator agent and register BOTH deployments with your Restate Cloud environment;
// the agent then discovers these handlers as tools (by their annotated names) and
// orchestrates them. Splitting it out of the agent mirrors real use — the services an
// agent drives live in their own deployment, discovered over the Admin API.
//
// It is served via agent.Deploy, so — like the agent — it connects to Restate Cloud
// through an outbound tunnel (configured from the injected RESTATE_INPROC_* environment).
package main

import (
	"context"
	"log"

	"github.com/restatedev/sdk-go/server"

	"restatedev/agent"
)

func main() {
	srv := server.NewRestate().
		Bind(inventoryService()).
		Bind(riskCheckService()).
		Bind(paymentsService())
	if err := agent.Deploy(context.Background(), srv, "backoffice"); err != nil {
		log.Fatalf("backoffice: %v", err)
	}
}
