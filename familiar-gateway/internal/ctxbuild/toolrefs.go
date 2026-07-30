package ctxbuild

// Tool-reference auditing for the system prompts.
//
// The prompts tell the model which tools to call, by name, in prose. Those
// names are not checked against anything at build time, so a rename on the
// Go side silently leaves the prompt instructing the model to call a tool
// that does not exist — and the failure is close to invisible: the model
// emits the phantom name, gets an unknown-tool error, and either recovers
// by guessing or quietly does without.
//
// That shipped for real. Four prompt files instructed `memory_search` while
// the registered tool was `search_memory` (only tool_policy.md was right,
// and that is the one file the trivial tier omits — so the cheapest turns
// had no correct mention anywhere). A second name, `core_memory_update`,
// was documented in four files and implemented nowhere at all.
//
// AuditToolRefs closes that gap. It is deliberately one-directional: a
// registered tool that no prompt mentions is merely undocumented, which is
// fine, while a prompt naming an unregistered tool is a bug.

import (
	"regexp"
	"sort"
	"strings"
)

// toolRefPatterns match the positions where these prompts actually
// instruct the model to invoke something. Each has exactly one capture
// group: the tool name.
//
// Being position-aware rather than matching every snake_case token is what
// keeps this quiet enough to live in the boot path. The prompts are full of
// parameter and field names — `book_slug`, `page_slug`, `action_name`,
// `finished_at` — which are not tools and must not be reported. They never
// appear in these positions: parameters show up inside an argument list or
// alone in backticks, never bolded as an entry, after "call"/"use", or
// followed by an open paren.
var toolRefPatterns = []*regexp.Regexp{
	// Documentation entry:  - **search_memory** — Search your ...
	regexp.MustCompile(`\*\*([a-z][a-z0-9_]*_[a-z0-9_]+)\*\*`),
	// Imperative reference: "you MUST call search_memory", "use web_search"
	regexp.MustCompile(`(?i)\b(?:call|calling|use|using|invoke|via)\s+` + "`?" + `([a-z][a-z0-9_]*_[a-z0-9_]+)` + "`?"),
	// Routing arrow:        "Factual recall → search_memory"
	regexp.MustCompile("[→>]\\s*`?([a-z][a-z0-9_]*_[a-z0-9_]+)`?"),
	// Invocation syntax:    read_page(book_slug, page_slug)
	//                       ^ captured        ^ not captured (no paren)
	regexp.MustCompile(`\b([a-z][a-z0-9_]*_[a-z0-9_]+)\(`),
}

// nonToolWords are snake_case tokens that legitimately appear in a
// tool-reference position but name something else. Keep this list short and
// justified — every entry is a place the heuristic is knowingly blind.
var nonToolWords = map[string]bool{
	// Prose that happens to be snake_case-shaped.
	"e_g": true,
	"i_e": true,
}

// ToolRefIssue is one prompt reference that resolves to no registered tool.
type ToolRefIssue struct {
	// Name is the tool the prompt told the model to call.
	Name string
	// Contexts are short excerpts showing where it was referenced, to make
	// the warning actionable without opening the files.
	Contexts []string
}

// AuditToolRefs reports every tool name referenced in promptText that is
// absent from known. Pass the live registry's skills.Registry.KnownToolNames()
// so this cannot drift from what the model is actually offered.
//
// An empty `known` map disables the audit rather than reporting every
// reference: it means the caller has no registry to check against, and a
// flood of false warnings is worse than silence.
//
// Returns nil when everything resolves.
func AuditToolRefs(promptText string, known map[string]bool) []ToolRefIssue {
	if strings.TrimSpace(promptText) == "" || len(known) == 0 {
		return nil
	}

	// name -> excerpts, so a name referenced five times is reported once.
	found := make(map[string][]string)
	for _, re := range toolRefPatterns {
		for _, m := range re.FindAllStringSubmatchIndex(promptText, -1) {
			name := promptText[m[2]:m[3]]
			if known[name] || nonToolWords[name] {
				continue
			}
			// Cap the excerpts per name; the first few are enough to
			// locate it. append creates the key on first sight.
			if len(found[name]) < 3 {
				found[name] = append(found[name], excerpt(promptText, m[0], m[1]))
			}
		}
	}
	if len(found) == 0 {
		return nil
	}

	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]ToolRefIssue, 0, len(names))
	for _, n := range names {
		out = append(out, ToolRefIssue{Name: n, Contexts: found[n]})
	}
	return out
}

// excerpt returns a single-line window around [start,end) for the warning.
func excerpt(s string, start, end int) string {
	const pad = 32
	lo := start - pad
	if lo < 0 {
		lo = 0
	}
	hi := end + pad
	if hi > len(s) {
		hi = len(s)
	}
	return strings.Join(strings.Fields(s[lo:hi]), " ")
}

// AuditedPromptText returns every prompt layer this store can serve,
// concatenated — base (or its admin override), the tool policy, and all
// tier overlays. That is the full surface a model could be shown, so it is
// the right input to AuditToolRefs.
func (s *PromptStore) AuditedPromptText() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var b strings.Builder
	write := func(t string) {
		if strings.TrimSpace(t) != "" {
			b.WriteString(t)
			b.WriteString("\n")
		}
	}
	if s.baseOverride != "" {
		write(s.baseOverride)
	} else {
		write(s.base)
	}
	write(s.toolPolicy)
	// Sorted for a stable warning order across boots.
	keys := make([]string, 0, len(s.overlays))
	for k := range s.overlays {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(s.overlays[k])
	}
	// A deployment with no prompt dir serves the monolithic fallback; it
	// names tools too, so it must be audited as well.
	write(s.fallback)
	return b.String()
}
