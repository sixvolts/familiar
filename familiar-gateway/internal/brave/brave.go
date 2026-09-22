// Package brave wraps the Brave Web Search API. It is the single shared
// HTTP client used by both pre-execution retrieval (internal/prefetch) and
// the LLM-driven search/news skills (internal/skills). Keeping this client
// in its own package avoids an import cycle between prefetch and skills.
package brave

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SearchResult holds a single web search result.
//
// Description is Brave's primary one-line snippet (~150-200 chars).
// ExtraSnippets are the additional passages Brave surfaces when the
// query asks for them (extra_snippets=true) — typically 2-4 strings,
// 150-300 chars each, drawn from different parts of the page. They
// give the LLM enough context to answer detail questions without a
// second tool call to fetch the page itself.
type SearchResult struct {
	Title         string
	URL           string
	Description   string
	Age           string // e.g. "2 days ago"
	ExtraSnippets []string
}

// defaultBaseURL is the production Brave Search endpoint. Tests override
// this with an httptest server via SetBaseURL.
const defaultBaseURL = "https://api.search.brave.com/res/v1/web/search"

// defaultContextURL is Brave's LLM-context endpoint. It returns the text
// Brave has ALREADY extracted from several relevant pages, keyed off a
// topic rather than a URL — the middle ground between web_search (ranked
// titles + snippets, no body) and fetching a single page live.
//
// Tests override it with SetContextURL, mirroring SetBaseURL.
const defaultContextURL = "https://api.search.brave.com/res/v1/llm/context"

// ContextSource is one page's worth of Brave-extracted content.
//
// Snippets are longer passages than SearchResult.ExtraSnippets — this
// endpoint is built for grounding, so it returns several paragraphs per
// source rather than SERP-sized fragments. Deduplicated on the way in:
// Brave sometimes repeats a passage across grounding categories.
type ContextSource struct {
	Title    string
	URL      string
	Snippets []string
}

// Client calls the Brave Search API.
type Client struct {
	apiKey     string
	maxResults int
	baseURL    string
	contextURL string
	client     *http.Client
}

