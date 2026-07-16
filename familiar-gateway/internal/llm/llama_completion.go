package llm

// LlamaCompletionProvider posts to llama.cpp's raw /completion
// endpoint instead of the OpenAI-compatible /v1/chat/completions.
// CHAT-REARCH §"familiar-raw-completion-design.md" — the turbo
// fork's reasoning parser broke under ROCm 7.2.3 but /completion
// still works; moving prompt construction into the gateway via a
// ModelFormatter lets us route around the bug entirely.
//
// The provider is formatter-agnostic: prompt building + response
// parsing live behind the ModelFormatter interface (qwen35.go,
// gemma.go, ...). This file is pure wire transport.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// LlamaCompletionProvider implements Provider against
// llama-server's /completion endpoint.
type LlamaCompletionProvider struct {
	name      string
	endpoint  string
	apiKey    string
	formatter ModelFormatter
	client    *http.Client

	// sampling holds the per-instance default sampling params,
	// initialised from the formatter's defaults at construction
	// and optionally overridden via WithSampling. Per-request
	// CompletionRequest.Temperature still wins over these.
	sampling map[string]any
}

// NewLlamaCompletionProvider constructs a provider against
// endpoint. formatter is the model-family-specific prompt builder
// + response parser. Both are required.
func NewLlamaCompletionProvider(name, endpoint, apiKey string, formatter ModelFormatter) *LlamaCompletionProvider {
	if formatter == nil {
		formatter = NewQwen35Formatter()
	}
	defaults := formatter.DefaultSamplingParams()
	sampling := make(map[string]any, len(defaults))
	for k, v := range defaults {
		sampling[k] = v
	}
	return &LlamaCompletionProvider{
		name:      name,
		endpoint:  strings.TrimRight(endpoint, "/"),
		apiKey:    apiKey,
		formatter: formatter,
		client: &http.Client{
			Timeout: 600 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true, // fresh connection per request — avoids EOF from stale pooled connections
			},
		},
		sampling: sampling,
	}
}

// WithSampling overlays operator-supplied sampling params onto the
// formatter defaults. Unknown keys are stored verbatim and forwarded
// to /completion as-is — llama.cpp ignores fields it doesn't know.
func (p *LlamaCompletionProvider) WithSampling(overrides map[string]any) *LlamaCompletionProvider {
	for k, v := range overrides {
		p.sampling[k] = v
	}
	return p
}

// Name implements Provider.
func (p *LlamaCompletionProvider) Name() string { return p.name }

