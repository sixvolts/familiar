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

// Client calls the Brave Search API.
type Client struct {
	apiKey     string
	maxResults int
	baseURL    string
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
		client:     &http.Client{Timeout: 5 * time.Second},
	}
}

// SetBaseURL overrides the API endpoint. Intended for tests that point
// the client at an httptest server.
func (b *Client) SetBaseURL(u string) { b.baseURL = u }

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
