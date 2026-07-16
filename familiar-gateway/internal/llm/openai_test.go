package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- buildOpenAIMessages ----------------------------------------------------

func TestBuildOpenAIMessages_PlainContent(t *testing.T) {
	in := []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hi"},
	}
	out := buildOpenAIMessages(in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "you are helpful" {
		t.Errorf("system msg wrong: %+v", out[0])
	}
	if len(out[0].ToolCalls) != 0 {
		t.Errorf("plain msg should have no tool_calls, got %d", len(out[0].ToolCalls))
	}
}

func TestBuildOpenAIMessages_ToolCallStringification(t *testing.T) {
	// Assistant message with a tool_call whose Arguments is already valid JSON.
	// buildOpenAIMessages must pass it through as a JSON *string* per the
	// OpenAI wire format, not re-marshal the bytes.
	args := json.RawMessage(`{"query":"weather in SF","limit":5}`)
	in := []Message{
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []ToolCall{
				{ID: "call_123", Name: "search", Arguments: args},
			},
		},
	}
	out := buildOpenAIMessages(in)
	if len(out[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(out[0].ToolCalls))
	}
	tc := out[0].ToolCalls[0]
	if tc.ID != "call_123" || tc.Function.Name != "search" {
		t.Errorf("wrong id/name: %+v", tc)
	}
	if tc.Function.Arguments != string(args) {
		t.Errorf("arguments not passed through verbatim:\ngot  %q\nwant %q", tc.Function.Arguments, string(args))
	}
	if tc.Type != "function" {
		t.Errorf("Type = %q, want function", tc.Type)
	}
}

func TestBuildOpenAIMessages_EmptyToolCallArgsDefaultsToBraces(t *testing.T) {
	// A model that calls a no-arg tool may emit "" for Arguments.
	// OpenAI requires a valid JSON string; we substitute "{}".
	in := []Message{
		{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: "x", Name: "noop", Arguments: json.RawMessage("")}},
		},
	}
	out := buildOpenAIMessages(in)
	if got := out[0].ToolCalls[0].Function.Arguments; got != "{}" {
		t.Errorf("empty args should default to \"{}\", got %q", got)
	}
}

func TestBuildOpenAIMessages_ToolRoleMessage(t *testing.T) {
	in := []Message{
		{Role: "tool", Content: "result!", ToolCallID: "call_abc", Name: "search"},
	}
	out := buildOpenAIMessages(in)
	if out[0].Role != "tool" || out[0].ToolCallID != "call_abc" || out[0].Name != "search" {
		t.Errorf("tool message not preserved: %+v", out[0])
	}
	if out[0].Content != "result!" {
		t.Errorf("content dropped: %+v", out[0])
	}
}

// --- buildOpenAITools -------------------------------------------------------

func TestBuildOpenAITools_NilEmpty(t *testing.T) {
	if got := buildOpenAITools(nil); got != nil {
		t.Errorf("nil in should give nil out, got %+v", got)
	}
	if got := buildOpenAITools([]ToolSpec{}); got != nil {
		t.Errorf("empty in should give nil out, got %+v", got)
	}
}

func TestBuildOpenAITools_PreservesJSONSchemaRawMessage(t *testing.T) {
	// Parameters is already a JSON schema — it must NOT be re-marshaled
	// (that would double-encode the object as a string). The RawMessage
	// pass-through is the whole point of using RawMessage here.
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	specs := []ToolSpec{
		{Name: "search", Description: "web search", Parameters: schema},
	}
	out := buildOpenAITools(specs)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].Type != "function" {
		t.Errorf("Type = %q, want function", out[0].Type)
	}
	if out[0].Function.Name != "search" || out[0].Function.Description != "web search" {
		t.Errorf("name/desc: %+v", out[0].Function)
	}
	if string(out[0].Function.Parameters) != string(schema) {
		t.Errorf("schema mutated:\ngot  %s\nwant %s", out[0].Function.Parameters, schema)
	}

	// Round-trip: when the whole request is marshaled, the schema appears
	// as an object (not a string). This catches the "double encoding" bug.
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"parameters":"{`) {
		t.Errorf("parameters got double-encoded as string: %s", body)
	}
}

// --- Complete: request body + tool-call parsing -----------------------------
//
// These drive a real OpenAIProvider against an httptest.Server so we cover
// the full request-build and response-parse paths (which are where tool call
// JSON handling and thinking-token injection actually live).

