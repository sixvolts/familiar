package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer answers every request with the given SSE lines.
func sseServer(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const openAIContent = `data: {"choices":[{"index":0,"delta":{"content":"partial answer"}}]}`

// A stream that stops with neither [DONE] nor a finish_reason (a backend
// that died after its 200) is an error, so the pipeline fails over: it
// returned the partial as a success and committed a blank or cut reply.
// An in-band error, as an "error:" line or a data chunk, is one too.
func TestOpenAIStream_UnterminatedOrErrorIsAnError(t *testing.T) {
	for name, lines := range map[string][]string{
		"no terminator":  {openAIContent},
		"nothing at all": {},
		// A [DONE] after the error still doesn't make it a success.
		"error line":  {openAIContent, `error: {"code":500,"message":"slot unavailable"}`, "data: [DONE]"},
		"error chunk": {openAIContent, `data: {"error":{"message":"CUDA out of memory"}}`, "data: [DONE]"},
	} {
		t.Run(name, func(t *testing.T) {
			p := NewOpenAIProvider("t", sseServer(t, lines...).URL, "")
			if resp, err := p.CompleteStream(context.Background(), CompletionRequest{Model: "m"}, func(string) {}); err == nil {
				t.Fatalf("returned %+v, want an error", resp)
			}
		})
	}
	for name, lines := range map[string][]string{
		"[DONE]":        {openAIContent, "data: [DONE]"},
		"finish_reason": {openAIContent, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
	} {
		t.Run("ok with "+name, func(t *testing.T) {
			p := NewOpenAIProvider("t", sseServer(t, lines...).URL, "")
			resp, err := p.CompleteStream(context.Background(), CompletionRequest{Model: "m"}, func(string) {})
			if err != nil || resp.Content != "partial answer" {
				t.Fatalf("got %+v, %v", resp, err)
			}
		})
	}
}

// llama-server's /completion stream ends with a stop=true chunk; without
// one the stream was cut and is an error.
func TestLlamaStream_WithoutStopChunkIsAnError(t *testing.T) {
	srv := sseServer(t, `data: {"content":"the answer","stop":false}`)
	p := NewLlamaCompletionProvider("q", srv.URL, "", NewQwen35Formatter())
	_, err := p.CompleteStream(context.Background(), CompletionRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, func(string) {})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want an unexpected-EOF error", err)
	}
}

// Qwen with thinking on: the prompt ends "<think>\n", so the output is
// reasoning until "</think>". Output that stops before it (a Stop, the
// token budget) is all reasoning; it was returned as the answer, shown
// in place of one and committed. The budget cut is "length".
func TestLlamaCompletion_UnclosedThinkingIsReasoning(t *testing.T) {
	stream := sseServer(t,
		`data: {"content":"Let me work out","stop":false}`,
		`data: {"content":" the options first","stop":false}`,
		`data: {"content":"","stop":true,"stop_type":"limit"}`)
	p := NewLlamaCompletionProvider("q", stream.URL, "", NewQwen35Formatter())
	var shown strings.Builder
	resp, err := p.CompleteStream(context.Background(), CompletionRequest{
		Messages: []Message{{Role: "user", Content: "hi"}}, EnableThinking: true,
	}, func(c string) { shown.WriteString(c) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "" || shown.Len() != 0 {
		t.Errorf("content %q (streamed %q), want none", resp.Content, shown.String())
	}
	if resp.ReasoningContent != "Let me work out the options first" {
		t.Errorf("reasoning = %q", resp.ReasoningContent)
	}
	if resp.FinishReason != "length" {
		t.Errorf("finish = %q, want length", resp.FinishReason)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "Still thinking about it", "stop_type": "limit"})
	}))
	defer plain.Close()
	p = NewLlamaCompletionProvider("q", plain.URL, "", NewQwen35Formatter())
	resp, err = p.Complete(context.Background(), CompletionRequest{Messages: []Message{{Role: "user", Content: "hi"}}, EnableThinking: true})
	if err != nil || resp.Content != "" || resp.ReasoningContent != "Still thinking about it" || resp.FinishReason != "length" {
		t.Errorf("non-stream: %+v, %v", resp, err)
	}
	// Thinking off: the same text is the answer.
	resp, _ = p.Complete(context.Background(), CompletionRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if resp.Content != "Still thinking about it" {
		t.Errorf("thinking off: content = %q", resp.Content)
	}
}

// An empty tool result is sent with content; content is omitempty and
// llama-server rejects a tool message with neither content nor calls.
func TestBuildOpenAIMessages_EmptyToolResultHasContent(t *testing.T) {
	out := buildOpenAIMessages([]Message{{Role: "tool", ToolCallID: "c1", Content: ""}})
	b, _ := json.Marshal(out[0])
	if !strings.Contains(string(b), `"content":"(no output)"`) {
		t.Errorf("tool message = %s", b)
	}
}

