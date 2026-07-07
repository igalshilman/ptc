// Command research is an example durable CodeAct agent: a Wikipedia research
// assistant. The model writes small JS programs that call the tools below; each
// model call and tool call is a durable, journaled Restate step.
//
//	OPENAI_API_KEY=sk-...  go run ./examples/research   # serves the Agent object on :9080
//
// Env: AGENT_ADDR (default :9080), AGENT_MODEL (default gpt-5), OPENAI_BASE_URL
// (optional endpoint override). All the wiring lives in agent.Main; this file is
// just the tool set (see tools.go) plus the one call that serves it.
package main

import (
	"github.com/openai/openai-go/v3"

	"restatedev/agent"
)

func main() {
	agent.Main(agent.RunConfig{
		// Tools receives the env-resolved client + model so research_topic can capture
		// them for its own durable summarization call.
		Tools: func(client openai.Client, model string) []agent.Tool {
			return []agent.Tool{
				wikiSearchTool(),
				wikiFetchTool(),
				researchTopicTool(client, model),
			}
		},
	})
}
