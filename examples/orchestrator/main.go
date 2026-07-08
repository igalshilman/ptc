// Command orchestrator is an example durable CodeAct agent that turns EVERY handler
// registered in your Restate deployment into a tool — discovered from the Admin API
// at startup — so the model can orchestrate your existing durable services (in
// parallel, durably) via generated JavaScript, with no manual tool wiring.
//
//	OPENAI_API_KEY=sk-...  go run ./examples/orchestrator   # serves on :9080
//
// Deploy the services you want the agent to orchestrate BEFORE starting it: discovery
// runs ONCE at startup. The agent's own services (Agent, AgentTools) are skipped
// automatically to avoid same-session reentrancy.
//
// Env: RESTATE_ADMIN_URL (default http://localhost:9070), AGENT_ADDR (default :9080),
// AGENT_MODEL (default gpt-5), OPENAI_BASE_URL, OPENAI_API_KEY (required).
package main

import (
	"context"
	"log"
	"os"

	"restatedev/agent"
)

func main() {
	ctx := context.Background()

	client, model, err := agent.ClientFromEnv()
	if err != nil {
		log.Fatalf("orchestrator: %v", err)
	}

	adminURL := os.Getenv("RESTATE_ADMIN_URL")
	if adminURL == "" {
		adminURL = "http://localhost:9070"
	}
	tools, err := agent.DiscoverTools(ctx, adminURL, agent.DiscoverOptions{})
	if err != nil {
		log.Fatalf("orchestrator: discover handlers: %v", err)
	}
	log.Printf("discovered %d handler tool(s) from %s", len(tools), adminURL)

	svc, err := agent.NewService(ctx, agent.Config{Client: client, Model: model, Tools: tools})
	if err != nil {
		log.Fatalf("orchestrator: %v", err)
	}
	defer svc.Close(ctx)

	agent.Serve(ctx, svc)
}
