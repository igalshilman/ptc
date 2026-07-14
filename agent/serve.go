package agent

import (
	"context"
	"errors"
	"fmt"
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
// definitions, and Deploy exposes a bound deployment to Restate either as a local
// listener (dev) or via an outbound Restate Cloud tunnel. An example wires them together
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

// Serve binds the Service's Restate service definitions (plus any extra ones) and exposes
// the deployment to Restate via Deploy (dev listener vs. Cloud tunnel, chosen by the
// environment — see Deploy), blocking until it stops. A fatal error exits the process.
func Serve(ctx context.Context, svc *Service, extra ...restate.ServiceDefinition) {
	srv := server.NewRestate()
	for _, d := range svc.Definitions() {
		srv = srv.Bind(d)
	}
	for _, d := range extra {
		srv = srv.Bind(d)
	}
	if err := Deploy(ctx, srv, envOr("AGENT_ADDR", ":9080"), "agent"); err != nil {
		log.Fatalf("agent.Serve: %v", err)
	}
}

// Deploy exposes a bound *server.Restate to Restate and blocks until it stops.
//
// By DEFAULT (dev-friendly, zero config) it LISTENS on addr: the Restate runtime connects
// IN and you register the deployment by URL (POST /deployments). This is what `make run`
// and the Quick start use — nothing to set.
//
// Set RESTATE_TUNNEL to instead open an OUTBOUND tunnel to Restate Cloud (no inbound
// listener or public URL needed — for private networks / Cloud). Tunnel mode also needs
// RESTATE_REGION, RESTATE_ENVIRONMENT_ID, RESTATE_SIGNING_KEY, RESTATE_AUTH_TOKEN (all
// required) and RESTATE_TUNNEL_NAME (optional; defaults to name).
//
// name identifies the deployment (and is the default tunnel name); addr is the listen
// address. Both examples deploy through this, so the whole demo listens or tunnels together.
func Deploy(ctx context.Context, srv *server.Restate, addr, name string) error {
	if _, useTunnel := os.LookupEnv("RESTATE_TUNNEL"); !useTunnel {
		log.Printf("[%s] listening on %s (register this address with Restate)", name, addr)
		return srv.Start(ctx, addr)
	}

	region := os.Getenv("RESTATE_REGION")
	envID := os.Getenv("RESTATE_ENVIRONMENT_ID")
	signingKey := os.Getenv("RESTATE_SIGNING_KEY")
	token := os.Getenv("RESTATE_AUTH_TOKEN")
	if region == "" || envID == "" || signingKey == "" || token == "" {
		return fmt.Errorf("RESTATE_TUNNEL is set but tunnel config is incomplete; need " +
			"RESTATE_REGION, RESTATE_ENVIRONMENT_ID, RESTATE_SIGNING_KEY and RESTATE_AUTH_TOKEN")
	}
	tunnelName := envOr("RESTATE_TUNNEL_NAME", name)
	log.Printf("[%s] tunneling to Restate Cloud (region %s) as %q", name, region, tunnelName)
	return tunnel.NewTunnel(srv,
		tunnel.WithRegion(region),
		tunnel.WithEnvironment(envID, signingKey),
		tunnel.WithAuthToken(token),
		tunnel.WithTunnelName(tunnelName),
	).Start(ctx)
}

// envOr returns the environment value for key, or def if it is unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
