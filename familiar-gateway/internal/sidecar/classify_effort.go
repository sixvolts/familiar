package sidecar

// Effort-level classifier: emits classifier.Output from the new
// CHAT-REARCH ordinal schema. Replaces the older RoutingDecision
// flow as consumers cut over. The classifier model is the sidecar
// (small slot); the prompt asks for a constrained JSON object
// per CHAT-REARCH §"Classifier — Output".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/familiar/gateway/internal/classifier"
)

// Classify returns the classifier's per-turn ordinal-effort
// verdict for the current user message, considering up to 2-3
// recent turns of context. Always returns a valid Output —
// failures fall back to ConservativeFallback per spec.
//
// Caller responsibility: trim `history` to the most recent few
// turns before calling. The classifier prompt advertises 2-3
// turns; passing 50 wastes tokens without changing the verdict.
func (c *Client) Classify(ctx context.Context, history []Turn, userMsg string) classifier.Output {
	if c == nil {
		return classifier.ConservativeFallback()
	}
	r, err := c.taskReady(TaskClassify)
	if err != nil {
		return classifier.ConservativeFallback()
	}
	if gate := c.gateForTask(TaskClassify); gate != nil {
		gate.syncEnter()
		defer gate.syncExit()
	}
	out, err := r.ClassifyEffort(ctx, history, userMsg)
	if err != nil {
		log.Printf("[sidecar] classify failed (%v) — using conservative fallback", err)
		return classifier.ConservativeFallback()
	}
	if !out.Validate() {
		log.Printf("[sidecar] classify returned invalid levels (%+v) — using conservative fallback", out)
		return classifier.ConservativeFallback()
	}
	return out
}

// ClassifyEffort runs the HTTP roundtrip and returns the parsed
// Output. Used by Client.Classify above; exported on HTTPRouter
// so tests can drive it directly without a Client wrapper.
func (r *HTTPRouter) ClassifyEffort(ctx context.Context, history []Turn, userMsg string) (classifier.Output, error) {
	type chatMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type chatReq struct {
		Model              string         `json:"model"`
		Messages           []chatMsg      `json:"messages"`
		MaxTokens          int            `json:"max_tokens"`
		Temperature        float64        `json:"temperature"`
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	}
	type chatChoice struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	type chatResp struct {
		Choices []chatChoice `json:"choices"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	// Build the message array: system prompt, then prior turns in
	// CHRONOLOGICAL order (oldest first), then the current user
	// message. The spec is explicit about not reversing — the model
	// treats reversed dialogue as off-distribution.
	msgs := []chatMsg{
		{Role: "system", Content: classifyEffortSystemPrompt},
	}
	for _, t := range history {
		role := t.Role
		if role != "user" && role != "assistant" {
			continue
		}
		msgs = append(msgs, chatMsg{Role: role, Content: t.Content})
	}
	msgs = append(msgs, chatMsg{Role: "user", Content: userMsg})

	reqBody, err := json.Marshal(chatReq{
		Model:              "gemma-4-26b-a4b",
		Messages:           msgs,
		MaxTokens:          200,
		Temperature:        0.1,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	})
	if err != nil {
		return classifier.Output{}, fmt.Errorf("marshal classify request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.endpoint+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return classifier.Output{}, fmt.Errorf("build classify request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return classifier.Output{}, fmt.Errorf("classify request to %s: %w", r.endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return classifier.Output{}, fmt.Errorf("read classify response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return classifier.Output{}, fmt.Errorf("classify HTTP %d: %s", resp.StatusCode, string(body))
	}

	var cr chatResp
	if err := json.Unmarshal(body, &cr); err != nil {
		return classifier.Output{}, fmt.Errorf("parse classify response: %w", err)
	}
	if cr.Error != nil {
		return classifier.Output{}, fmt.Errorf("classify API error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return classifier.Output{}, fmt.Errorf("empty classify response")
	}

	return parseClassifierOutput(cr.Choices[0].Message.Content)
}

// parseClassifierOutput extracts a classifier.Output from the
// model's response text. The model is asked for a JSON object;
// in practice it sometimes wraps it in prose or fences. The
// extractor scans for the first `{` and last `}` and decodes
// what's between, mirroring the lenient parseRoutingDecision
// path that's been load-tested.
func parseClassifierOutput(raw string) (classifier.Output, error) {
	start := bytes.IndexByte([]byte(raw), '{')
	end := bytes.LastIndexByte([]byte(raw), '}')
	if start < 0 || end < 0 || end <= start {
		return classifier.Output{}, fmt.Errorf("no JSON object in classifier output: %q", raw)
	}
	jsonText := raw[start : end+1]

	var out classifier.Output
	if err := json.Unmarshal([]byte(jsonText), &out); err != nil {
		return classifier.Output{}, fmt.Errorf("decode classifier JSON: %w (text: %q)", err, jsonText)
	}
	return out, nil
}

// classifyEffortSystemPrompt is the constraint the classifier
// model gets. Tone is intentionally terse — the model needs
// to emit a JSON object, not prose, and biasing toward
// over-effort matters more than length. Per CHAT-REARCH
// §"Classifier — Behavior".
//
// This is a starter prompt. The eval harness + hand-labeled
// dataset (deferred — see Chat-Backend-Rearch.md §"Dataset &
// Eval") will iterate this in a follow-up.
const classifyEffortSystemPrompt = `You are a request classifier. For each user turn, emit a JSON object with FOUR fields. No prose. No code fences. Just the object.

{
  "thinking":     "off" | "low" | "medium" | "high",
  "memory_depth": "none" | "shallow" | "deep",
  "search_depth": "none" | "shallow" | "deep",
  "tools": []
}

GUIDANCE
- "thinking" — how much reasoning the answer needs. Greetings and one-word follow-ups: off. Factual lookups: low. Multi-step reasoning: medium. Hard problems with branching: high. Bias toward MORE thinking when uncertain — wasted tokens are cheaper than a degraded answer.
- "memory_depth" — how deeply to search the user's saved memories. Topic continuation referencing prior context: shallow. New question that may pull on past discussions: deep. Pure greeting / meta-question: none.
- "search_depth" — web search budget. The user's literal request must imply a search ("look up", "what's the latest", "search for"); otherwise none. NEVER pick "shallow" or "deep" because the topic feels searchable. Hallucinated searches are worse than no search.
- "tools" — a list of tools the answer needs. Common values: "search", "notes_read", "notes_write", "wiki_read", "wiki_write", "memory_save". Empty list when none apply. Do not include tools the request doesn't actually need.

OUTPUT
Just the JSON object. No explanation, no fences, no preamble.`
