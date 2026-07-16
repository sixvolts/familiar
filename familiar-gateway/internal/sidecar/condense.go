package sidecar

import (
	"context"
	"strings"
)

const condenseSystemPrompt = `You rewrite a user's latest chat message into ONE self-contained search query for a personal knowledge base.

The user is mid-conversation. Their latest message often leans on earlier turns — "what about the timeout?", "and his email?", "do that for the other one too". A query built from that message alone retrieves nothing useful. Rewrite it into a single standalone query that someone with no knowledge of the conversation could act on.

Rules:
- Output EXACTLY ONE line: the rewritten query. No explanation, no quotes, no numbering, no preamble.
- Resolve pronouns and back-references using the conversation history.
- If the latest message is already self-contained, output it unchanged.
- If the latest message is small talk or a greeting with nothing to retrieve, output it unchanged.
- Keep it concise — a search query, not a paragraph.

Examples:

History:
user: Tell me about the gpu-host server.
assistant: Gpu-host is the GPU box running the 122B model.
Latest message: what's its IP?
Rewritten query: gpu-host server IP address

History:
user: I'm setting up Postgres.
assistant: Sure — what do you need?
Latest message: what's the default port?
Rewritten query: Postgres default port

History:
user: hey
Latest message: hey
Rewritten query: hey`

// CondenseQuery rewrites a mid-conversation user message into a
// single self-contained retrieval query, resolving pronouns and
// back-references against the recent dialogue. Returns the rewritten
// query; on any failure the caller should fall back to the raw
// message (condensation is a retrieval-quality optimization, never
// a hard dependency).
//
// history should be the last few turns in chronological order. With
// no history there's nothing to resolve against, so the message is
// returned unchanged without an LLM round-trip.
func (r *HTTPRouter) CondenseQuery(ctx context.Context, history []Turn, userMsg string) (string, error) {
	if len(history) == 0 {
		return userMsg, nil
	}

	var b strings.Builder
	b.WriteString("History:\n")
	for _, t := range history {
		role := t.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(t.Content)
		b.WriteString("\n")
	}
	b.WriteString("Latest message: ")
	b.WriteString(userMsg)
	b.WriteString("\nRewritten query:")

	raw, err := r.chatComplete(ctx, condenseSystemPrompt, b.String(), 80, 0.1)
	if err != nil {
		return "", err
	}
	condensed := parseCondensed(raw)
	if condensed == "" {
		return userMsg, nil
	}
	return condensed, nil
}

// parseCondensed extracts the rewritten query from the sidecar's
// response: first non-empty line, stripped of quotes and any
// "Rewritten query:" prefix the model may have echoed. Exported
// shape kept package-private; covered by condense_test.go.
func parseCondensed(raw string) string {
	q := strings.TrimSpace(raw)
	if i := strings.IndexByte(q, '\n'); i >= 0 {
		q = strings.TrimSpace(q[:i])
	}
	// Drop a leading label if the model echoed the prompt scaffold.
	if i := strings.Index(strings.ToLower(q), "rewritten query:"); i == 0 {
		q = strings.TrimSpace(q[len("rewritten query:"):])
	}
	q = strings.Trim(q, `"'`)
	return strings.TrimSpace(q)
}
