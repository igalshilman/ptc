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

	restate "github.com/restatedev/sdk-go"

	"restatedev/agent"
)

// The developer's durable tools for a Wikipedia RESEARCH agent — two tools, each a
// single durable side effect (an HTTP GET, journaled) returning a Future. A Promise.all
// over several of them runs durably IN PARALLEL, in-process. The model does the
// multi-step reasoning itself (search → fetch → summarize in its answer), so there is
// no bundled "research" tool — an LLM-in-a-tool would just duplicate the agent.
//
// The data source is the public Wikipedia API — no API key, self-contained.

// ---- wiki_search ------------------------------------------------------------

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

// wikiSearchTool: a Wikipedia title search; several searches in a Promise.all run
// durably in PARALLEL and are journaled (not re-issued on replay).
func wikiSearchTool() agent.Tool {
	return agent.NewTool("wiki_search",
		`search Wikipedia and return the top matching article titles; use it for a quick lookup or to find the exact title to pass to wiki_fetch. Returns {query, results:[{title, description, url}]}`,
		func(ctx restate.Context, a wikiSearchArgs) (agent.Future[wikiSearchResult], error) {
			return agent.Run(ctx, func(rc restate.RunContext) (wikiSearchResult, error) {
				return wikiSearch(rc, a.Query, a.Limit)
			}, restate.WithName("wiki_search")), nil
		})
}

// ---- wiki_fetch -------------------------------------------------------------

type wikiFetchArgs struct {
	Title string `json:"title" jsonschema:"description=the exact article title to read, as returned by wiki_search"`
}
type wikiPage struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Extract string `json:"extract"`
	Found   bool   `json:"found"`
}

// wikiFetchTool: fetch one article's plain-text extract.
func wikiFetchTool() agent.Tool {
	return agent.NewTool("wiki_fetch",
		`fetch the plain-text of a Wikipedia article by its EXACT title (use a title from wiki_search). Returns {title, url, extract, found}; the extract is truncated for long articles`,
		func(ctx restate.Context, a wikiFetchArgs) (agent.Future[wikiPage], error) {
			return agent.Run(ctx, func(rc restate.RunContext) (wikiPage, error) {
				return wikiFetch(rc, a.Title)
			}, restate.WithName("wiki_fetch")), nil
		})
}

// ---- Wikipedia API client ---------------------------------------------------

const (
	wikiAPI       = "https://en.wikipedia.org/w/api.php"
	wikiUserAgent = "quickjs-worker-go-demo/1.0 (https://github.com/restatedev; a durable CodeAct agent example)"
)

// wikiGet performs a Wikipedia API GET with the etiquette-required User-Agent. It runs
// inside a durable step (RunContext), so a transient error is retried and a success is
// journaled. 5xx/429 are transient (retry); other non-200s are terminal.
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
