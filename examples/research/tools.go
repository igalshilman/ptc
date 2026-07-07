package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

// The developer's durable tools for a Wikipedia RESEARCH agent. Each becomes a plain
// async JS function the agent can call; the model can't tell a leaf tool from a seq
// tool. Two leaf primitives plus one multi-step seq tool:
//
//   - wiki_search / wiki_fetch — LEAF tools (agent.NewTool): each does ONE durable
//     side effect (an HTTP GET, journaled) and returns a Future. A Promise.all over
//     several of them runs durably IN PARALLEL, in-process.
//   - research_topic — a SEQ tool (agent.NewSeqTool): a data-dependent sequence
//     (search → pick best → fetch → LLM-summarize) with the full restate.Context. It
//     blocks between steps AND makes its own durable model call, which is exactly why
//     it's a seq tool: it runs in its own sub-invocation, so it may orchestrate freely
//     and still parallelize with sibling research_topic calls in a Promise.all batch.
//
// The data source is the public Wikipedia API — no API key, self-contained.

// ---- wiki_search (leaf) -----------------------------------------------------

type wikiSearchArgs struct {
	Query string `json:"query" jsonschema:"description=search terms, e.g. \"causes of the French Revolution\""`
	Limit int    `json:"limit,omitempty" jsonschema:"description=max results to return (default 5, max 10)"`
}
type wikiHit struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url"`
}
type wikiSearchResult struct {
	Query   string    `json:"query"`
	Results []wikiHit `json:"results"`
}

// wikiSearchTool: a Wikipedia title search as a leaf tool, so several searches in a
// Promise.all run durably in PARALLEL and are journaled (not re-issued on replay).
func wikiSearchTool() agent.Tool {
	return agent.NewTool("wiki_search",
		`search Wikipedia and return the top matching article titles; use it for a quick lookup or to find the exact title to pass to wiki_fetch. Returns {query, results:[{title, description, url}]}`,
		func(ctx restate.Context, a wikiSearchArgs) (agent.Future[wikiSearchResult], error) {
			return agent.Run(ctx, func(rc restate.RunContext) (wikiSearchResult, error) {
				return wikiSearch(rc, a.Query, a.Limit)
			}, restate.WithName("wiki_search")), nil
		})
}

// ---- wiki_fetch (leaf) ------------------------------------------------------

type wikiFetchArgs struct {
	Title string `json:"title" jsonschema:"description=the exact article title to read, as returned by wiki_search"`
}
type wikiPage struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Extract string `json:"extract"`
	Found   bool   `json:"found"`
}

// wikiFetchTool: fetch one article's plain-text extract as a leaf tool.
func wikiFetchTool() agent.Tool {
	return agent.NewTool("wiki_fetch",
		`fetch the plain-text of a Wikipedia article by its EXACT title (use a title from wiki_search). Returns {title, url, extract, found}; the extract is truncated for long articles`,
		func(ctx restate.Context, a wikiFetchArgs) (agent.Future[wikiPage], error) {
			return agent.Run(ctx, func(rc restate.RunContext) (wikiPage, error) {
				return wikiFetch(rc, a.Title)
			}, restate.WithName("wiki_fetch")), nil
		})
}

// ---- research_topic (seq) ---------------------------------------------------

type researchArgs struct {
	Subtopic string `json:"subtopic" jsonschema:"description=ONE focused subtopic to research, e.g. \"causes of the Russian Revolution\""`
	Focus    string `json:"focus,omitempty" jsonschema:"description=optional angle or question to focus the notes on"`
}
type researchNote struct {
	Subtopic string `json:"subtopic"`
	Source   string `json:"source"` // the Wikipedia article title actually read
	URL      string `json:"url"`
	Summary  string `json:"summary"` // concise, model-written notes drawn from the article
}

