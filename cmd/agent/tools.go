package main

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"quickjsworker/agent"
)

// The developer's durable tools. Each becomes a plain async JS function the
// agent can call. NewRunTool tools run in parallel under Promise.all; NewTool
// tools hold the full restate.Context (timers, service calls, state).

type computeArgs struct {
	Expr string `json:"expr" jsonschema:"description=a simple arithmetic expression like \"6*7\" or \"42+1\""`
}

// computeTool: a pure, no-network durable step. A stand-in for a real capability.
func computeTool() agent.Tool {
	return agent.NewRunTool("compute", `evaluate a simple arithmetic expression; arg {"expr": string}`,
		func(_ restate.RunContext, a computeArgs) (any, error) {
			return map[string]any{"value": evalSimple(a.Expr)}, nil
		})
}

type httpGetArgs struct {
	URL string `json:"url" jsonschema:"description=the absolute URL to GET"`
}

// httpGetTool: an HTTP GET. Built with NewRunTool so several http_get calls in a
// single Promise.all run durably in PARALLEL; the response is journaled and not
// re-fetched on replay.
func httpGetTool() agent.Tool {
	return agent.NewRunTool("http_get", "fetch a URL and return its HTTP status and body",
		func(rc restate.RunContext, a httpGetArgs) (any, error) {
			req, err := http.NewRequestWithContext(rc, http.MethodGet, a.URL, nil)
			if err != nil {
				return nil, restate.TerminalErrorf("bad url: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err // transient: retry
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return map[string]any{"status": resp.StatusCode, "body": string(body)}, nil
		})
}

type waitArgs struct {
	Seconds float64 `json:"seconds" jsonschema:"description=how long to pause, in seconds"`
}

// waitTool: a durable timer. restate.Sleep survives crashes and resumes after
// the delay, so it must be a context tool (NewTool), not a run tool.
func waitTool() agent.Tool {
	return agent.NewTool("wait", "pause execution for a number of seconds",
		func(ctx restate.Context, a waitArgs) (any, error) {
			if err := restate.Sleep(ctx, time.Duration(a.Seconds*float64(time.Second)), restate.WithName("wait")); err != nil {
				return nil, err
			}
			return map[string]any{"waited_seconds": a.Seconds}, nil
		})
}

// evalSimple evaluates "a*b" or "a+b" for the compute tool.
func evalSimple(expr string) int {
	for _, op := range []byte{'*', '+'} {
		if i := strings.IndexByte(expr, op); i > 0 {
			a, _ := strconv.Atoi(strings.TrimSpace(expr[:i]))
			b, _ := strconv.Atoi(strings.TrimSpace(expr[i+1:]))
			if op == '*' {
				return a * b
			}
			return a + b
		}
	}
	n, _ := strconv.Atoi(strings.TrimSpace(expr))
	return n
}
