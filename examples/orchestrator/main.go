// Command orchestrator is an example durable CodeAct agent that turns EVERY handler
// registered in your Restate deployment into a tool, so the model can orchestrate
// your existing durable services (in parallel, durably) via generated JavaScript,
// with no manual tool wiring.
//
//	OPENAI_API_KEY=sk-...  go run ./examples/orchestrator   # serves on :9080
//
// Discovery runs LAZILY on each Ask (journaled for replay), not at startup — so it
// works even when the target services register alongside the agent in the same
// standalone deployment (they aren't visible until after the server is registered).
// New services deployed between turns are picked up on the next Ask. The agent's own
// services (Agent, AgentTools) are skipped to avoid same-session reentrancy.
//
// Env: RESTATE_ADMIN_URL (default http://localhost:9070), AGENT_ADDR (default :9080),
// AGENT_MODEL (default gpt-5), OPENAI_BASE_URL, OPENAI_API_KEY (required).
package main

import (
	"os"

	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

func main() {
	agent.Main(agent.RunConfig{
		// Co-deploy demo target services so the standalone agent has something to
		// discover; discovery (lazy, per Ask) skips the agent's own Agent/AgentTools.
		Extra:    []restate.ServiceDefinition{echoService(), counterService()},
		Discover: &agent.DiscoverConfig{AdminURL: os.Getenv("RESTATE_ADMIN_URL")},
	})
}
