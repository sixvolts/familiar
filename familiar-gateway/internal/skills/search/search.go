// Package search provides the first concrete Familiar skill: a thin
// adapter around internal/brave that exposes web search through the
// skills.Skill interface.
//
// This is deliberately a wrapper, not a reimplementation — the Brave
// client lives in internal/brave so that the pre-execution orchestrator
// (internal/prefetch) and the LLM-driven skill registry can share the
// same underlying HTTP plumbing.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/familiar/gateway/internal/brave"
	"github.com/familiar/gateway/internal/skills"
)

// Skill exposes Brave web search as the `web_search` tool.
type Skill struct {
	brave *brave.Client
}

// New constructs a search skill from an existing Brave client. The client
// must be non-nil — callers that want to omit the skill entirely should
// simply not register it.
func New(b *brave.Client) *Skill {
	return &Skill{brave: b}
}

// webSearchParams is the JSON schema for the `web_search` tool's params.
//
// Kept as a literal raw message rather than generating from a Go struct
// so the schema reads naturally and we can tune descriptions without
// touching the unmarshal path.
var webSearchParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "The search query. Be specific; Brave favors keyword-like queries over natural language."
    },
    "count": {
      "type": "integer",
      "description": "Maximum number of results to return (1-10). Defaults to the client's configured max.",
      "minimum": 1,
      "maximum": 10
    }
  },
  "required": ["query"]
}`)

func (s *Skill) Name() string        { return "search" }
func (s *Skill) Description() string { return "Web search via the Brave Search API" }
func (s *Skill) Version() string     { return "1.0.0" }

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name:        "web_search",
			Description: "Search the web for current information. Each result includes a title, URL, and several passages drawn from the page (typically a primary description plus 2-4 extra snippets). The passages are usually enough to answer detail questions without a second fetch — read them before deciding to call again with a refined query.",
			Parameters:  webSearchParams,
		},
	}
}

func (s *Skill) Init(_ json.RawMessage) error {
	if s.brave == nil {
		return fmt.Errorf("search: nil brave client")
	}
	return nil
}

func (s *Skill) Close() error { return nil }

// webSearchArgs is the typed form of webSearchParams. `count` is a
// pointer so we can tell "unset" from "zero" without a sentinel.
type webSearchArgs struct {
	Query string `json:"query"`
	Count *int   `json:"count,omitempty"`
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	if toolName != "web_search" {
		return skills.ToolResult{}, fmt.Errorf("search: unknown tool %q", toolName)
	}

	var args webSearchArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	if args.Query == "" {
		return skills.ToolResult{Error: "query is required"}, nil
	}

	results, err := s.brave.Search(ctx, args.Query)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("search: brave query: %w", err)
	}

	if args.Count != nil && *args.Count > 0 && *args.Count < len(results) {
		results = results[:*args.Count]
	}

	content := formatResults(args.Query, results)
	data, _ := json.Marshal(results)
	return skills.ToolResult{
		Content: content,
		Data:    data,
		Tokens:  len(content) / 4,
	}, nil
}

// snippetCharCap is the per-snippet truncation. Brave's extra
// snippets are typically 150-300 chars; we allow up to 600 to handle
// the occasional longer passage without letting a single result
// dominate the context budget.
const snippetCharCap = 600

// formatResults builds an LLM-friendly multi-line block. Each result
// is numbered and renders title + url + age on the header line, then
// the primary description, then any extra snippets as bullet points.
// Multi-line output (vs the previous one-liner) gives the model
// enough context to answer detail questions without re-querying.
func formatResults(query string, results []brave.SearchResult) string {
	if len(results) == 0 {
		return fmt.Sprintf("No results for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n", query)
	for i, r := range results {
		age := ""
		if r.Age != "" {
			age = " · " + r.Age
		}
		fmt.Fprintf(&b, "\n%d. %s\n   %s%s\n", i+1, r.Title, r.URL, age)
		if d := truncateSnippet(r.Description); d != "" {
			fmt.Fprintf(&b, "   %s\n", d)
		}
		for _, snip := range r.ExtraSnippets {
			if s := truncateSnippet(snip); s != "" {
				fmt.Fprintf(&b, "   • %s\n", s)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncateSnippet trims and clamps one snippet so a single bullet
// can't blow the context. Empty / whitespace-only snippets return
// "" so the caller can skip them entirely.
func truncateSnippet(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= snippetCharCap {
		return s
	}
	return string(runes[:snippetCharCap-1]) + "…"
}
