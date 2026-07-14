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
	"github.com/restatedev/sdk-go/x/tunnel"
)

// serve.go holds the conveniences an example main() shares on top of the pure API
// (NewService / Definitions / Close, which is what the tests use): ClientFromEnv resolves
// the OpenAI client + model from the environment, Serve binds the Service's Restate
// definitions, and Deploy connects the deployment to Restate Cloud through an outbound
// tunnel. An example wires them together itself (see examples/orchestrator).

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

// Serve binds the Service's Restate service definitions (plus any extra ones) and connects
// them to Restate Cloud via Deploy, blocking until it stops. A fatal error exits the process.
func Serve(ctx context.Context, svc *Service, extra ...restate.ServiceDefinition) {
	srv := server.NewRestate()
	for _, d := range svc.Definitions() {
		srv = srv.Bind(d)
	}
	for _, d := range extra {
		srv = srv.Bind(d)
	}
	if err := Deploy(ctx, srv, "agent"); err != nil {
		log.Fatalf("agent.Serve: %v", err)
	}
}

// Deploy connects a bound *server.Restate to Restate Cloud through an outbound tunnel
// (github.com/restatedev/sdk-go/x/tunnel) and blocks until it stops — no inbound listener
// or public URL.
//
// All tunnel configuration (environment id, signing key, cloud region / servers, auth
// token) is read by the tunnel itself from the injected environment; Deploy passes none
// of it. The ONE exception is the tunnel NAME: each deployment needs its own, and two
// co-deployed services can't share a single RESTATE_INPROC_TUNNEL_NAME — so pass a
// non-empty tunnelName to set it in code; pass "" to let the tunnel read
// RESTATE_INPROC_TUNNEL_NAME from the environment.
func Deploy(ctx context.Context, srv *server.Restate, tunnelName string) error {
	var opts []tunnel.Option
	if tunnelName != "" {
		opts = append(opts, tunnel.WithTunnelName(tunnelName))
	}
	return tunnel.NewTunnel(srv, opts...).Start(ctx)
}

// envOr returns the environment value for key, or def if it is unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
