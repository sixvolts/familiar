package sidecar

import (
	"context"
	"strings"
)

const expandSystemPrompt = `You decompose a user message into short, specific search queries for a personal knowledge base.

Rules:
- Output 2 to 4 queries, one per line. No numbering, no bullets, no explanation.
- Each query should target a distinct aspect of the user's message.
- Prefer short keyword phrases (3-6 words) over full questions.
- If the user message asks about a named entity and a specific property (e.g. "gpu-host's IP"), produce one query for the entity and one for the property.
- If the user message is a simple greeting or has no retrievable content, return a single line containing just the original message.

Examples:

User: What is gpu-host and what is its IP address?
gpu-host hardware specs
gpu-host IP address
gpu-host network config

User: How does my PKI setup work?
personal PKI architecture
CA hierarchy certificates
PKI signing keys

User: hey
hey`

// ExpandQueries asks the sidecar to break a user message into multiple
// targeted search queries for the memory store. Returns the expanded
// queries, not including the original message — the caller is expected
// to prepend the original so a single-embedding fallback always runs too.
//
// On any error (sidecar down, parse failure, empty response) the caller
// should fall back to searching the original message alone. Query
// expansion is an optimization; it must not block retrieval.
func (r *HTTPRouter) ExpandQueries(ctx context.Context, userMsg string) ([]string, error) {
	raw, err := r.chatComplete(ctx, expandSystemPrompt, userMsg, 120, 0.2)
	if err != nil {
		return nil, err
	}
	return parseExpandedQueries(raw), nil
}

// parseExpandedQueries splits the sidecar's newline-separated response
// into clean query strings. Empty lines and trivial markdown cruft are
// dropped. Exported for tests.
func parseExpandedQueries(raw string) []string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	out := make([]string, 0, len(lines))
	seen := make(map[string]bool)
	for _, line := range lines {
		q := strings.TrimSpace(line)
		// Strip leading bullets / numbering if the model ignored the
		// "no numbering" instruction.
		q = strings.TrimLeft(q, "-*0123456789. )")
		q = strings.TrimSpace(q)
		// Strip surrounding quotes.
		q = strings.Trim(q, `"'`)
		if q == "" || seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, q)
	}
	return out
}
