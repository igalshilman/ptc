package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"
)

// DiscoverTools queries a Restate Admin API and turns EVERY registered handler into a
// leaf tool the agent can call — so the model can orchestrate an existing deployment's
// durable handlers (in parallel, durably) via generated code, with no manual wiring.
//
// Each handler becomes a leaf tool backed by a durable service call (agent.Call for a
// plain Service, agent.CallObject for a Virtual Object / Workflow, whose arg is
// {key, input}). The handler's input/output JSON schemas from discovery are surfaced
// to the model, so it calls handlers with the right shapes.
//
// It runs at STARTUP, so the target services must already be registered in the runtime
// (deploy them before starting the agent). The agent's own services (and any in Deny)
// are skipped to avoid same-session reentrancy/deadlock.
func DiscoverTools(ctx context.Context, adminURL string, opts DiscoverOptions) ([]Tool, error) {
	svcs, err := fetchServices(ctx, adminURL)
	if err != nil {
		return nil, err
	}
	deny := map[string]bool{agentObjectService: true, agentToolsService: true}
	for _, d := range opts.Deny {
		deny[d] = true
	}
	allow := map[string]bool{}
	for _, a := range opts.Allow {
		allow[a] = true
	}

	var tools []Tool
	for _, s := range svcs {
		if deny[s.Name] || (len(allow) > 0 && !allow[s.Name]) {
			continue
		}
		keyed := s.keyed()
		for _, h := range s.Handlers {
			tools = append(tools, discoveredTool(s.Name, h, keyed))
		}
	}
	return tools, nil
}

// DiscoverOptions filters which services become tools. Allow (if non-empty) is a
// whitelist of service names; Deny is a blacklist. The agent's own services are always
// skipped.
type DiscoverOptions struct {
	Allow []string
	Deny  []string
}

const agentObjectService = "Agent"

// --- Admin API client --------------------------------------------------------

type adminService struct {
	Name     string         `json:"name"`
	Ty       string         `json:"ty"` // "Service" | "VirtualObject" | "Workflow"
	Handlers []adminHandler `json:"handlers"`
}

// keyed reports whether calls to this service must carry a key (Virtual Objects and
// Workflows are keyed; plain Services are not).
func (s adminService) keyed() bool {
	t := strings.ToLower(s.Ty)
	return strings.Contains(t, "object") || strings.Contains(t, "workflow")
}

type adminHandler struct {
	Name             string          `json:"name"`
	Documentation    string          `json:"documentation"`
	InputJSONSchema  json.RawMessage `json:"input_json_schema"`
	OutputJSONSchema json.RawMessage `json:"output_json_schema"`
}

// fetchServices calls GET {adminURL}/services and returns the registered services.
func fetchServices(ctx context.Context, adminURL string) ([]adminService, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := strings.TrimRight(adminURL, "/") + "/services"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("admin discovery: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admin discovery GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("admin discovery: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin discovery GET %s: HTTP %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	// The Admin API returns {"services":[...]}; tolerate a bare array too.
	var wrapped struct {
		Services []adminService `json:"services"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Services != nil {
		return wrapped.Services, nil
	}
	var bare []adminService
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("admin discovery: unexpected response shape")
	}
	return bare, nil
}

// --- Tool construction -------------------------------------------------------

// discoveredTool builds a leaf tool that durably calls one handler. A plain Service
// handler takes the handler's input directly; a keyed (Virtual Object / Workflow)
// handler takes {key, input}. In-package so it can set the unexported submit.
func discoveredTool(service string, h adminHandler, keyed bool) Tool {
	name := sanitizeName(service + "_" + h.Name)
	params := h.InputJSONSchema
	if keyed {
		params = keyedParams(h.InputJSONSchema)
	}
	t := Tool{
		Name:        name,
		Description: handlerDescription(service, h, keyed),
		Params:      params,
		Result:      h.OutputJSONSchema,
	}
	if keyed {
		t.submit = func(ctx restate.Context, raw json.RawMessage) (anyFuture, error) {
			var a struct {
				Key   string          `json:"key"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, restate.TerminalErrorf("bad args for %q: %v", name, err)
			}
			if strings.TrimSpace(a.Key) == "" {
				return nil, restate.TerminalErrorf("%q is a keyed handler; provide a non-empty \"key\"", name)
			}
			return CallObject[json.RawMessage](ctx, service, a.Key, h.Name, a.Input), nil
		}
	} else {
		hName := h.Name
		t.submit = func(ctx restate.Context, raw json.RawMessage) (anyFuture, error) {
			return Call[json.RawMessage](ctx, service, hName, raw), nil
		}
	}
	return t
}

// keyedParams wraps a handler's input schema so the tool takes {key, input}.
func keyedParams(input json.RawMessage) json.RawMessage {
	in := string(input)
	if strings.TrimSpace(in) == "" {
		in = "{}"
	}
	return json.RawMessage(`{"type":"object","properties":{` +
		`"key":{"type":"string","description":"the Virtual Object / Workflow key to invoke"},` +
		`"input":` + in + `},"required":["key"]}`)
}

func handlerDescription(service string, h adminHandler, keyed bool) string {
	var b strings.Builder
	if strings.TrimSpace(h.Documentation) != "" {
		b.WriteString(strings.TrimSpace(h.Documentation))
		b.WriteString(" ")
	}
	fmt.Fprintf(&b, "Durably call the %s/%s handler.", service, h.Name)
	if keyed {
		b.WriteString(` Keyed: pass {"key": <string>, "input": <handler input>}.`)
	}
	return b.String()
}

// sanitizeName maps a service/handler pair to a valid JS identifier (the tool is
// exposed as a global function), replacing any non-word char with '_'.
func sanitizeName(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
