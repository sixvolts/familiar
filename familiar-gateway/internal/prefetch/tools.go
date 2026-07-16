// Package prefetch runs side-information retrieval *before* the main LLM
// call, synchronously, and injects the results into the system prompt.
// This is distinct from internal/skills, which exposes tools the LLM can
// choose to call mid-turn via function calling:
//
//   - prefetch: deterministic, gated by the router's tool selection, always
//     fires before the model sees the user message. Used when we know up
//     front the model will need fresh web results (e.g. "what's the latest
//     news on X") and don't want to pay a second round-trip.
//   - skills: model-driven, fires only when the model asks for it during
//     tool-use loops. Preferred when the need for a tool depends on the
//     model's reasoning about the user message.
//
// Both paths share the same internal/brave client.
package prefetch

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/familiar/gateway/internal/brave"
)

// Orchestrator manages available tools and executes them based on router decisions.
type Orchestrator struct {
	brave *brave.Client // nil if not configured
}

// NewOrchestrator creates a prefetch orchestrator. b may be nil if unconfigured.
func NewOrchestrator(b *brave.Client) *Orchestrator {
	return &Orchestrator{brave: b}
}

// ExecuteResult holds tool output and metadata for status display.
type ExecuteResult struct {
	Context     string   // formatted context for system prompt injection
	Queries     []string // search queries that were executed
	ResultCount int      // total number of results returned
}

// Execute runs requested tools concurrently and returns formatted context with metadata.
// Returns an ExecuteResult; Context will be empty if no tools were needed/available.
// complexity controls the result budget: "trivial"/"knowledge" (sidecar targets)
// get a tighter budget to fit the smaller context window; "analytical"/
// "deep_reasoning" (gpu-host targets) get more room.
func (o *Orchestrator) Execute(ctx context.Context, toolsNeeded []string, searchQueries []string, complexity string) ExecuteResult {
	if len(toolsNeeded) == 0 {
		return ExecuteResult{}
	}

	// Budget: sidecar-bound tiers get fewer results and shorter snippets.
	maxTotalResults := 6
	descLen := 300
	switch complexity {
	case "trivial", "knowledge":
		maxTotalResults = 3
		descLen = 160
	}

	var results []brave.SearchResult
	var queriesUsed []string
	for _, tool := range toolsNeeded {
		switch tool {
		case "web_search":
			// Cap queries
			q := searchQueries
			if len(q) > 3 {
				q = q[:3]
			}
			queriesUsed = q
			results = o.executeWebSearch(ctx, q)
		default:
			// Unknown tool — ignore silently.
		}
	}

	if len(results) == 0 {
		return ExecuteResult{Queries: queriesUsed}
	}

	if len(results) > maxTotalResults {
		results = results[:maxTotalResults]
	}

	return ExecuteResult{
		Context:     formatSearchResults(results, descLen),
		Queries:     queriesUsed,
		ResultCount: len(results),
	}
}

// executeWebSearch runs Brave queries concurrently and aggregates results.
func (o *Orchestrator) executeWebSearch(ctx context.Context, queries []string) []brave.SearchResult {
	if o.brave == nil {
		return nil
	}
	if len(queries) == 0 {
		return nil
	}

	// Hard cap: max 3 queries.
	if len(queries) > 3 {
		queries = queries[:3]
	}

	type queryResult struct {
		results []brave.SearchResult
	}

	resultsCh := make([]queryResult, len(queries))
	var wg sync.WaitGroup

	for i, q := range queries {
		wg.Add(1)
		go func(idx int, query string) {
			defer wg.Done()
			res, err := o.brave.Search(ctx, query)
			if err != nil {
				log.Printf("[prefetch] brave search error for %q: %v", query, err)
				return
			}
			resultsCh[idx] = queryResult{results: res}
		}(i, q)
	}

	wg.Wait()

	var all []brave.SearchResult
	for _, qr := range resultsCh {
		all = append(all, qr.results...)
	}

	return all
}

// formatSearchResults builds the context string injected into the system prompt.
// descLen truncates each result's description to keep the injected block compact
// for models with small context windows.
func formatSearchResults(results []brave.SearchResult, descLen int) string {

	var b strings.Builder
	b.WriteString("Web search results (synthesize into a natural answer, cite inline):\n")
	for _, r := range results {
		age := r.Age
		if age != "" {
			age = " (" + age + ")"
		}
		desc := r.Description
		if descLen > 0 && len(desc) > descLen {
			desc = desc[:descLen-3] + "..."
		}
		fmt.Fprintf(&b, "- [%s](%s) — %s%s\n", r.Title, r.URL, desc, age)
	}
	return b.String()
}
