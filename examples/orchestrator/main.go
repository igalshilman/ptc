// Command orchestrator is an example durable CodeAct agent: an autonomous
// ORDER-FULFILLMENT agent dropped into a Restate deployment. It discovers the durable
// handlers you've annotated and orchestrates them via generated code — reserving stock
// for each line item in PARALLEL, scoring the order's risk, and (if flagged) opening a
// named SIGNAL and blocking for a human's approval before charging. Every step is
// durable: crash it mid-wait, or let it wait overnight, and it resumes; payment is
// charged exactly once.
//
// It combines two kinds of tool:
//
//   - AUTO-DISCOVERED handler tools: every handler annotated with
//     restate.WithMetadata(agent.AgentToolAnnotation, "<name>") becomes a tool,
//     discovered from the Admin API — reserve_stock (keyed Inventory), risk_score
//     (RiskCheck), charge_payment (Payments), all served by the SEPARATE back-office
//     deployment (see ./examples/backoffice), plus resolve/reject from the framework's
//     own AgentSignals service. No manual wiring.
//
//   - The two RESTATE PRIMITIVES that aren't just handler calls, as static tools:
//     sleep (durable timer) and signal (create + await a named external signal).
//
//     OPENAI_API_KEY=sk-...  go run ./examples/orchestrator
//
// It connects to Restate Cloud through an outbound tunnel (agent.Deploy);
// the back-office runs as its own tunneled deployment (./examples/backoffice). Discovery
// runs LAZILY on each Ask (journaled for replay), not at startup, so it doesn't matter
// which deployment registers first, and new annotated services are picked up on the next
// Ask.
//
// Env: RESTATE_ADMIN_URL (default http://localhost:9070), AGENT_MODEL (default gpt-5),
// OPENAI_BASE_URL, OPENAI_API_KEY (required), plus the tunnel's RESTATE_INPROC_* vars
// (see the README's "Deploying through a tunnel").
package main

import (
	"context"
	"log"
	"os"

	"restatedev/agent"
)

func main() {
	ctx := context.Background()

	// The OpenAI client + model come from the environment (OPENAI_API_KEY required,
	// AGENT_MODEL / OPENAI_BASE_URL optional).
	client, model, err := agent.ClientFromEnv()
	if err != nil {
		log.Fatalf("orchestrator: %v", err)
	}

	svc, err := agent.NewService(ctx, agent.Config{
		Client: client,
		Model:  model,
		// The two primitives that aren't just handler calls (durable timer + named signal).
		Tools: []agent.Tool{sleepTool(), signalTool()},
		// Discover annotated handlers (the back-office deployment + the framework's
		// AgentSignals service) from the Admin API, lazily, per Ask.
		Discover: &agent.DiscoverConfig{AdminURL: os.Getenv("RESTATE_ADMIN_URL")},
	})
	if err != nil {
		log.Fatalf("orchestrator: build service: %v", err)
	}
	defer svc.Close(ctx)

	// Deploy the agent's Restate definitions to Restate Cloud through the tunnel (name "agent").
	if err := agent.Deploy(ctx, "agent", svc.Definitions()...); err != nil {
		log.Fatalf("orchestrator: %v", err)
	}
}