// researchTopicTool: the showcase SEQ tool. It runs a whole research sub-pipeline as
// its own durable sub-invocation — search, pick the best article, read it, then make
// a durable LLM call to distill notes. Every step is journaled, so on replay nothing
// re-runs (no re-fetch, no re-billing the summary). Because it's a seq tool, firing
// several via Promise.all researches multiple subtopics in PARALLEL, each in its own
// invocation. It captures the model client + model id from setup() — a tool is plain
// developer code and may use whatever it needs.
func researchTopicTool(client openai.Client, model string) agent.Tool {
	return agent.NewSeqTool("research_topic",
		`deeply research ONE focused subtopic end-to-end: it searches Wikipedia, picks the best article, reads it, and returns concise factual notes. A durable multi-step tool — fire SEVERAL in a single Promise.all to research subtopics in PARALLEL. Prefer this over wiki_search+wiki_fetch when you want ready-to-use notes. Returns {subtopic, source, url, summary}`,
		func(ctx restate.Context, a researchArgs) (researchNote, error) {
			if strings.TrimSpace(a.Subtopic) == "" {
				return researchNote{}, restate.TerminalErrorf("research_topic needs a non-empty subtopic")
			}

			// 1. Search Wikipedia (durable step).
			sr, err := restate.Run(ctx, func(rc restate.RunContext) (wikiSearchResult, error) {
				return wikiSearch(rc, a.Subtopic, 5)
			}, restate.WithName("search"))
			if err != nil {
				return researchNote{}, err
			}
			if len(sr.Results) == 0 {
				return researchNote{}, restate.TerminalErrorf("no Wikipedia results for %q", a.Subtopic)
			}

			// 2. Pick the best readable article: try the top hits until one fetches.
			var page wikiPage
			for i, hit := range sr.Results {
				if i >= 3 {
					break
				}
				title := hit.Title
				p, err := restate.Run(ctx, func(rc restate.RunContext) (wikiPage, error) {
					return wikiFetch(rc, title)
				}, restate.WithName(fmt.Sprintf("fetch-%d", i)))
				if err != nil {
					return researchNote{}, err
				}
				if p.Found {
					page = p
					break
				}
			}
			if !page.Found {
				return researchNote{}, restate.TerminalErrorf("no readable Wikipedia article found for %q", a.Subtopic)
			}

			// 3. Distill notes with a durable model call (journaled: not re-billed on replay).
			summary, err := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
				return summarize(rc, client, model, a.Subtopic, a.Focus, page.Extract)
			}, restate.WithName("summarize"))
			if err != nil {
				return researchNote{}, err
			}

			return researchNote{Subtopic: a.Subtopic, Source: page.Title, URL: page.URL, Summary: summary}, nil
		})
}