func TestOpenAIProviderComplete_RequestShapeAndToolParsing(t *testing.T) {
	var captured openAIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if ct := r.Header.Get("content-type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("auth header = %q", auth)
		}

		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}

		// Respond with a tool_calls choice: model wants to call search.
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1",
			"model": "fake-model",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "call_abc",
						"type": "function",
						"function": {"name": "search", "arguments": "{\"q\":\"weather\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test", srv.URL, "test-key")
	resp, err := p.Complete(context.Background(), CompletionRequest{
		Model:          "fake-model",
		Messages:       []Message{{Role: "user", Content: "check weather"}},
		EnableThinking: true,
		Tools: []ToolSpec{
			{Name: "search", Description: "search the web", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Request-side assertions.
	if captured.Model != "fake-model" {
		t.Errorf("captured.Model = %q", captured.Model)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Function.Name != "search" {
		t.Errorf("tools not forwarded: %+v", captured.Tools)
	}
	// Thinking-token plumbing: OpenAIProvider must forward req.EnableThinking
	// through to chat_template_kwargs.enable_thinking verbatim, not hardcode
	// a value. This test case passes EnableThinking=true above; the separate
	// TestOpenAIComplete_ThinkingOff case below asserts the false path.
	if got, ok := captured.ChatTemplateKwargs["enable_thinking"]; !ok || got != true {
		t.Errorf("expected chat_template_kwargs.enable_thinking=true (forwarded from req), got %+v", captured.ChatTemplateKwargs)
	}
	if captured.Stream {
		t.Errorf("Complete must set stream=false")
	}

	// Response-side assertions: tool call parsing.
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Name != "search" {
		t.Errorf("wrong tool call: %+v", tc)
	}
	if string(tc.Arguments) != `{"q":"weather"}` {
		t.Errorf("arguments not passed through as RawMessage: %q", tc.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q", resp.FinishReason)
	}
	if resp.InputTokens != 10 || resp.OutputTokens != 5 {
		t.Errorf("usage: got (%d, %d), want (10, 5)", resp.InputTokens, resp.OutputTokens)
	}
}

func TestOpenAIProviderComplete_EmptyToolArgsDefault(t *testing.T) {
	// Some models emit "" for tool arguments when the function takes no
	// params. Complete() must substitute "{}" so downstream unmarshal
	// doesn't explode.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {
					"role": "assistant",
					"tool_calls": [{
						"id": "c1",
						"type": "function",
						"function": {"name": "ping", "arguments": ""}
					}]
				}
			}]
		}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test", srv.URL, "")
	resp, err := p.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls", len(resp.ToolCalls))
	}
	if string(resp.ToolCalls[0].Arguments) != "{}" {
		t.Errorf("empty args should default to {}, got %q", resp.ToolCalls[0].Arguments)
	}
}

func TestOpenAIProviderComplete_APIErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"nope","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test", srv.URL, "")
	_, err := p.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("expected wrapped API error, got %v", err)
	}
}

func TestOpenAIProviderComplete_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test", srv.URL, "")
	_, err := p.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected error on 502")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should mention status code, got %v", err)
	}
}

// --- CompleteStream: delta-by-index tool call assembly ---------------------

