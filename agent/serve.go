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

// serve.go is the convenience entrypoint shared by every example under examples/.
// It resolves the OpenAI client + model from the environment, wires the Service, and
// serves it — so an example is just its tool set plus a one-call main(). The pure API
// (NewService / Definitions / Close) is unchanged and still available for full control
// (that's what the tests use); Main/Serve are only opinionated sugar on top of it.

// RunConfig configures the Main convenience entrypoint. Only Tools is required; the
// rest fall back to Config's defaults (see NewService).
type RunConfig struct {
	// Tools builds the example's tool set. It receives the OpenAI client and model id
	// resolved from the environment, so a tool may CAPTURE them — e.g. a tool that makes
	// its own durable model call (see examples/research's research_topic). Required.
	Tools func(client openai.Client, model string) []Tool
	// MaxRounds is the per-message loop budget (0 → default 10).
	MaxRounds int
	// Extra are additional Restate services to bind alongside the agent — e.g. a
	// service the `rpc` tool can call (see examples/primitives). Optional.
	Extra []restate.ServiceDefinition
}

// Main is the one-call entrypoint for an example binary: it resolves the OpenAI client
// and model from the environment (see ClientFromEnv), builds the tool set, constructs
// the Service, binds its Restate services, and serves on AGENT_ADDR (default :9080),
// blocking until the server stops. Any fatal setup/serve error is logged and exits the
// process — it is meant for a `package main`, not for embedding (use NewService/Serve
// directly for that).
func Main(cfg RunConfig) {
	if cfg.Tools == nil {
		log.Fatal("agent.Main: RunConfig.Tools is required")
	}
	ctx := context.Background()
	client, model, err := ClientFromEnv()
	if err != nil {
		log.Fatalf("agent.Main: %v", err)
	}
	svc, err := NewService(ctx, Config{
		Client:    client,
		Model:     model,
		Tools:     cfg.Tools(client, model),
		MaxRounds: cfg.MaxRounds,
	})
	if err != nil {
		log.Fatalf("agent.Main: build service: %v", err)
	}
	defer svc.Close(ctx)
	Serve(ctx, svc, cfg.Extra...)
}

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
