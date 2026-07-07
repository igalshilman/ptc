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

// The developer's durable tools. Each becomes a plain async JS function the agent
// can call; the model can't tell a leaf tool from a seq tool.
//
//   - LEAF tools (agent.NewTool) return a durable Future from one non-blocking
//     submission (agent.Run / agent.Timer / agent.Call). A Promise.all over them
//     runs durably IN PARALLEL, in-process.
//   - SEQ tools (agent.NewSeqTool) are ordinary blocking, multi-step handlers with
//     the full restate.Context; they run in their own sub-invocation, so they may
//     orchestrate freely and still parallelize with siblings.

type computeArgs struct {
	Expr string `json:"expr" jsonschema:"description=a simple arithmetic expression like \"6*7\" or \"42+1\""`
}
type computeResult struct {
	Value int `json:"value"`
}

// computeTool: a pure, no-network durable step — a leaf tool. A stand-in for a real
// capability.
func computeTool() agent.Tool {
	return agent.NewTool("compute", `evaluate a simple arithmetic expression; arg {"expr": string}`,
		func(ctx restate.Context, a computeArgs) (agent.Future[computeResult], error) {
			return agent.Run(ctx, func(_ restate.RunContext) (computeResult, error) {
				return computeResult{Value: evalSimple(a.Expr)}, nil
			}, restate.WithName("compute")), nil
		})
}

type httpGetArgs struct {
	URL string `json:"url" jsonschema:"description=the absolute URL to GET"`
}
type httpGetResult struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// httpGetTool: an HTTP GET as a leaf tool, so several http_get calls in a single
// Promise.all run durably in PARALLEL; the response is journaled and not re-fetched
// on replay.
func httpGetTool() agent.Tool {
	return agent.NewTool("http_get", "fetch a URL and return its HTTP status and body",
		func(ctx restate.Context, a httpGetArgs) (agent.Future[httpGetResult], error) {
			return agent.Run(ctx, func(rc restate.RunContext) (httpGetResult, error) {
				return doGet(rc, a.URL)
			}, restate.WithName("http_get")), nil
		})
}

type waitArgs struct {
	Seconds float64 `json:"seconds" jsonschema:"description=how long to pause, in seconds"`
}
type waitResult struct {
	WaitedSeconds float64 `json:"waited_seconds"`
}

// waitTool: a durable timer as a leaf tool. agent.Timer wraps restate.After (a
// NON-blocking timer future), so waits fired via Promise.all run concurrently with
// each other and with other tools — no longer forced serial.
func waitTool() agent.Tool {
	return agent.NewTool("wait", "pause for a number of seconds, then continue",
		func(ctx restate.Context, a waitArgs) (agent.Future[waitResult], error) {
			return agent.Timer(ctx, dur(a.Seconds), waitResult{WaitedSeconds: a.Seconds}, restate.WithName("wait")), nil
		})
}

type delayedFetchArgs struct {
	URL          string  `json:"url" jsonschema:"description=the absolute URL to GET"`
	DelaySeconds float64 `json:"delay_seconds" jsonschema:"description=seconds to wait before fetching"`
}

// delayedFetchTool: a SEQ tool — a two-step durable sequence (a durable timer, THEN
// a durable fetch that depends on it completing). It blocks between steps, which is
// exactly why it's a seq tool: it runs in its own sub-invocation, so blocking is
// fine AND it still parallelizes with sibling tools in a Promise.all batch.
func delayedFetchTool() agent.Tool {
	return agent.NewSeqTool("delayed_fetch", "wait delay_seconds, then HTTP GET the url (a durable two-step tool)",
		func(ctx restate.Context, a delayedFetchArgs) (httpGetResult, error) {
			if a.DelaySeconds > 0 {
				if err := restate.Sleep(ctx, dur(a.DelaySeconds), restate.WithName("delay")); err != nil {
					return httpGetResult{}, err
				}
			}
			res, err := restate.Run(ctx, func(rc restate.RunContext) (httpGetResult, error) {
				return doGet(rc, a.URL)
			}, restate.WithName("fetch"))
			if err != nil {
				return httpGetResult{}, err
			}
			return res, nil
		})
}

// doGet performs the actual HTTP GET, shared by http_get and delayed_fetch. It runs
// inside a durable step (RunContext), so a transient network error is retried and a
// success is journaled.
func doGet(rc restate.RunContext, url string) (httpGetResult, error) {
	req, err := http.NewRequestWithContext(rc, http.MethodGet, url, nil)
	if err != nil {
		return httpGetResult{}, restate.TerminalErrorf("bad url: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return httpGetResult{}, err // transient: retry
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return httpGetResult{Status: resp.StatusCode, Body: string(body)}, nil
}

func dur(seconds float64) time.Duration { return time.Duration(seconds * float64(time.Second)) }

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
