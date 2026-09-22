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

// pageReadParams is the JSON schema for `brave_page_read`.
//
// max_chars exists because this endpoint returns page BODIES, not
// snippets: without a cap a five-source call can swallow a large share of
// the turn's tool budget on its own. The default is deliberately modest.
var pageReadParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "The topic you want page content about. You describe a subject, not a URL — Brave chooses which indexed pages to return."
    },
    "count": {
      "type": "integer",
      "description": "How many sources to return (1-10). Defaults to 5. Each source costs real context, so ask for what you need.",
      "minimum": 1,
      "maximum": 10
    },
    "freshness": {
      "type": "string",
      "description": "Restrict by age: pd (past day), pw (week), pm (month), py (year). Omit for no restriction.",
      "enum": ["pd", "pw", "pm", "py"]
    },
    "max_chars": {
      "type": "integer",
      "description": "Cap on total returned characters (1000-30000). Defaults to 6000. Raise it only when the answer genuinely needs more.",
      "minimum": 1000,
      "maximum": 30000
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
		{
			Name: "brave_page_read",
			Description: "Get page CONTENT for a topic in one call: Brave returns the text it has already extracted from several relevant pages, with their URLs. This is the middle ground between web_search (ranked titles and snippets, never a page body) and fetching one page live. Use it to ground an answer in sources, or when reading pages one at a time would be too slow.\n" +
				"Caveats worth respecting: you describe a topic and cannot choose which pages come back; coverage is limited to pages Brave has indexed; and the text is Brave's stored extraction, so it can be out of date and is usually partial rather than the whole page. When you need one specific URL, or the live version of a page, fetch that page instead.",
			Parameters: pageReadParams,
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

// pageReadArgs is the typed form of pageReadParams.
type pageReadArgs struct {
	Query     string `json:"query"`
	Count     *int   `json:"count,omitempty"`
	Freshness string `json:"freshness,omitempty"`
	MaxChars  *int   `json:"max_chars,omitempty"`
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	if toolName == "brave_page_read" {
		return s.executePageRead(ctx, params)
	}
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

// defaultPageReadChars is the character budget when the caller does not
// set max_chars. Kept modest on purpose: this endpoint returns page
// bodies, and the per-turn tool budget is shared with every other call
// the model makes.
const defaultPageReadChars = 6000

// executePageRead handles the brave_page_read tool.
//
// The character budget is enforced HERE rather than by asking Brave for
// less, because the API has no length parameter — it returns what it has
// and the trimming is ours to do. Truncation is reported inline so the
// model knows it is looking at a partial source rather than the whole
// thing, and the count of omitted sources is stated so it can decide
// whether to ask again with a tighter query.
func (s *Skill) executePageRead(ctx context.Context, params json.RawMessage) (skills.ToolResult, error) {
	var args pageReadArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	if args.Query == "" {
		return skills.ToolResult{Error: "query is required"}, nil
	}

	count := 5
	if args.Count != nil && *args.Count > 0 {
		count = *args.Count
	}
	budget := defaultPageReadChars
	if args.MaxChars != nil && *args.MaxChars > 0 {
		budget = *args.MaxChars
	}
	if budget < 1000 {
		budget = 1000
	}
	if budget > 30000 {
		budget = 30000
	}

	sources, err := s.brave.Context(ctx, args.Query, count, args.Freshness)
	if err != nil {
		// Surfaced to the model rather than failing the turn: it can retry
		// with a different query, fall back to web_search, or answer from
		// what it already has.
		return skills.ToolResult{Error: err.Error()}, nil
	}
	if len(sources) == 0 {
		return skills.ToolResult{Content: "No indexed content for that query."}, nil
	}

	content := formatPageRead(sources, budget)
	data, _ := json.Marshal(sources)
	return skills.ToolResult{
		Content: content,
		Data:    data,
		Tokens:  len(content) / 4,
	}, nil
}

// formatPageRead renders sources under a total character budget, split
// FAIR-SHARE rather than first-come.
//
// The greedy version this replaces filled sources in order until the
// budget ran out. Measured against the live endpoint, one source came
// back with 42 snippets totalling ~5.5k characters — so on a 5-source
// request the first page consumed the entire 6000-char budget and the
// other four were dropped. That inverts the point of the tool, which is
// breadth across sources for grounding, not depth on whichever page
// Brave happened to rank first.
//
// Each source now gets budget/N to start. Sources shorter than their
// share hand the remainder back, and a second pass redistributes it to
// the ones that were clipped, so a short source never wastes its slice
// and a long one still gets more than its floor when there is room.
// Every source that came back is represented, and any that is trimmed
// says so inline.
func formatPageRead(sources []brave.ContextSource, budget int) string {
	if len(sources) == 0 {
		return ""
	}

	// Render each source's full text once, then decide what fits.
	full := make([]string, len(sources))
	head := make([]string, len(sources))
	for i, src := range sources {
		title := src.Title
		if title == "" {
			title = "(untitled)"
		}
		head[i] = fmt.Sprintf("%d. %s\n   %s", i+1, title, src.URL)
		body := strings.Join(src.Snippets, "\n\n")
		full[i] = head[i]
		if body != "" {
			full[i] += "\n\n" + body
		}
	}

	// Reserve the headers: a source is worthless without its URL, and the
	// model needs to see that the source exists even if the body is cut.
	reserved := 0
	for i := range head {
		reserved += len(head[i]) + 2 // +2 for the blank line between entries
	}
	bodyBudget := budget - reserved
	if bodyBudget < 0 {
		bodyBudget = 0
	}

	// Pass 1: equal shares, collecting what the short sources do not use.
	share := bodyBudget / len(sources)
	alloc := make([]int, len(sources))
	spare := 0
	for i := range full {
		want := len(full[i]) - len(head[i])
		if want <= share {
			alloc[i] = want
			spare += share - want
		} else {
			alloc[i] = share
		}
	}
	// Pass 2: hand the spare to whoever is still short, in order.
	for i := range full {
		if spare == 0 {
			break
		}
		want := len(full[i]) - len(head[i])
		if alloc[i] < want {
			give := want - alloc[i]
			if give > spare {
				give = spare
			}
			alloc[i] += give
			spare -= give
		}
	}

	var b strings.Builder
	for i := range full {
		if i > 0 {
			b.WriteString("\n\n")
		}
		bodyLen := len(full[i]) - len(head[i])
		if alloc[i] >= bodyLen {
			b.WriteString(full[i])
			continue
		}
		b.WriteString(full[i][:len(head[i])+alloc[i]])
		b.WriteString("\n   [trimmed — raise max_chars or ask about one source]")
	}
	return strings.TrimSpace(b.String())
}
