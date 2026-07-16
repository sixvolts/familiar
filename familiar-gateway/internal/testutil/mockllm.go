package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// MockLLM is an httptest server that impersonates an OpenAI-compatible
// /v1/chat/completions endpoint. It consumes a queue of scripted
// responses in FIFO order and records every inbound request so tests
// can assert on what the pipeline sent.
//
// The scripted-response model is deliberate: tests declare the exact
// sequence of model replies up front (including tool calls), and the
// server returns them one by one as the pipeline drives its tool loop.
// Running out of scripted responses is a test failure — that almost
// always means the pipeline called the model more times than expected.
//
// MockLLM also answers GET /v1/models with an empty OK response so
// HealthCheck doesn't 404.
type MockLLM struct {
	t      *testing.T
	server *httptest.Server

	mu        sync.Mutex
	responses []ScriptedResponse
	calls     []RecordedCall
}

// ScriptedResponse declares one reply the mock will return. Exactly one
// of Content or ToolCalls should be non-empty per response, matching the
// two modes a real LLM uses (plain text answer vs tool call request).
type ScriptedResponse struct {
	// Content is the assistant text reply. Leave empty when returning
	// tool calls instead.
	Content string

	// ToolCalls is the list of tool calls the model is requesting. When
	// non-empty, Content should usually be empty and FinishReason will
	// default to "tool_calls".
	ToolCalls []ScriptedToolCall

	// FinishReason overrides the default. "stop" for text, "tool_calls"
	// for tool-calling responses.
	FinishReason string

	// Usage overrides token accounting. Zero values produce sensible
	// defaults (10 prompt / 5 completion) so tests that don't care about
	// numbers don't have to set them.
	PromptTokens     int
	CompletionTokens int
}

// ScriptedToolCall models one function-call entry in a tool_calls
// response. Arguments is a Go map that will be JSON-encoded into the
// `function.arguments` wire field as a string.
type ScriptedToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// RecordedCall is one request the MockLLM received. Tests assert on
// these to verify the pipeline built the request they expected.
type RecordedCall struct {
	Model    string
	Messages []RecordedMessage
	Tools    []RecordedTool
	Stream   bool
	RawBody  []byte
}

// RecordedMessage mirrors the chat-completions message wire format,
// including tool_call metadata so tests can verify multi-turn exchanges.
type RecordedMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	ToolCalls  []RecordedToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	Name       string             `json:"name,omitempty"`
}

// RecordedToolCall is the shape of one tool_call entry inside a
// recorded assistant message.
type RecordedToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// RecordedTool mirrors the `tools[]` entry in the chat-completions
// request so tests can assert on which specs the pipeline advertised.
type RecordedTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// NewMockLLM starts a test server. Close via t.Cleanup automatically.
func NewMockLLM(t *testing.T) *MockLLM {
	t.Helper()
	m := &MockLLM{t: t}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

// URL returns the base URL of the running mock server. Pass this to
// NewOpenAIProvider as the endpoint.
func (m *MockLLM) URL() string { return m.server.URL }

// Enqueue appends scripted responses to the queue. Responses are
// consumed FIFO as requests arrive.
func (m *MockLLM) Enqueue(responses ...ScriptedResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, responses...)
}

// Calls returns a copy of the recorded requests made so far.
func (m *MockLLM) Calls() []RecordedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// CallCount returns the number of requests received.
func (m *MockLLM) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *MockLLM) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		_, _ = w.Write([]byte(`{"data":[]}`))
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req struct {
		Model    string            `json:"model"`
		Messages []RecordedMessage `json:"messages"`
		Tools    []RecordedTool    `json:"tools"`
		Stream   bool              `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.calls = append(m.calls, RecordedCall{
		Model:    req.Model,
		Messages: req.Messages,
		Tools:    req.Tools,
		Stream:   req.Stream,
		RawBody:  body,
	})
	if len(m.responses) == 0 {
		m.mu.Unlock()
		m.t.Errorf("MockLLM: no scripted response for request #%d (model=%q)", len(m.calls), req.Model)
		http.Error(w, "no scripted response", http.StatusInternalServerError)
		return
	}
	next := m.responses[0]
	m.responses = m.responses[1:]
	m.mu.Unlock()

	if req.Stream {
		m.writeStreamResponse(w, next)
		return
	}
	m.writeBatchResponse(w, next)
}

func (m *MockLLM) writeBatchResponse(w http.ResponseWriter, s ScriptedResponse) {
	finish := s.FinishReason
	if finish == "" {
		if len(s.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	prompt := s.PromptTokens
	if prompt == 0 {
		prompt = 10
	}
	completion := s.CompletionTokens
	if completion == 0 {
		completion = 5
	}

	type oaCall struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}

	var calls []oaCall
	for _, tc := range s.ToolCalls {
		argBytes, _ := json.Marshal(tc.Arguments)
		var c oaCall
		c.ID = tc.ID
		c.Type = "function"
		c.Function.Name = tc.Name
		c.Function.Arguments = string(argBytes)
		calls = append(calls, c)
	}

	resp := map[string]any{
		"id":     "mock-1",
		"object": "chat.completion",
		"model":  "mock-model",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":       "assistant",
				"content":    s.Content,
				"tool_calls": calls,
			},
			"finish_reason": finish,
		}},
		"usage": map[string]int{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *MockLLM) writeStreamResponse(w http.ResponseWriter, s ScriptedResponse) {
	// Streaming format: one chunk with role, one with content or
	// tool_calls, one with finish_reason + usage, then [DONE]. We
	// intentionally keep it minimal — tests covering chunk-by-chunk
	// accumulation belong in internal/llm.
	w.Header().Set("content-type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	write := func(chunk string) {
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		if flusher != nil {
			flusher.Flush()
		}
	}

	if len(s.ToolCalls) > 0 {
		for i, tc := range s.ToolCalls {
			argBytes, _ := json.Marshal(tc.Arguments)
			frag := fmt.Sprintf(
				`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
				i, tc.ID, tc.Name, string(argBytes),
			)
			write(frag)
		}
	} else if s.Content != "" {
		// Escape by reusing JSON marshalling for the content value.
		cb, _ := json.Marshal(s.Content)
		write(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"role":"assistant","content":%s}}]}`, cb))
	}

	finish := s.FinishReason
	if finish == "" {
		if len(s.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	prompt := s.PromptTokens
	if prompt == 0 {
		prompt = 10
	}
	completion := s.CompletionTokens
	if completion == 0 {
		completion = 5
	}
	write(fmt.Sprintf(
		`{"choices":[{"index":0,"delta":{},"finish_reason":%q}],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`,
		finish, prompt, completion,
	))
	write("[DONE]")
}

// AssertAllConsumed fails the test if scripted responses remain
// unconsumed. Call at the end of a test to catch "expected N calls but
// only got M" bugs.
func (m *MockLLM) AssertAllConsumed() {
	m.t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.responses) > 0 {
		names := make([]string, 0, len(m.responses))
		for _, r := range m.responses {
			if r.Content != "" {
				names = append(names, "text:"+truncate(r.Content, 40))
			} else {
				tc := make([]string, 0, len(r.ToolCalls))
				for _, t := range r.ToolCalls {
					tc = append(tc, t.Name)
				}
				names = append(names, "tool_calls:"+strings.Join(tc, ","))
			}
		}
		m.t.Errorf("MockLLM: %d scripted responses unconsumed: %v", len(m.responses), names)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
