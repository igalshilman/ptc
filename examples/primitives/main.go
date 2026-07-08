// Command primitives is an example durable CodeAct agent that exposes Restate's
// durable PRIMITIVES directly as tools — a durable timer (sleep), a service call
// (rpc), and an external awaitable (awakeable / resolve / reject). The model writes
// JavaScript that composes them into durable workflows.
//
//	OPENAI_API_KEY=sk-...  go run ./examples/primitives   # serves on :9080
//
// Try (against a real Restate runtime): ask something like
//
//	"sleep 2 seconds, then rpc the Echo service's echo handler with {message:'hi'},
//	 then create an awakeable and wait for my approval before finishing."
//
// The awakeable's id is logged; complete it from outside, e.g. via the Restate
// awakeable API or a second session's `resolve`/`reject` tool.
//
// Env: AGENT_ADDR (default :9080), AGENT_MODEL (default gpt-5), OPENAI_BASE_URL.
package main

import (
	"github.com/openai/openai-go/v3"
	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

func main() {
	agent.Main(agent.RunConfig{
		// The Echo service gives the rpc tool a real, self-contained target to call.
		Extra: []restate.ServiceDefinition{echoService()},
		Tools: func(_ openai.Client, _ string) []agent.Tool {
			return []agent.Tool{
				sleepTool(),
				rpcTool(),
				awakeableTool(),
				resolveTool(),
				rejectTool(),
			}
		},
	})
}
