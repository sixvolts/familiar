package sidecar

import (
	"context"
	"strings"
	"testing"
	"time"
)

// lastUserPrompt returns the content of the final user message captured by
// fakeChatServer — the assembled extractor prompt.
func lastUserPrompt(t *testing.T, c *capturedChatReq) string {
	t.Helper()
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == "user" {
			return c.Messages[i].Content
		}
	}
	t.Fatalf("no user message captured")
	return ""
}

const okExtraction = `{"facts":[{"content":"gpu-host has 64GB RAM","category":"hardware"}],"relationships":[]}`

// ExtractFactsWithContext must emit a <context> block (prior turns, for
// reference resolution) that precedes the <conversation> block (the turn pair
// we actually extract from), and the prompt must instruct the model not to
// extract from the context.
func TestExtractFactsWithContext_PromptShape(t *testing.T) {
	srv, captured := fakeChatServer(t, okExtraction)
	r := NewHTTPRouter(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	turns := []Turn{
		{Role: "user", Content: "bump it to 64GB"},
		{Role: "assistant", Content: "Done — gpu-host is now at 64GB."},
	}
	context := []Turn{
		{Role: "user", Content: "how much RAM does gpu-host have?"},
		{Role: "assistant", Content: "gpu-host has 32GB."},
	}
	if _, err := r.ExtractFactsWithContext(ctx, turns, context); err != nil {
		t.Fatalf("extract: %v", err)
	}

	prompt := lastUserPrompt(t, captured)
	ci := strings.Index(prompt, "<context>")
	if ci < 0 {
		t.Fatalf("prompt missing <context> block:\n%s", prompt)
	}
	convi := strings.Index(prompt, "<conversation>")
	if convi < 0 {
		t.Fatalf("prompt missing <conversation> block")
	}
	if ci > convi {
		t.Errorf("<context> must precede <conversation> (ctx=%d conv=%d)", ci, convi)
	}
	// Prior turn content lands only in the context block; the current pair in
	// the conversation block.
	if !strings.Contains(prompt[ci:convi], "how much RAM does gpu-host have?") {
		t.Errorf("prior turn missing from <context> block:\n%s", prompt[ci:convi])
	}
	if !strings.Contains(prompt[convi:], "bump it to 64GB") {
		t.Errorf("current turn missing from <conversation> block")
	}
	if strings.Contains(prompt[convi:], "gpu-host has 32GB") {
		t.Errorf("prior turn leaked into <conversation> block")
	}
}

// The nil-context path (ExtractFacts, and document extraction via
// wikiknowledge) must not emit a <context> block at all — unchanged behavior.
func TestExtractFacts_NoContextBlock(t *testing.T) {
	srv, captured := fakeChatServer(t, okExtraction)
	r := NewHTTPRouter(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	turns := []Turn{
		{Role: "user", Content: "my api key is sk-123"},
		{Role: "assistant", Content: "noted"},
	}
	if _, err := r.ExtractFacts(ctx, turns); err != nil {
		t.Fatalf("extract: %v", err)
	}
	prompt := lastUserPrompt(t, captured)
	if strings.Contains(prompt, "<context>") {
		t.Errorf("nil-context extract must not emit a <context> block:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<conversation>") {
		t.Errorf("prompt missing <conversation> block")
	}
}
