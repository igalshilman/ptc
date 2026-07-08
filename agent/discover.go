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

// DiscoverConfig configures Admin-API handler discovery: turn EVERY registered
// handler into a leaf tool the agent can call, so the model can orchestrate an
// existing deployment's durable handlers (in parallel, durably) via generated code.
type DiscoverConfig struct {
	AdminURL string   // Restate Admin API base URL (default http://localhost:9070)
	Allow    []string // if non-empty, only these service names become tools
	Deny     []string // service names to skip (Agent/AgentTools are always skipped)
}

const (
	agentObjectService = "Agent"
	defaultAdminURL    = "http://localhost:9070"
)

// DiscoverTools fetches the handlers now and builds their tools in one call — for
// STARTUP discovery of services that are ALREADY registered. For a standalone
// deployment (where the target services register alongside the agent, so they aren't
// visible until after startup), use Config.Discover, which discovers lazily per Ask
// and journals the result for replay-safety.
func DiscoverTools(ctx context.Context, cfg DiscoverConfig) ([]Tool, error) {
	ds, err := fetchHandlers(ctx, cfg)
	if err != nil {
		return nil, err
	}
	tools := make([]Tool, len(ds))
	for i, d := range ds {
		tools[i] = toolFromDescriptor(d)
	}
	return tools, nil
}

// handlerDescriptor is the JSON-serializable discovery result for one handler. It is
// what Ask JOURNALS (via restate.Run), so the tool set is rebuilt IDENTICALLY on
// replay — discovery is a non-deterministic external call that shapes the prompt and
// the callable tools, so it must be captured once and replayed.
type handlerDescriptor struct {
	Service string          `json:"service"`
	Handler string          `json:"handler"`
	Keyed   bool            `json:"keyed"`
	Doc     string          `json:"doc,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// fetchHandlers queries the Admin API and returns a descriptor per handler (after
// filtering). This is the non-deterministic part; keep it pure of Tool construction
// so callers can journal the descriptors.
func fetchHandlers(ctx context.Context, cfg DiscoverConfig) ([]handlerDescriptor, error) {
	svcs, err := fetchServices(ctx, cfg.AdminURL)
	if err != nil {
		return nil, err
	}
	deny := map[string]bool{agentObjectService: true, agentToolsService: true}
	for _, d := range cfg.Deny {
		deny[d] = true
	}
	allow := map[string]bool{}
	for _, a := range cfg.Allow {
		allow[a] = true
	}
	var out []handlerDescriptor
	for _, s := range svcs {
		if deny[s.Name] || (len(allow) > 0 && !allow[s.Name]) {
			continue
		}
		keyed := s.keyed()
		for _, h := range s.Handlers {
			out = append(out, handlerDescriptor{
				Service: s.Name,
				Handler: h.Name,
				Keyed:   keyed,
				Doc:     h.Documentation,
				Params:  h.InputJSONSchema,
				Result:  h.OutputJSONSchema,
			})
		}
	}
	return out, nil
}

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

func fetchServices(ctx context.Context, adminURL string) ([]adminService, error) {
	if strings.TrimSpace(adminURL) == "" {
		adminURL = defaultAdminURL
	}
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

// --- Tool construction (pure) ------------------------------------------------

// toolFromDescriptor builds a leaf tool that durably calls one handler. A plain
// Service handler takes the handler's input directly; a keyed (Virtual Object /
// Workflow) handler takes {key, input}. In-package so it can set the unexported
// submit; deterministic so it rebuilds identically on replay.
func toolFromDescriptor(d handlerDescriptor) Tool {
	name := sanitizeName(d.Service + "_" + d.Handler)
	params := d.Params
	if d.Keyed {
		params = keyedParams(d.Params)
	}
	service, handler, keyed := d.Service, d.Handler, d.Keyed
	t := Tool{
		Name:        name,
		Description: descriptorDescription(d),
		Params:      params,
		Result:      d.Result,
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
			return CallObject[json.RawMessage](ctx, service, a.Key, handler, a.Input), nil
		}
	} else {
		t.submit = func(ctx restate.Context, raw json.RawMessage) (anyFuture, error) {
			return Call[json.RawMessage](ctx, service, handler, raw), nil
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

func descriptorDescription(d handlerDescriptor) string {
	var b strings.Builder
	if strings.TrimSpace(d.Doc) != "" {
		b.WriteString(strings.TrimSpace(d.Doc))
		b.WriteString(" ")
	}
	fmt.Fprintf(&b, "Durably call the %s/%s handler.", d.Service, d.Handler)
	if d.Keyed {
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
