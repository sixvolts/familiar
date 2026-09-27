package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// thinkingKwargs builds the chat_template_kwargs payload.
//
// reasoning_effort MUST travel inside chat_template_kwargs, not as a
// top-level request field. Measured against rune (Qwen 3.8 on llama.cpp) with
// one fixed prompt: as a top-level field, low/medium/xhigh all produced ~420
// characters of reasoning — silently ignored. Inside chat_template_kwargs the
// same prompt gave 430 chars at low and 991 at xhigh. The server's
// --reasoning-effort launch flag sets the default; this overrides it per
// request.
//
// An empty Effort omits the key entirely, so the server default still applies
// and backends without an effort dial are unaffected.
func thinkingKwargs(req CompletionRequest) map[string]any {
	kw := map[string]any{"enable_thinking": req.EnableThinking}
	if req.EnableThinking && req.ReasoningEffort != "" {
		kw["reasoning_effort"] = req.ReasoningEffort
	}
	return kw
}

// OpenAIProvider implements Provider for OpenAI-compatible endpoints
// (llama-server, Ollama, vLLM, etc.).
type OpenAIProvider struct {
	name     string
	endpoint string
	apiKey   string
	client   *http.Client
}

// NewOpenAIProvider constructs a provider for an OpenAI-compatible endpoint.
func NewOpenAIProvider(name, endpoint, apiKey string) *OpenAIProvider {
	return &OpenAIProvider{
		name:     name,
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		// No Client.Timeout: it bounds the whole request, body included,
		// and cut every completion streaming past 600s with an error that
		// discarded what the user had already seen (the turn allows 30
		// minutes). The caller's context bounds each request (the
		// pipeline's turnHardCap); HealthCheck sets its own.
		client: &http.Client{
			// Fresh connection per request. llama.cpp closes the socket
			// after a stream completes, so a pooled keep-alive connection
			// goes stale; the next tool-loop iteration then writes to a
			// dead socket (broken pipe / EOF). Matches llama_completion.go.
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}
}

// Name returns the provider name.
func (p *OpenAIProvider) Name() string { return p.name }

// openAIFunctionCall is the `function` sub-object inside a tool_call.
// OpenAI serialises arguments as a JSON *string* (not object); we pass
// the Go side through as a json.RawMessage but quote it on the wire.
type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

// openAIToolSpec is the `tools[]` entry in a chat completion request.
// Parameters is already a JSON Schema object, so json.RawMessage avoids
// a double-marshal round-trip.
type openAIToolSpec struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type openAIRequest struct {
	Model              string           `json:"model"`
	Messages           []openAIMessage  `json:"messages"`
	MaxTokens          int              `json:"max_tokens,omitempty"`
	Temperature        *float32         `json:"temperature,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	StreamOptions      *streamOptions   `json:"stream_options,omitempty"`
	Tools              []openAIToolSpec `json:"tools,omitempty"`
	ToolChoice         any              `json:"tool_choice,omitempty"`
	ChatTemplateKwargs map[string]any   `json:"chat_template_kwargs,omitempty"`
}

// streamOptions toggles streaming-specific behaviors. include_usage
// asks the upstream to emit a final chunk with usage.prompt_tokens /
// usage.completion_tokens populated — without it, llama-server's
// OpenAI-compat streaming endpoint omits usage entirely and our
// CompletionResponse comes back with InputTokens/OutputTokens=0.
// CHAT-REARCH §"Phase 0" moved this from the client (which used to
// speak OpenAI directly to the gateway) to here on the gateway →
// llama-server hop, where it actually belongs.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// openAIDeltaToolCall is the streaming fragment of a tool_call. OpenAI
// splits each call across many chunks keyed by Index: the first chunk
// usually carries id + type + function.name, subsequent chunks append
// to function.arguments. We accumulate by Index and assemble the final
// ToolCall slice at stream end.
type openAIDeltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type openAIStreamChunk struct {
	// Error is an in-band error some servers (vLLM) send as a data
	// chunk after the 200.
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			ReasoningContent string                `json:"reasoning_content"`
			Role             string                `json:"role"`
			Content          string                `json:"content"`
			ToolCalls        []openAIDeltaToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
	// Timings is a llama.cpp extension included on the final stream
	// chunk: the server's own measurement of decode time. Using it
	// (rather than browser wall-clock) keeps the chat UI's tok/s honest
	// for reasoning models, whose hidden think phase inflates wall-clock
	// (see docs/tokrate-metric-issue.md).
	Timings *struct {
		PredictedN         int     `json:"predicted_n"`
		PredictedMs        float64 `json:"predicted_ms"`
		PredictedPerSecond float64 `json:"predicted_per_second"`
	} `json:"timings,omitempty"`
}

func (p *OpenAIProvider) doRequest(ctx context.Context, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	httpReq.Header.Set("content-type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	return p.client.Do(httpReq)
}

func buildOpenAIMessages(msgs []Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		om := openAIMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if len(m.ToolCalls) > 0 {
			om.ToolCalls = make([]openAIToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				args := string(tc.Arguments)
				if args == "" {
					args = "{}"
				}
				om.ToolCalls = append(om.ToolCalls, openAIToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openAIFunctionCall{
						Name:      tc.Name,
						Arguments: args,
					},
				})
			}
		}
		if om.Role == "assistant" && om.Content == "" && len(om.ToolCalls) == 0 {
			om.Content = "..."
		}
		// content is omitempty, and llama-server rejects a tool message
		// with neither content nor tool_calls: an empty tool result (a
		// search with no hits, a script that printed nothing) failed the
		// next request and the turn.
		if om.Role == "tool" && om.Content == "" {
			om.Content = "(no output)"
		}
		out = append(out, om)
	}
	return out
}

// stripToolMessages removes tool-role messages and assistant messages
// with tool_calls from the history. Also removes orphaned tool messages
// that lost their tool_call_id (pipeline storage doesn't always preserve
// the full tool-use message structure). Needed to prevent Cohere2's
// Jinja template from crashing on malformed tool history.
func stripToolMessages(msgs []openAIMessage) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" {
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			// Keep the assistant turn's prose without its tool calls; a
			// call with no prose goes. It used to become a "..." stub,
			// sent as an assistant message right before the real reply
			// (strict-alternation templates refuse two in a row).
			if m.Content == "" || m.Content == "..." {
				continue
			}
			cleaned := m
			cleaned.ToolCalls = nil
			out = append(out, cleaned)
			continue
		}
		// Skip stub assistant messages (content "...") that were
		// tool-calling turns with their calls stripped.
		if m.Role == "assistant" && m.Content == "..." {
			continue
		}
		out = append(out, m)
	}
	return out
}

// sanitizeToolHistory fixes malformed tool messages in conversation
// history. Tool messages without tool_call_id crash Cohere2's Jinja
// template. Remove them and their orphaned assistant counterparts.
//
// A tool message must also answer a call of the assistant message
// before it. One whose call isn't there (a history window that opened
// after the call, a result after the final reply) is an HTTP 400 on
// servers that check the pairing, and was sent as-is because it had an
// id.
func sanitizeToolHistory(msgs []openAIMessage) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	var calls map[string]bool // ids of the tool calls awaiting results
	for _, m := range msgs {
		if m.Role == "tool" {
			// Remove tool messages with empty tool_call_id, and results
			// no preceding assistant message asked for.
			if m.ToolCallID == "" || !calls[m.ToolCallID] {
				continue
			}
			out = append(out, m)
			continue
		}
		// Remove stub assistant messages that lost their tool_calls
		if m.Role == "assistant" && m.Content == "..." && len(m.ToolCalls) == 0 {
			continue
		}
		calls = nil
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			calls = make(map[string]bool, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls[tc.ID] = true
			}
		}
		out = append(out, m)
	}
	return out
}

// buildOpenAITools converts provider-agnostic ToolSpecs into the
// chat-completions `tools` array shape.
func buildOpenAITools(specs []ToolSpec) []openAIToolSpec {
	if len(specs) == 0 {
		return nil
	}
	out := make([]openAIToolSpec, 0, len(specs))
	for _, s := range specs {
		var t openAIToolSpec
		t.Type = "function"
		t.Function.Name = s.Name
		t.Function.Description = s.Description
		t.Function.Parameters = s.Parameters
		out = append(out, t)
	}
	return out
}

// Complete sends a non-streaming request and returns the full response.
func (p *OpenAIProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	msgs := buildOpenAIMessages(req.Messages)
	tools := buildOpenAITools(req.Tools)
	// Always sanitize — malformed tool history crashes Cohere2's
	// template even when tools ARE present in the request.
	msgs = sanitizeToolHistory(msgs)
	if len(tools) == 0 {
		msgs = stripToolMessages(msgs)
	}
	body := &openAIRequest{
		Model:       req.Model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      false,
		Tools:       tools,
		// Pass the caller's thinking decision through to llama-server.
		// For Qwen-family models, enable_thinking=true means "emit <think>
		// blocks"; false means "answer directly". llama-server extracts
		// <think> blocks into reasoning_content regardless (controlled by
		// the --reasoning server flag, not this kwarg), so content stays
		// clean either way.
		//
		// Historical note: this used to be hardcoded true for Step-3.5-Flash
		// (which always thinks regardless of the kwarg). After the switch
		// to Qwen3.5-122B, hardcoding true forced thinking on everywhere,
		// which broke tier 3's "fast structured output, no thinking"
		// contract. Respect the request field now. See BUGS.md Bug 2.
		ChatTemplateKwargs: thinkingKwargs(req),
	}
	if req.ToolChoice != "" {
		body.ToolChoice = req.ToolChoice
	}

	resp, err := p.doRequest(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("openai complete: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai API error %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var or2 openAIResponse
	if err := json.Unmarshal(bodyBytes, &or2); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if or2.Error != nil {
		return nil, fmt.Errorf("openai error %s: %s", or2.Error.Type, or2.Error.Message)
	}

	var (
		content      string
		toolCalls    []ToolCall
		finishReason string
	)
	if len(or2.Choices) > 0 {
		choice := or2.Choices[0]
		content = choice.Message.Content
		finishReason = choice.FinishReason
		for _, tc := range choice.Message.ToolCalls {
			// arguments arrives as a JSON-encoded string; pass the raw
			// bytes through as a RawMessage so the skill layer can
			// unmarshal it directly into typed args.
			args := toolArgs(tc.Function.Arguments)
			toolCalls = append(toolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: args,
			})
		}
	}

	result := &CompletionResponse{
		// Defense in depth: strip any tool-protocol tags a model
		// leaked into content even on the structured-tool_calls path
		// (see qwen35.go scrubToolTags — the 2026-06-13 Slack leak).
		Content:      scrubToolTags(content),
		InputTokens:  or2.Usage.PromptTokens,
		OutputTokens: or2.Usage.CompletionTokens,
		Model:        or2.Model,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}
	// No post-hoc reasoning split here. splitUntaggedReasoning was
	// written for Command A's untagged chain-of-thought, but ran on every
	// answer from every OpenAI-compatible model: an ordinary reply that
	// opened with "According to…", "Let's…" or "Okay," and later said
	// "This is…" had everything before that sentence moved into the
	// collapsed thinking panel, and only the tail saved as the answer.
	// Command A is served through llama-completion with the cohere2
	// formatter, which does its own split.
	return result, nil
}

// CompleteStream sends a streaming request and calls onChunk for each text delta.
func (p *OpenAIProvider) CompleteStream(ctx context.Context, req CompletionRequest, onChunk func(string)) (*CompletionResponse, error) {
	msgs := buildOpenAIMessages(req.Messages)
	tools := buildOpenAITools(req.Tools)
	msgs = sanitizeToolHistory(msgs)
	if len(tools) == 0 {
		msgs = stripToolMessages(msgs)
	}
	body := &openAIRequest{
		Model:         req.Model,
		Messages:      msgs,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Tools:         tools,
		// See Complete() for why this respects req.EnableThinking rather
		// than forcing true. Same rationale applies for streaming.
		ChatTemplateKwargs: thinkingKwargs(req),
	}
	if req.ToolChoice != "" {
		body.ToolChoice = req.ToolChoice
	}

	resp, err := p.doRequest(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("openai stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai API error %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// accToolCall accumulates one tool_call across streaming chunks.
	// id/name typically arrive in the first chunk; args is appended as
	// fragments flow in. order preserves the delta Index so we can
	// output ToolCalls in a stable sequence even if indices are sparse.
	type accToolCall struct {
		id   string
		name string
		args strings.Builder
	}

	var (
		fullContent  strings.Builder
		inputTokens  int
		outputTokens int
		decodeMs     float64
		modelID      string
		finishReason string
		toolAcc      = make(map[int]*accToolCall)
		toolOrder    []int
	)

	// sawDone records that the stream ended properly: [DONE], or a
	// finish_reason. A stream that just stops (a backend that died after
	// its 200) returned whatever it had as a success, so a blank reply
	// was committed instead of failing over to the next model.
	sawDone := false
	scanner := bufio.NewScanner(resp.Body)
	// Bump the buffer — default 64 KB is tight if a model emits a
	// large tool_calls fragment or a big argument blob in one chunk.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		// llama.cpp reports a failure after the 200 as an SSE line
		// "error: {...}".
		if strings.HasPrefix(line, "error: ") || strings.HasPrefix(line, "error:") {
			return nil, fmt.Errorf("openai stream: server error: %s", truncateForLog(strings.TrimSpace(strings.TrimPrefix(line, "error:"))))
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			break
		}
		if data == "" {
			continue
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("openai stream: server error: %s", truncateForLog(chunk.Error.Message))
		}

		if modelID == "" {
			modelID = chunk.Model
		}

		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}
		if chunk.Timings != nil {
			if chunk.Timings.PredictedMs > 0 {
				decodeMs = chunk.Timings.PredictedMs
			}
			// Fall back to the server's predicted token count when usage
			// is absent — older llama.cpp builds report only one or the
			// other.
			if outputTokens == 0 && chunk.Timings.PredictedN > 0 {
				outputTokens = chunk.Timings.PredictedN
			}
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				onChunk(choice.Delta.Content)
				fullContent.WriteString(choice.Delta.Content)
			}
			if choice.Delta.ReasoningContent != "" && req.OnReasoningChunk != nil {
				req.OnReasoningChunk(choice.Delta.ReasoningContent)
			}
			for _, dtc := range choice.Delta.ToolCalls {
				acc, seen := toolAcc[dtc.Index]
				if !seen {
					acc = &accToolCall{}
					toolAcc[dtc.Index] = acc
					toolOrder = append(toolOrder, dtc.Index)
				}
				if dtc.ID != "" {
					acc.id = dtc.ID
				}
				if dtc.Function.Name != "" {
					acc.name = dtc.Function.Name
				}
				if dtc.Function.Arguments != "" {
					acc.args.WriteString(dtc.Function.Arguments)
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}

	// If the caller cancelled mid-stream (user pressed Stop, hard cap, or
	// shutdown), the scan aborts with a context error. Rather than discard
	// the tokens already produced and streamed to the user, return them as
	// a truncated-but-complete response so the turn commits the partial —
	// keeping the persisted history in sync with what the user saw. Only
	// salvage when there is actual content; an empty cancel is a real error.
	if ctxErr := ctx.Err(); ctxErr != nil && fullContent.Len() > 0 {
		finishReason = "stopped"
	} else if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading stream: %w", err)
	} else if !sawDone && finishReason == "" {
		return nil, fmt.Errorf("openai stream: ended without [DONE] or a finish_reason (%d chars received): %w", fullContent.Len(), io.ErrUnexpectedEOF)
	}

	var toolCalls []ToolCall
	for _, idx := range toolOrder {
		acc := toolAcc[idx]
		if acc == nil || acc.name == "" {
			// Some providers emit empty trailing tool_call frames;
			// skip anything without a function name.
			continue
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: toolArgs(acc.args.String()),
		})
	}

	result := &CompletionResponse{
		Content:      scrubToolTags(fullContent.String()),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		DecodeMs:     decodeMs,
		Model:        modelID,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}
	// No post-hoc reasoning split (see Complete).
	return result, nil
}

// toolArgs turns a tool call's arguments string into a RawMessage that
// is valid JSON. A backend can pass through a truncated or malformed
// string; kept raw, it failed to marshal when the turn was saved, and
// the whole assistant turn's tool calls were dropped, leaving its tool
// results orphaned in every later prompt. Invalid text is kept as a
// JSON string (the dispatch still fails, as it should).
func toolArgs(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

// HealthCheck verifies the endpoint is reachable.
func (p *OpenAIProvider) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.endpoint+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("building health request: %w", err)
	}

	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close()

	// Unhealthy: rejected credentials, or a server that answers but can't
	// serve (5xx: an upstream behind a proxy is dead, or llama-server is
	// still loading its model). Every non-401 status used to count as
	// healthy, so a listening-but-broken primary stayed "online", every
	// call went to it and failed, and failover never started. Other 4xx
	// (a 404 from a server without /v1/models) still count as reachable.
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s: unauthorized (HTTP %d)", p.name, resp.StatusCode)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%s: health HTTP %d", p.name, resp.StatusCode)
	}
	return nil
}