// HealthCheck implements Provider. llama-server's /health returns
// 200 when the model is loaded and ready.
func (p *LlamaCompletionProvider) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// completionRequest is the wire payload for /completion. Field
// names match llama.cpp's expected schema.
type completionRequest struct {
	Prompt      string   `json:"prompt"`
	NPredict    int      `json:"n_predict,omitempty"`
	Temperature *float32 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	TopK        *int     `json:"top_k,omitempty"`
	RepeatPen   *float64 `json:"repeat_penalty,omitempty"`
	RepeatLastN *int     `json:"repeat_last_n,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Stream      bool     `json:"stream"`
	CachePrompt bool     `json:"cache_prompt"`
}

// completionStreamChunk is one SSE event's data payload. llama-server
// streams partial tokens in `content`; the terminal event sets
// `stop=true` and may include `tokens_predicted` / `tokens_evaluated`.
type completionStreamChunk struct {
	Content         string `json:"content"`
	Stop            bool   `json:"stop"`
	TokensEvaluated int    `json:"tokens_evaluated"`
	TokensPredicted int    `json:"tokens_predicted"`
	Timings         struct {
		PredictedMS float64 `json:"predicted_ms"`
		PromptMS    float64 `json:"prompt_ms"`
	} `json:"timings"`
}

// completionResponse is the non-streaming reply.
type completionResponse struct {
	Content         string `json:"content"`
	TokensEvaluated int    `json:"tokens_evaluated"`
	TokensPredicted int    `json:"tokens_predicted"`
}

// extractSystem pulls the first system message out of msgs and
// returns its content + the remaining turns. CompletionRequest
// carries the system prompt as a `system`-role message at index 0
// (Pipeline.buildLLMRequest convention); we hoist it into the
// formatter's separate systemPrompt arg.
func extractSystem(msgs []Message) (string, []Message) {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		return msgs[0].Content, msgs[1:]
	}
	for i, m := range msgs {
		if m.Role == "system" {
			rest := make([]Message, 0, len(msgs)-1)
			rest = append(rest, msgs[:i]...)
			rest = append(rest, msgs[i+1:]...)
			return m.Content, rest
		}
	}
	return "", msgs
}

// buildPayload renders the request payload from req + the
// formatter's prompt + the provider's sampling defaults.
func (p *LlamaCompletionProvider) buildPayload(req CompletionRequest) ([]byte, error) {
	systemPrompt, rest := extractSystem(req.Messages)
	turns := MessagesToFormatterTurns(rest)
	prompt := p.formatter.BuildPrompt(systemPrompt, turns, req.Tools, req.EnableThinking)

	wire := completionRequest{
		Prompt:      prompt,
		Stream:      req.Stream,
		Stop:        p.formatter.StopSequences(),
		CachePrompt: true,
	}
	if req.MaxTokens > 0 {
		wire.NPredict = req.MaxTokens
	}
	if req.Temperature != nil {
		t := *req.Temperature
		wire.Temperature = &t
	} else if v, ok := p.sampling["temperature"].(float64); ok {
		t := float32(v)
		wire.Temperature = &t
	}
	if v, ok := p.sampling["top_p"].(float64); ok {
		wire.TopP = &v
	}
	if v, ok := p.sampling["top_k"].(int); ok {
		wire.TopK = &v
	}
	if v, ok := p.sampling["repeat_penalty"].(float64); ok {
		wire.RepeatPen = &v
	}
	if v, ok := p.sampling["repeat_last_n"].(int); ok {
		wire.RepeatLastN = &v
	}
	return json.Marshal(wire)
}

// Complete implements Provider. Non-streaming POST to /completion;
// the formatter parses the raw response into reasoning + content +
// tool calls.
func (p *LlamaCompletionProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	req.Stream = false
	body, err := p.buildPayload(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/completion", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("completion request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("completion HTTP %d: %s", resp.StatusCode, truncateForLog(string(raw)))
	}
	var cr completionResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("parse response: %w (body: %s)", err, truncateForLog(string(raw)))
	}
	return p.parseAndFinalize(cr.Content, cr.TokensEvaluated, cr.TokensPredicted, 0, "stop")
}

// CompleteStream implements Provider. Streams /completion SSE
// events, forwards visible-content tokens via onChunk, surfaces
// reasoning tokens via req.OnReasoningChunk. Tool-call blocks are
// buffered until they close so a partial <tool_call> doesn't leak
// into the user-visible stream.
func (p *LlamaCompletionProvider) CompleteStream(ctx context.Context, req CompletionRequest, onChunk func(string)) (*CompletionResponse, error) {
	req.Stream = true
	body, err := p.buildPayload(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/completion", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		log.Printf("[llm] completion EOF debug: endpoint=%s body_len=%d err=%v", p.endpoint, len(body), err)
		return nil, fmt.Errorf("completion stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("completion HTTP %d: %s", resp.StatusCode, truncateForLog(string(raw)))
	}

	state := newCompletionStreamState(p.formatter.StreamTags(), req.EnableThinking, onChunk, req.OnReasoningChunk)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var tokensIn, tokensOut int
	var decodeMs float64

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(line[len("data: "):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk completionStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		state.feed(chunk.Content)
		if chunk.Stop {
			if chunk.TokensEvaluated > 0 {
				tokensIn = chunk.TokensEvaluated
			}
			if chunk.TokensPredicted > 0 {
				tokensOut = chunk.TokensPredicted
			}
			if chunk.Timings.PredictedMS > 0 {
				decodeMs = chunk.Timings.PredictedMS
			}
		}
	}
	// Mid-stream cancellation (user Stop / hard cap / shutdown): salvage
	// the tokens already produced and streamed rather than erroring the
	// turn, so the committed history matches what the user saw. Only when
	// there is content to keep; an empty cancel is a real error.
	finish := "stop"
	state.flush()
	full := state.full()
	if ctxErr := ctx.Err(); ctxErr != nil && strings.TrimSpace(full) != "" {
		finish = "stopped"
	} else if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan stream: %w", err)
	}

	return p.parseAndFinalize(full, tokensIn, tokensOut, decodeMs, finish)
}

// parseAndFinalize runs the formatter parser and packages the
// result into the wire shape callers expect.
func (p *LlamaCompletionProvider) parseAndFinalize(raw string, tokensIn, tokensOut int, decodeMs float64, finish string) (*CompletionResponse, error) {
	reasoning, content, calls, err := p.formatter.ParseResponse(raw)
	if err != nil {
		return nil, err
	}
	resp := &CompletionResponse{
		Content:          content,
		ReasoningContent: reasoning,
		ToolCalls:        calls,
		InputTokens:      tokensIn,
		OutputTokens:     tokensOut,
		DecodeMs:         decodeMs,
		FinishReason:     finish,
		Model:            p.name,
	}
	if len(calls) > 0 {
		resp.FinishReason = "tool_calls"
	}
	return resp, nil
}

// truncateForLog clips raw responses so a 64K HTML error page
// doesn't fill the logs when something upstream goes sideways.
func truncateForLog(s string) string {
	if len(s) <= 400 {
		return s
	}
	return s[:397] + "..."
}
