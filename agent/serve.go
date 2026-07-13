package agent

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/server"
)

// serve.go holds the two small conveniences an example main() shares on top of the pure
// API (NewService / Definitions / Close, which is what the tests use): ClientFromEnv
// resolves the OpenAI client + model from the environment, and Serve binds the Service's
// Restate definitions and serves them on AGENT_ADDR. An example wires them together
// itself (see examples/orchestrator) — there is no hidden Main entrypoint.

// ClientFromEnv builds an OpenAI(-compatible) client and resolves the model id from the
// environment: OPENAI_API_KEY (required — fail fast at boot; use "dummy" for a keyless
// local endpoint), OPENAI_BASE_URL (optional endpoint override), AGENT_MODEL (default
// gpt-5). This is the client wiring every example shares.
func ClientFromEnv() (openai.Client, string, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return openai.Client{}, "", errors.New("OPENAI_API_KEY is not set")
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	return openai.NewClient(opts...), envOr("AGENT_MODEL", "gpt-5"), nil
}

// Serve binds the Service's Restate service definitions (plus any extra ones) and
// serves them on AGENT_ADDR (default :9080), blocking until the server stops (a fatal
// error exits the process).
func Serve(ctx context.Context, svc *Service, extra ...restate.ServiceDefinition) {
	addr := envOr("AGENT_ADDR", ":9080")
	log.Printf("durable CodeAct agent listening on %s", addr)
	srv := server.NewRestate()
	for _, d := range svc.Definitions() {
		srv = srv.Bind(d)
	}
	for _, d := range extra {
		srv = srv.Bind(d)
	}
	if err := srv.Start(ctx, addr); err != nil {
		log.Fatalf("agent.Serve: %v", err)
	}
}

// envOr returns the environment value for key, or def if it is unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
