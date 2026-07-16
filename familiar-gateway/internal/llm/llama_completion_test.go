package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestQwenStreamStateBasic — thinking → content transition, no tools.
func TestQwenStreamStateBasic(t *testing.T) {
	var reasoning, content strings.Builder
	s := newCompletionStreamState(StreamTagConfig{ThinkOpen: "<think>", ThinkClose: "</think>", ToolOpen: "<tool_call>", ToolClose: "</tool_call>"}, true,
		func(c string) { content.WriteString(c) },
		func(r string) { reasoning.WriteString(r) },
	)
	// Feed chunked stream simulating arbitrary token group boundaries.
	chunks := []string{"reason ", "about it ", "</thi", "nk>\n\n", "the ", "answer ", "is 42."}
	for _, c := range chunks {
		s.feed(c)
	}
	s.flush()
	if reasoning.String() != "reason about it " {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if content.String() != "the answer is 42." {
		t.Errorf("content = %q", content.String())
	}
}

// TestQwenStreamStateToolCallBuffered — a partial <tool_call> mid-chunk
// must not leak through onContent.
func TestQwenStreamStateToolCallBuffered(t *testing.T) {
	var content strings.Builder
	s := newCompletionStreamState(StreamTagConfig{ThinkOpen: "<think>", ThinkClose: "</think>", ToolOpen: "<tool_call>", ToolClose: "</tool_call>"}, false,
		func(c string) { content.WriteString(c) },
		nil,
	)
	chunks := []string{"let me search ", "<tool_call>\n<function=search>\n", "<parameter=q>\nfoo\n</parameter>\n</function>\n</tool_call>", "\nokay"}
	for _, c := range chunks {
		s.feed(c)
	}
	s.flush()
	got := content.String()
	if !strings.Contains(got, "let me search") {
		t.Errorf("missing prefix in content: %q", got)
	}
	if strings.Contains(got, "<tool_call>") {
		t.Errorf("tool_call leaked into content: %q", got)
	}
	if !strings.Contains(got, "okay") {
		t.Errorf("missing suffix after tool_call: %q", got)
	}
}

// TestQwenStreamStateThinkingDisabled — no reasoning channel; first
// chunk goes directly to content.
func TestQwenStreamStateThinkingDisabled(t *testing.T) {
	var content, reasoning strings.Builder
	s := newCompletionStreamState(StreamTagConfig{ThinkOpen: "<think>", ThinkClose: "</think>", ToolOpen: "<tool_call>", ToolClose: "</tool_call>"}, false,
		func(c string) { content.WriteString(c) },
		func(r string) { reasoning.WriteString(r) },
	)
	s.feed("hello world")
	s.flush()
	if reasoning.String() != "" {
		t.Errorf("reasoning should be empty: %q", reasoning.String())
	}
	if content.String() != "hello world" {
		t.Errorf("content = %q", content.String())
	}
}

func TestTagPrefixHold(t *testing.T) {
	cases := []struct {
		text, tag string
		want      int
	}{
		{"hello </thi", "</think>", 5},
		{"hello </think", "</think>", 7},
		{"hello </think>", "</think>", 0},
		{"hello world", "</think>", 0},
		{"<", "<tool_call>", 1},
		{"abc<tool_", "<tool_call>", 6},
	}
	for _, c := range cases {
		got := tagPrefixHold(c.text, c.tag)
		if got != c.want {
			t.Errorf("tagPrefixHold(%q, %q) = %d, want %d", c.text, c.tag, got, c.want)
		}
	}
}

// TestLlamaCompletionProviderNonStream — end-to-end against a fake
// /completion server. Verifies the request payload, system-prompt
// hoisting, and final parsed shape.
func TestLlamaCompletionProviderNonStream(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(completionResponse{
			Content:         "thinking output</think>\n\nThe answer.",
			TokensEvaluated: 42,
			TokensPredicted: 12,
		})
	}))
	defer srv.Close()

	p := NewLlamaCompletionProvider("local/qwen", srv.URL, "", NewQwen35Formatter())
	resp, err := p.Complete(context.Background(), CompletionRequest{
		Messages: []Message{
			{Role: "system", Content: "you are familiar"},
			{Role: "user", Content: "hi"},
		},
		EnableThinking: true,
		MaxTokens:      4096,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotPath != "/completion" {
		t.Errorf("path = %q", gotPath)
	}
	prompt, _ := gotBody["prompt"].(string)
	if !strings.Contains(prompt, "<|im_start|>system\nyou are familiar<|im_end|>") {
		t.Errorf("prompt missing system: %s", prompt)
	}
	if !strings.HasSuffix(prompt, "<|im_start|>assistant\n<think>\n") {
		t.Errorf("prompt should end with <think> generation prompt; got tail: %s", prompt[len(prompt)-50:])
	}
	stops, _ := gotBody["stop"].([]any)
	if len(stops) != 2 {
		t.Errorf("stop sequences = %v", stops)
	}
	if resp.Content != "The answer." {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.InputTokens != 42 || resp.OutputTokens != 12 {
		t.Errorf("tokens = %d/%d", resp.InputTokens, resp.OutputTokens)
	}
}

// TestLlamaCompletionProviderStream — fake SSE; verifies content +
// reasoning emit on the right channels and the final response
// reflects the full parse.
func TestLlamaCompletionProviderStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		emit := func(c string, stop bool) {
			b, _ := json.Marshal(completionStreamChunk{Content: c, Stop: stop, TokensPredicted: 7})
			io.WriteString(w, "data: ")
			w.Write(b)
			io.WriteString(w, "\n\n")
			flusher.Flush()
		}
		emit("thinking ", false)
		emit("done</think>\n\n", false)
		emit("the answer", false)
		emit("", true)
	}))
	defer srv.Close()

	p := NewLlamaCompletionProvider("local/qwen", srv.URL, "", NewQwen35Formatter())
	var reasoning, content strings.Builder
	resp, err := p.CompleteStream(context.Background(), CompletionRequest{
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		},
		EnableThinking:   true,
		OnReasoningChunk: func(r string) { reasoning.WriteString(r) },
	}, func(c string) { content.WriteString(c) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(reasoning.String(), "thinking done") {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if !strings.Contains(content.String(), "the answer") {
		t.Errorf("content = %q", content.String())
	}
	if resp.Content != "the answer" {
		t.Errorf("final content = %q", resp.Content)
	}
	if resp.OutputTokens != 7 {
		t.Errorf("output tokens = %d", resp.OutputTokens)
	}
}

// scannerEmitsTerminatedLines is a regression check on bufio's
// behavior we depend on — SSE events are \n\n-separated; scanner
// must see complete `data: ...` lines.
func TestSSELinesFlush(t *testing.T) {
	rd := strings.NewReader("data: {\"content\":\"a\"}\n\ndata: {\"content\":\"b\",\"stop\":true}\n\n")
	sc := bufio.NewScanner(rd)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) < 2 {
		t.Fatalf("scanned %d lines", len(lines))
	}
}
