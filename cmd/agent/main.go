// Command agent runs the durable CodeAct AI agent as a Restate service.
//
//	OPENAI_API_KEY=sk-...  go run ./cmd/agent      # serves the Agent object on :9080
//
// Env: AGENT_ADDR (default :9080), AGENT_MODEL (default gpt-5; e.g. gpt-5-mini /
// gpt-5-nano for cheaper+faster), OPENAI_BASE_URL (optional endpoint override).
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/restatedev/sdk-go/server"

	"quickjsworker/agent"
)

func main() {
	ctx := context.Background()

	svc, err := setup(ctx)
	if err != nil {
		log.Fatalf("setup: %v", err)
	}
	defer svc.Close(ctx)

	addr := envOr("AGENT_ADDR", ":9080")
	log.Printf("durable CodeAct agent listening on %s", addr)
	// The entry point binds the durable services (Agent + AgentTools) and serves them.
	srv := server.NewRestate()
	for _, d := range svc.Definitions() {
		srv = srv.Bind(d)
	}
	if err := srv.Start(ctx, addr); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// setup wires the agent: it creates the model client, registers the durable
// tools, and configures the loop. This is the one place to add/change tools.
func setup(ctx context.Context) (*agent.Service, error) {
	// Fail fast at boot rather than at the first model call. For a keyless local
	// OpenAI-compatible endpoint, set OPENAI_API_KEY=dummy.
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, errors.New("OPENAI_API_KEY is not set")
	}
	clientOpts := []option.RequestOption{option.WithAPIKey(key)}
	if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(base))
	}
	return agent.NewService(ctx, agent.Config{
		Client:        openai.NewClient(clientOpts...),
		Model:         envOr("AGENT_MODEL", "gpt-5"),
		MaxRounds:     10,
		ProgramBudget: 60 * time.Second,
		Tools: []agent.Tool{
			computeTool(),
			httpGetTool(),
			waitTool(),
			delayedFetchTool(),
		},
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
