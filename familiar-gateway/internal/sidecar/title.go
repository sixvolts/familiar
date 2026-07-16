package sidecar

import (
	"context"
	"strings"
)

const titleSystemPrompt = `You name a chat conversation. Given the opening exchange, reply with a SHORT title that captures the topic.

Rules:
- Reply with ONLY the title. No quotes, no punctuation, no "Title:" prefix, no explanation.
- 1 to 3 words, Title Case.
- Name the topic, not the action — "Postgres Indexing", not "Help With Postgres".
- Trivial small talk → reply "Quick Chat".

Examples:
Exchange: "how do I add a GIN index in postgres" → Postgres Indexing
Exchange: "what's a good name for my cat" → Cat Names
Exchange: "hey" → Quick Chat`

// GenerateTitle asks the model for a 1-3 word title for a new chat,
// given its opening user message and (optionally) the assistant's
// first reply. Returns a cleaned title; the caller treats an error
// as "keep the existing derived title".
func (r *HTTPRouter) GenerateTitle(ctx context.Context, userMsg, assistantMsg string) (string, error) {
	var b strings.Builder
	b.WriteString("User: ")
	b.WriteString(clampForTitle(userMsg, 800))
	if strings.TrimSpace(assistantMsg) != "" {
		b.WriteString("\n\nAssistant: ")
		b.WriteString(clampForTitle(assistantMsg, 800))
	}
	// 64 tokens: three words need very few, but if the classify task
	// is pointed at a reasoning model that still emits a short
	// <think> block despite enable_thinking=false, a 16-token cap
	// would be consumed entirely by the think block and leave no
	// title. cleanTitle strips the block; the cap just has to clear it.
	raw, err := r.chatComplete(ctx, titleSystemPrompt, b.String(), 64, 0.3)
	if err != nil {
		return "", err
	}
	return cleanTitle(raw), nil
}

// clampForTitle trims an input string to at most max bytes on a rune
// boundary — the title model only needs the gist of the exchange,
// not the whole thing.
func clampForTitle(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !isUTF8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// cleanTitle normalizes the model's reply into a usable title:
// strips any leading <think> block, takes the first non-empty line,
// drops surrounding quotes / a "Title:" scaffold / trailing
// punctuation, and clamps to 3 words. Returns "" when nothing usable
// survives so the caller falls back to its derived title.
func cleanTitle(raw string) string {
	t := strings.TrimSpace(raw)
	// A reasoning model may inline a <think>…</think> block ahead of
	// the answer — drop everything up to and including the close tag.
	if i := strings.LastIndex(t, "</think>"); i >= 0 {
		t = strings.TrimSpace(t[i+len("</think>"):])
	}
	// Skip any blank lines a think block left behind; take the first
	// line with real content.
	for _, line := range strings.Split(t, "\n") {
		if strings.TrimSpace(line) != "" {
			t = strings.TrimSpace(line)
			break
		}
	}
	// Drop a "Title:" scaffold the model might echo.
	if i := strings.Index(strings.ToLower(t), "title:"); i == 0 {
		t = strings.TrimSpace(t[len("title:"):])
	}
	t = strings.Trim(t, `"'.,;:!?— `)
	if t == "" {
		return ""
	}
	// Clamp to 3 words — a runaway response shouldn't become the title.
	words := strings.Fields(t)
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.Join(words, " ")
}