func TestOpenAIProviderCompleteStream_AssemblesToolCallsFromDeltas(t *testing.T) {
	// Simulate the OpenAI streaming pattern: id+name arrive in chunk 1,
	// arguments arrive across chunks 2 and 3, then a [DONE] terminator.
	// The provider must accumulate by index and produce one complete
	// ToolCall.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\""}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"weather\"}"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
			`data: [DONE]`,
			``,
		}
		_, _ = w.Write([]byte(strings.Join(chunks, "\n\n")))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test", srv.URL, "")
	var chunks []string
	resp, err := p.CompleteStream(context.Background(), CompletionRequest{Model: "m"}, func(c string) {
		chunks = append(chunks, c)
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 assembled tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "search" {
		t.Errorf("wrong id/name: %+v", tc)
	}
	if string(tc.Arguments) != `{"q":"weather"}` {
		t.Errorf("arguments not stitched: %q", tc.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q", resp.FinishReason)
	}
	if len(chunks) != 0 {
		t.Errorf("no text deltas were emitted, but onChunk was called %d times", len(chunks))
	}
}

func TestOpenAIProviderCompleteStream_SalvagesPartialOnCancel(t *testing.T) {
	// The server streams two content chunks, flushes, then hangs (no
	// [DONE]). The caller cancels mid-stream — the user pressed Stop.
	// CompleteStream must return the partial already streamed, not an
	// error, with a "stopped" finish, so the turn commits what the user
	// saw instead of discarding it.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, c := range []string{"Hello", " world"} {
			_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"` + c + `"}}]}` + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		<-release // hang until the test lets the handler return
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewOpenAIProvider("test", srv.URL, "")
	var got strings.Builder
	resp, err := p.CompleteStream(ctx, CompletionRequest{Model: "m"}, func(c string) {
		got.WriteString(c)
		if got.String() == "Hello world" {
			cancel() // user hits Stop after seeing both chunks
		}
	})
	if err != nil {
		t.Fatalf("CompleteStream returned an error instead of salvaging the partial: %v", err)
	}
	if resp.Content != "Hello world" {
		t.Errorf("partial content = %q, want %q", resp.Content, "Hello world")
	}
	if resp.FinishReason != "stopped" {
		t.Errorf("finish reason = %q, want stopped", resp.FinishReason)
	}
}

func TestOpenAIProviderCompleteStream_ReasoningChunkCallback(t *testing.T) {
	// reasoning_content in the delta should be routed to OnReasoningChunk,
	// not to onChunk. OnReasoningChunk nil ⇒ drop silently.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"reasoning_content":"thinking..."}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
			`data: [DONE]`,
			``,
		}
		_, _ = w.Write([]byte(strings.Join(chunks, "\n\n")))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test", srv.URL, "")
	var textChunks, reasoningChunks []string
	resp, err := p.CompleteStream(context.Background(), CompletionRequest{
		Model:            "m",
		OnReasoningChunk: func(c string) { reasoningChunks = append(reasoningChunks, c) },
	}, func(c string) { textChunks = append(textChunks, c) })
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if resp.Content != "answer" {
		t.Errorf("content = %q, want answer", resp.Content)
	}
	if len(textChunks) != 1 || textChunks[0] != "answer" {
		t.Errorf("textChunks = %v", textChunks)
	}
	if len(reasoningChunks) != 1 || reasoningChunks[0] != "thinking..." {
		t.Errorf("reasoningChunks = %v", reasoningChunks)
	}
}

func TestOpenAIProviderCompleteStream_ParsesLlamaTimings(t *testing.T) {
	// llama.cpp appends a `timings` object to the final stream chunk.
	// We surface predicted_ms as DecodeMs so the chat UI can report an
	// honest tok/s (server-measured) instead of browser wall-clock,
	// which is deflated for reasoning models. predicted_n backfills the
	// token count when usage is absent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"reasoning_content":"think think think"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"42"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"timings":{"predicted_n":3103,"predicted_ms":61000.0,"predicted_per_second":50.6}}`,
			`data: [DONE]`,
			``,
		}
		_, _ = w.Write([]byte(strings.Join(chunks, "\n\n")))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test", srv.URL, "")
	resp, err := p.CompleteStream(context.Background(), CompletionRequest{Model: "m"}, func(string) {})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if resp.DecodeMs != 61000.0 {
		t.Errorf("DecodeMs = %v, want 61000", resp.DecodeMs)
	}
	// No usage block in this stream → predicted_n backfills the count.
	if resp.OutputTokens != 3103 {
		t.Errorf("OutputTokens = %d, want 3103 (from predicted_n)", resp.OutputTokens)
	}
	// Sanity: this is the honest ~50 tok/s, not a deflated wall-clock rate.
	rate := float64(resp.OutputTokens) / (resp.DecodeMs / 1000)
	if rate < 48 || rate > 52 {
		t.Errorf("derived decode rate = %.1f tok/s, want ~50", rate)
	}
}

func TestOpenAIProviderCompleteStream_UsageWinsOverPredictedN(t *testing.T) {
	// When both usage and timings are present, usage.completion_tokens is
	// authoritative for the count; timings still supplies DecodeMs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":200},"timings":{"predicted_n":999,"predicted_ms":4000.0}}`,
			`data: [DONE]`,
			``,
		}
		_, _ = w.Write([]byte(strings.Join(chunks, "\n\n")))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test", srv.URL, "")
	resp, err := p.CompleteStream(context.Background(), CompletionRequest{Model: "m"}, func(string) {})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if resp.OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200 (usage wins)", resp.OutputTokens)
	}
	if resp.DecodeMs != 4000.0 {
		t.Errorf("DecodeMs = %v, want 4000", resp.DecodeMs)
	}
}