// Without tools advertised, a tool call with no prose is dropped; it
// became a "..." assistant message right before the real reply.
func TestStripToolMessages_DropsCallsWithoutProse(t *testing.T) {
	out := stripToolMessages(buildOpenAIMessages([]Message{
		{Role: "user", Content: "what's X?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "c", Content: "42"},
		{Role: "assistant", Content: "X is 42."},
	}))
	var got []string
	for _, m := range out {
		got = append(got, m.Role+":"+m.Content)
	}
	if strings.Join(got, "|") != "user:what's X?|assistant:X is 42." {
		t.Errorf("stripped = %v", got)
	}
}

// Malformed tool-call arguments stay valid JSON (as a string), on both
// paths, so the turn's tool calls survive being saved.
func TestToolArgs_InvalidJSONKeptAsString(t *testing.T) {
	if got := string(toolArgs(`{"q": "unterminated`)); got != `"{\"q\": \"unterminated"` {
		t.Errorf("toolArgs = %s", got)
	}
	if got := string(toolArgs(`{"q":"ok"}`)); got != `{"q":"ok"}` {
		t.Errorf("valid args changed: %s", got)
	}
	srv := sseServer(t,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"search","arguments":"{\"q\": \"unterm"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	resp, err := NewOpenAIProvider("t", srv.URL, "").CompleteStream(context.Background(), CompletionRequest{Model: "m"}, func(string) {})
	if err != nil || len(resp.ToolCalls) != 1 || !json.Valid(resp.ToolCalls[0].Arguments) {
		t.Fatalf("stream tool call = %+v, %v", resp, err)
	}
}

// A re-rendered Qwen tool call lists its parameters in the order the
// model wrote them, the same on every render. Map order changed between
// renders, breaking llama.cpp's cached prefix at every tool call.
func TestQwen_ToolCallRendersInEmissionOrder(t *testing.T) {
	_, _, calls, _ := NewQwen35Formatter().ParseResponse(
		"<tool_call>\n<function=update_page>\n<parameter=page_slug>\nplans\n</parameter>\n<parameter=book_slug>\nhome\n</parameter>\n<parameter=version>\n3\n</parameter>\n</function>\n</tool_call>")
	if len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
	first := renderToolCall(calls[0])
	for i := 0; i < 50; i++ {
		if r := renderToolCall(calls[0]); r != first {
			t.Fatalf("render %d differs:\n%s\nvs\n%s", i, r, first)
		}
	}
	p, b, v := strings.Index(first, "page_slug"), strings.Index(first, "book_slug"), strings.Index(first, "version")
	if !(p < b && b < v) || !strings.Contains(first, "<parameter=version>\n3\n") {
		t.Errorf("rendered out of order or retyped:\n%s", first)
	}
}

// Cohere2 renders each call with its own id, which its result carries;
// a formatter-lifetime counter matched nothing and changed every build.
func TestCohere2_ToolCallIDsMatchResults(t *testing.T) {
	f := NewCohere2Formatter()
	turns := []FormatterTurn{
		{Role: "user", Content: "find it"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "coh_0_search", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)}}},
		{Role: "tool", ToolCallID: "coh_0_search", Name: "search", Content: "found"},
	}
	a := f.BuildPrompt("sys", turns, nil, false)
	if b := f.BuildPrompt("sys", turns, nil, false); a != b {
		t.Error("two builds of one history differ")
	}
	if strings.Count(a, `"tool_call_id": "coh_0_search"`) != 2 {
		t.Errorf("call and result ids don't match:\n%s", a)
	}
}

// Inside a tool loop the assistant's reasoning reaches the Qwen prompt:
// MessagesToFormatterTurns dropped it, so the in-loop branch never ran
// outside a hand-built test.
func TestLlamaCompletion_ToolLoopKeepsReasoning(t *testing.T) {
	p := NewLlamaCompletionProvider("q", "http://unused", "", NewQwen35Formatter())
	payload, err := p.buildPayload(CompletionRequest{EnableThinking: true, Messages: []Message{
		{Role: "user", Content: "fix the page"},
		{Role: "assistant", ReasoningContent: "Plan: read it, then patch line 3.", ToolCalls: []ToolCall{{ID: "c", Name: "read_page", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "c", Name: "read_page", Content: "page text"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "Plan: read it, then patch line 3.") {
		t.Errorf("in-loop reasoning missing from the prompt: %s", payload)
	}
}

// Providers have no whole-request timeout: it cut every completion
// streaming past 600s (the turn allows 30 minutes) and discarded what
// the user had seen. The caller's context bounds requests.
func TestProviders_NoWholeRequestTimeout(t *testing.T) {
	if to := NewOpenAIProvider("t", "http://x", "").client.Timeout; to != 0 {
		t.Errorf("openai client timeout = %v", to)
	}
	if to := NewLlamaCompletionProvider("q", "http://x", "", NewQwen35Formatter()).client.Timeout; to != 0 {
		t.Errorf("llama client timeout = %v", to)
	}
}
