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
		client: &http.Client{
			Timeout: 600 * time.Second,
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
			// Keep the assistant turn but without tool calls — just
			// preserve any content it had.
			cleaned := m
			cleaned.ToolCalls = nil
			if cleaned.Content == "" {
				cleaned.Content = "..."
			}
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
func sanitizeToolHistory(msgs []openAIMessage) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		// Remove tool messages with empty tool_call_id
		if m.Role == "tool" && m.ToolCallID == "" {
			continue
		}
		// Remove stub assistant messages that lost their tool_calls
		if m.Role == "assistant" && m.Content == "..." && len(m.ToolCalls) == 0 {
			continue
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
		ChatTemplateKwargs: map[string]any{"enable_thinking": req.EnableThinking},
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
			args := json.RawMessage(tc.Function.Arguments)
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
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
	// Post-hoc reasoning split for models that generate untagged
	// chain-of-thought (like Cohere2 for non-tool queries).
	if result.ReasoningContent == "" && len(result.ToolCalls) == 0 {
		if r, c, ok := splitUntaggedReasoning(result.Content); ok {
			result.ReasoningContent = r
			result.Content = c
		}
	}
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
		ChatTemplateKwargs: map[string]any{"enable_thinking": req.EnableThinking},
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

	scanner := bufio.NewScanner(resp.Body)
	// Bump the buffer — default 64 KB is tight if a model emits a
	// large tool_calls fragment or a big argument blob in one chunk.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		if data == "" {
			continue
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
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
	}

	var toolCalls []ToolCall
	for _, idx := range toolOrder {
		acc := toolAcc[idx]
		if acc == nil || acc.name == "" {
			// Some providers emit empty trailing tool_call frames;
			// skip anything without a function name.
			continue
		}
		args := acc.args.String()
		if args == "" {
			args = "{}"
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: json.RawMessage(args),
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
	// Post-hoc reasoning split (same as non-streaming path).
	if result.ReasoningContent == "" && len(result.ToolCalls) == 0 {
		if r, c, ok := splitUntaggedReasoning(result.Content); ok {
			result.ReasoningContent = r
			result.Content = c
		}
	}
	return result, nil
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

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s: unauthorized", p.name)
	}
	return nil
}