// New creates a Brave Search client.
// maxResults controls how many results per query (capped at 10).
func New(apiKey string, maxResults int) *Client {
	if maxResults <= 0 {
		maxResults = 3
	}
	if maxResults > 10 {
		maxResults = 10
	}
	return &Client{
		apiKey:     apiKey,
		maxResults: maxResults,
		baseURL:    defaultBaseURL,
		contextURL: defaultContextURL,
		// The context endpoint returns page bodies rather than snippets, so
		// it is slower than Search; it gets its own longer deadline at the
		// call site rather than widening this shared 5s timeout.
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// SetBaseURL overrides the API endpoint. Intended for tests that point
// the client at an httptest server.
func (b *Client) SetBaseURL(u string) { b.baseURL = u }

// SetContextURL overrides the LLM-context endpoint, for tests.
func (b *Client) SetContextURL(u string) { b.contextURL = u }

// Search queries the Brave Web Search API and returns results.
// Returns empty results on error — tool results are optional context enrichment.
//
// extra_snippets=true asks Brave to include 2-4 longer passages per
// result alongside the default one-line description. Available on
// all paid Brave tiers; the free tier ignores the param and returns
// only the description, so the upgrade is safe across plans.
//
// result_filter=web narrows to web results only (no infobox, news,
// videos, FAQ blocks) so the response shape stays small and the
// snippets we render are all page-text rather than mixed with
// SERP card content.
func (b *Client) Search(ctx context.Context, query string) ([]SearchResult, error) {
	reqURL := fmt.Sprintf("%s?q=%s&count=%d&extra_snippets=true&result_filter=web",
		b.baseURL, url.QueryEscape(query), b.maxResults)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building brave request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("brave HTTP %d: %s", resp.StatusCode, truncateBytes(body, 200))
	}

	var braveResp struct {
		Web struct {
			Results []struct {
				Title         string   `json:"title"`
				URL           string   `json:"url"`
				Description   string   `json:"description"`
				Age           string   `json:"age"`
				ExtraSnippets []string `json:"extra_snippets"`
			} `json:"results"`
		} `json:"web"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&braveResp); err != nil {
		return nil, fmt.Errorf("parsing brave response: %w", err)
	}

	var results []SearchResult
	for _, r := range braveResp.Web.Results {
		results = append(results, SearchResult{
			Title:         r.Title,
			URL:           r.URL,
			Description:   r.Description,
			Age:           r.Age,
			ExtraSnippets: r.ExtraSnippets,
		})
	}

	// Total extra-snippet count is the empirical signal that the
	// Brave plan is honoring extra_snippets=true. Free-tier
	// subscriptions ignore the param and return 0 here; paid tiers
	// typically return 2-4 per result. If you see results>0 and
	// extra_snippets=0 over multiple queries, your subscription
	// isn't entitled to the longer passages.
	extraTotal := 0
	for _, r := range results {
		extraTotal += len(r.ExtraSnippets)
	}
	log.Printf("[brave] query=%q results=%d extra_snippets=%d", query, len(results), extraTotal)
	return results, nil
}

func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// Context queries Brave's LLM-context endpoint and returns the extracted
// page content for a topic.
//
// This sits between Search and fetching a page: Search gives ranked
// titles with snippets but never a body, while a live fetch gives one
// page you must name. Context gives the bodies of several relevant pages
// in a single call, which is what grounding an answer actually needs.
//
// Its caveats matter and are surfaced in the tool description: you
// describe a topic and cannot choose which pages come back, coverage is
// limited to pages Brave has indexed, and the text is Brave's stored
// extraction — so it can be stale and is usually partial rather than the
// whole page.
//
// count is clamped to 1-10 (the endpoint's range, narrower than
// Search's 20). freshness accepts pd/pw/pm/py and is passed through
// untouched; an unrecognised value is Brave's problem to reject rather
// than something to silently drop.
//
// Unlike Search, this returns the error rather than swallowing it: a
// caller asking for page content has no useful degraded mode, whereas
// Search's results are optional context enrichment.
func (b *Client) Context(ctx context.Context, query string, count int, freshness string) ([]ContextSource, error) {
	if query == "" {
		return nil, fmt.Errorf("brave: empty query")
	}
	if count <= 0 {
		count = b.maxResults
	}
	if count > 10 {
		count = 10
	}

	q := url.Values{}
	q.Set("q", query)
	q.Set("count", fmt.Sprintf("%d", count))
	if freshness != "" {
		q.Set("freshness", freshness)
	}
	reqURL := b.contextURL + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("brave context: build request: %w", err)
	}
	req.Header.Set("X-Subscription-Token", b.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave context: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("brave context: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Map the codes worth distinguishing; the rest carry the status.
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return nil, fmt.Errorf("brave context: token rejected (401)")
		case http.StatusUnprocessableEntity:
			return nil, fmt.Errorf("brave context: unprocessable query (422)")
		case http.StatusTooManyRequests:
			return nil, fmt.Errorf("brave context: rate limited by Brave (429)")
		default:
			return nil, fmt.Errorf("brave context: HTTP %d: %s",
				resp.StatusCode, truncateBytes(body, 200))
		}
	}

	// The payload groups sources under arbitrary category keys, so flatten
	// every category rather than assuming a fixed set.
	var parsed struct {
		Grounding map[string][]struct {
			Title    string   `json:"title"`
			URL      string   `json:"url"`
			Snippets []string `json:"snippets"`
		} `json:"grounding"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("brave context: decode: %w", err)
	}

	var out []ContextSource
	for _, cat := range parsed.Grounding {
		for _, it := range cat {
			seen := make(map[string]struct{}, len(it.Snippets))
			var sn []string
			for _, s := range it.Snippets {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				if _, dup := seen[s]; dup {
					continue
				}
				seen[s] = struct{}{}
				sn = append(sn, s)
			}
			out = append(out, ContextSource{
				Title:    strings.TrimSpace(it.Title),
				URL:      strings.TrimSpace(it.URL),
				Snippets: sn,
			})
		}
	}
	if len(out) > count {
		out = out[:count]
	}
	return out, nil
}
