// Command orchestrator is an example durable CodeAct agent that combines two things:
//
//   - AUTO-DISCOVERED handler tools: every handler annotated with
//     restate.WithMetadata(agent.AgentToolAnnotation, …) in your Restate deployment
//     becomes a tool, discovered from the Admin API. The model orchestrates your
//     existing durable services (in parallel, durably) via generated code.
//
//   - The two RESTATE PRIMITIVES that aren't just handler calls, as static tools:
//     sleep (durable timer) and signal (a named external signal). resolve/reject a
//     signal are themselves discovered handlers (the Signals service).
//
//     OPENAI_API_KEY=sk-...  go run ./examples/orchestrator   # serves on :9080
//
// Discovery runs LAZILY on each Ask (journaled for replay), not at startup — so it
// works even for the co-deployed demo services here (Echo, Counter, Signals), which
// register alongside the agent and so aren't visible until after it is registered.
// New annotated services deployed between turns are picked up on the next Ask.
//
// Env: RESTATE_ADMIN_URL (default http://localhost:9070), AGENT_ADDR (default :9080),
// AGENT_MODEL (default gpt-5), OPENAI_BASE_URL, OPENAI_API_KEY (required).
package main

import (
	"os"

	"github.com/openai/openai-go/v3"
	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

func main() {
	agent.Main(agent.RunConfig{
		// Co-deploy demo target services (their handlers are annotated for discovery)
		// so the standalone agent has something to discover — including Signals, whose
		// resolve/reject handlers complete a named signal by (invocation, name).
		Extra: []restate.ServiceDefinition{echoService(), counterService(), signalsService()},
		// Discover annotated handlers from the Admin API, lazily, per Ask.
		Discover: &agent.DiscoverConfig{AdminURL: os.Getenv("RESTATE_ADMIN_URL")},
		// The two primitives that aren't just handler calls (durable timer + named signal).
		Tools: func(_ openai.Client, _ string) []agent.Tool {
			return []agent.Tool{
				sleepTool(),
				signalTool(),
			}
		},
	})
}