// summarize asks the model to distill factual notes on the subtopic from the article
// text. Runs inside a durable restate.Run (in research_topic), so the call is
// journaled — a replay returns the captured notes instead of calling OpenAI again.
// A cheaper model could be used here than the agent's planner; we reuse the same one.
func summarize(ctx context.Context, client openai.Client, model, subtopic, focus, text string) (string, error) {
	// Bound the model call so a stalled connection can't hang this sub-invocation
	// indefinitely (the parent Ask awaits it under the session lock). On timeout the
	// call returns a non-terminal error, so the durable step is retried.
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	const system = "You are a meticulous research assistant. Read the SOURCE TEXT and write concise, factual notes on the given subtopic as short bullet points. Use ONLY facts present in the source; never invent. If the source does not cover the subtopic, say so plainly."
	var b strings.Builder
	fmt.Fprintf(&b, "Subtopic: %s\n", subtopic)
	if strings.TrimSpace(focus) != "" {
		fmt.Fprintf(&b, "Focus the notes on: %s\n", focus)
	}
	fmt.Fprintf(&b, "\nSOURCE TEXT:\n%s", truncateRunes(text, 12000))

	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: openai.ChatModel(model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(b.String()),
		},
	})
	if err != nil {
		return "", err // transient: Restate retries the Run with backoff
	}
	if len(resp.Choices) == 0 {
		return "", restate.TerminalErrorf("summarizer returned no choices")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// ---- Wikipedia API client ---------------------------------------------------

const (
	wikiAPI       = "https://en.wikipedia.org/w/api.php"
	wikiUserAgent = "quickjs-worker-go-demo/1.0 (https://github.com/restatedev; a durable CodeAct agent example)"
)

// wikiGet performs a Wikipedia API GET with the etiquette-required User-Agent. It
// runs inside a durable step (RunContext), so a transient error is retried and a
// success is journaled. 5xx/429 are transient (retry); other non-200s are terminal.
func wikiGet(ctx context.Context, params url.Values) ([]byte, error) {
	// Bound each attempt: nothing else bounds a tool call's wall-clock, so without this
	// a stalled / half-open connection would hang the invocation and hold the session
	// lock indefinitely. A deadline also cooperates with Restate cancellation; on
	// timeout Do returns a non-terminal error, so the durable step is retried.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wikiAPI+"?"+params.Encode(), nil)
	if err != nil {
		return nil, restate.TerminalErrorf("bad request: %v", err)
	}
	req.Header.Set("User-Agent", wikiUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err // transient (incl. deadline exceeded): retry
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("wikipedia returned HTTP %d (transient)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, restate.TerminalErrorf("wikipedia returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		// A mid-transfer read failure (connection reset / timeout after a 200) is
		// transient — return a plain error so Restate retries, instead of letting a
		// truncated body fall through to a terminal JSON-parse error downstream.
		return nil, fmt.Errorf("reading wikipedia response body: %w", err)
	}
	return body, nil
}

// wikiSearch runs an opensearch query. The opensearch response is a heterogeneous
// array [query, [titles], [descriptions], [urls]] — parsed positionally.
func wikiSearch(ctx context.Context, query string, limit int) (wikiSearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return wikiSearchResult{}, restate.TerminalErrorf("wiki_search needs a non-empty query")
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	body, err := wikiGet(ctx, url.Values{
		"action":    {"opensearch"},
		"format":    {"json"},
		"namespace": {"0"},
		"limit":     {strconv.Itoa(limit)},
		"search":    {query},
	})
	if err != nil {
		return wikiSearchResult{}, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) < 4 {
		return wikiSearchResult{}, restate.TerminalErrorf("unexpected opensearch response")
	}
	var titles, descs, urls []string
	_ = json.Unmarshal(raw[1], &titles)
	_ = json.Unmarshal(raw[2], &descs)
	_ = json.Unmarshal(raw[3], &urls)

	// Initialize Results so a zero-hit query serializes to [] (not null), matching the
	// advertised {results:[...]} schema — else model code like r.results.map(...) throws.
	res := wikiSearchResult{Query: query, Results: []wikiHit{}}
	for i, t := range titles {
		hit := wikiHit{Title: t}
		if i < len(descs) {
			hit.Description = descs[i]
		}
		if i < len(urls) {
			hit.URL = urls[i]
		}
		res.Results = append(res.Results, hit)
	}
	return res, nil
}

// wikiFetch retrieves an article's plain-text extract by exact title (following
// redirects). A missing page returns {found:false} (not an error) so the caller can
// try another title.
func wikiFetch(ctx context.Context, title string) (wikiPage, error) {
	if strings.TrimSpace(title) == "" {
		return wikiPage{}, restate.TerminalErrorf("wiki_fetch needs a non-empty title")
	}
	body, err := wikiGet(ctx, url.Values{
		"action":      {"query"},
		"format":      {"json"},
		"prop":        {"extracts"},
		"explaintext": {"1"},
		"redirects":   {"1"},
		"titles":      {title},
	})
	if err != nil {
		return wikiPage{}, err
	}
	var parsed struct {
		Query struct {
			Pages map[string]struct {
				Title   string          `json:"title"`
				Extract string          `json:"extract"`
				Missing json.RawMessage `json:"missing"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return wikiPage{}, restate.TerminalErrorf("unexpected extract response")
	}
	// A single title yields a single page (map keyed by page id, or "-1" if missing).
	for _, p := range parsed.Query.Pages {
		if p.Missing != nil || strings.TrimSpace(p.Extract) == "" {
			return wikiPage{Title: title, Found: false}, nil
		}
		return wikiPage{
			Title:   p.Title,
			URL:     "https://en.wikipedia.org/wiki/" + strings.ReplaceAll(p.Title, " ", "_"),
			Extract: truncateRunes(p.Extract, 15000),
			Found:   true,
		}, nil
	}
	return wikiPage{Title: title, Found: false}, nil
}

// truncateRunes bounds a string to max runes (not bytes), so multibyte text is never
// cut mid-rune, noting that it was clipped.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n… [truncated]"
}
