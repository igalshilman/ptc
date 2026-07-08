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
//     discovered from the Admin API — here reserve_stock (keyed Inventory), risk_score
//     (RiskCheck), charge_payment (Payments), and resolve/reject (Signals). No manual
//     wiring; see services.go.
//
//   - The two RESTATE PRIMITIVES that aren't just handler calls, as static tools:
//     sleep (durable timer) and signal (create + await a named external signal).
//
//     OPENAI_API_KEY=sk-...  go run ./examples/orchestrator   # serves on :9080
//
// Discovery runs LAZILY on each Ask (journaled for replay), not at startup — so it
// works even for the co-deployed back-office here (Inventory, RiskCheck, Payments,
// Signals), which register alongside the agent and aren't visible until after it is
// registered.
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
		// Co-deploy the order-fulfillment back-office (handlers annotated for discovery)
		// so the standalone agent has real services to orchestrate — including Signals,
		// whose resolve/reject handlers complete a named signal by (invocation, name).
		Extra: []restate.ServiceDefinition{
			inventoryService(),
			riskCheckService(),
			paymentsService(),
			signalsService(),
		},
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
