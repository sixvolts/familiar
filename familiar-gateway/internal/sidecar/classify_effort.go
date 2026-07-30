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
	"time"

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
	out, _ := c.ClassifyWithStats(ctx, history, userMsg)
	return out
}

// ClassifyStats is the per-call telemetry the turn log needs. Without
// it the two numbers that decide whether this call earns its place on
// the critical path — its tail latency and how often it falls back —
// are simply unknowable, which is the state this code shipped in.
type ClassifyStats struct {
	// Duration is the wall time of the whole attempt, including any
	// wait on the slot gate.
	Duration time.Duration
	// InputTokens / OutputTokens as reported by the server. Zero when
	// the server didn't report usage or the call failed.
	InputTokens  int
	OutputTokens int
	// ModelID is the model the classify role resolved to for this call
	// (chains can fail over between turns).
	ModelID string
	// Err is the failure, if any, that caused a fallback.
	Err error
}

// ClassifyWithStats is Classify plus telemetry. Always returns a valid
// Output; every failure path is stamped with its Source so the caller
// can log the fallback rate and distinguish a real verdict from a
// guess.
func (c *Client) ClassifyWithStats(ctx context.Context, history []Turn, userMsg string) (classifier.Output, ClassifyStats) {
	start := time.Now()
	var st ClassifyStats

	// No sidecar wired at all, or the classify role names no model /
	// its whole chain is offline. Either way we learned nothing about
	// this turn, so take the cheap default rather than billing the box
	// for maximum effort.
	if c == nil {
		st.Duration = time.Since(start)
		st.Err = ErrNoModelConfigured
		return classifier.StaticDefault(), st
	}
	// resolveTask for the model id (telemetry); taskReady for the
	// readiness verdict. Both are cheap map lookups over the same chain.
	modelID, _, _ := c.resolveTask(TaskClassify)
	st.ModelID = modelID
	r, err := c.taskReady(TaskClassify)
	if err != nil {
		st.Duration = time.Since(start)
		st.Err = err
		return classifier.StaticDefault(), st
	}
	if gate := c.gateForTask(TaskClassify); gate != nil {
		gate.syncEnter()
		defer gate.syncExit()
	}

	// Bound the call. This is the last blocking step before the turn can
	// start assembling, and it used to have no deadline of its own — its
	// only ceiling was the shared 10s http.Client, which is far too long
	// to spend deciding how hard to try. [sidecar].request_timeout_ms was
	// parsed and read by nothing; this is now its meaning.
	classifyCtx, cancel := context.WithTimeout(ctx, c.classifyTimeout())
	defer cancel()

	out, usage, err := r.classifyEffortWithUsage(classifyCtx, history, userMsg)
	st.Duration = time.Since(start)
	st.InputTokens, st.OutputTokens = usage.prompt, usage.completion
	if err != nil {
		st.Err = err
		// Transport failure, timeout, non-200, empty choices, or
		// unparseable JSON. We have no verdict — cheap default.
		log.Printf("[sidecar] classify failed after %s (%v) — using static default", st.Duration.Round(time.Millisecond), err)
		return classifier.StaticDefault(), st
	}
	if !out.Validate() {
		st.Err = fmt.Errorf("invalid levels: %+v", out)
		// The model answered but the levels are unusable. That is weak
		// evidence this turn confused a small model, so this is the one
		// case where erring expensive is defensible.
		log.Printf("[sidecar] classify returned invalid levels (%+v) — using conservative fallback", out)
		return classifier.ConservativeFallback(), st
	}
	out.Source = classifier.SourceModel
	return out, st
}

// defaultClassifyTimeout bounds one classify attempt. Deliberately not
// as tight as the 3s used by condense/expand: on a single-slot server
// this call can queue behind an in-flight background extract, and a
// deadline that trips on normal queueing would convert working verdicts
// into fallbacks — trading a real decision for a guess to save a second.
const defaultClassifyTimeout = 6 * time.Second

// classifyTimeout is the per-attempt deadline, from
// [sidecar].request_timeout_ms when set.
func (c *Client) classifyTimeout() time.Duration {
	if c != nil && c.cfg.RequestTimeoutMs > 0 {
		return time.Duration(c.cfg.RequestTimeoutMs) * time.Millisecond
	}
	return defaultClassifyTimeout
}

// classifyUsage is the token accounting the server reports back. The
// numbers are already in the response and were previously discarded;
// they are the cheapest available signal for what this call costs.
type classifyUsage struct {
	prompt     int
	completion int
}

// ClassifyEffort runs the HTTP roundtrip and returns the parsed
// Output. Used by Client.Classify above; exported on HTTPRouter
// so tests can drive it directly without a Client wrapper.
func (r *HTTPRouter) ClassifyEffort(ctx context.Context, history []Turn, userMsg string) (classifier.Output, error) {
	out, _, err := r.classifyEffortWithUsage(ctx, history, userMsg)
	return out, err
}

// classifyEffortWithUsage is ClassifyEffort plus the server's token
// counts.
func (r *HTTPRouter) classifyEffortWithUsage(ctx context.Context, history []Turn, userMsg string) (classifier.Output, classifyUsage, error) {
	var usage classifyUsage
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
		Usage   *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage,omitempty"`
		Error *struct {
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
		// Cap each history turn. The verdict depends on the SHAPE of the
		// exchange, not its full text, but the turns arrive verbatim — so
		// a conversation with a pasted stack trace or a long tool result
		// in it would otherwise prefill thousands of tokens on the
		// critical path to answer a four-field question.
		msgs = append(msgs, chatMsg{Role: role, Content: capClassifyText(t.Content, maxClassifyTurnChars)})
	}
	// The current message is the primary signal, so it gets a larger
	// budget than the history — but still bounded.
	msgs = append(msgs, chatMsg{Role: "user", Content: capClassifyText(userMsg, maxClassifyUserChars)})

	reqBody, err := json.Marshal(chatReq{
		// The served model name. llama-server with a single loaded model
		// ignores this field, which is why a stale literal here has been
		// harmless; it is NOT resolved from the [roles.classify] chain,
		// so a backend that validates the name would reject a failover.
		Model:              classifyServedModelName,
		Messages:           msgs,
		MaxTokens:          200,
		Temperature:        0.1,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	})
	if err != nil {
		return classifier.Output{}, usage, fmt.Errorf("marshal classify request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.endpoint+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return classifier.Output{}, usage, fmt.Errorf("build classify request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return classifier.Output{}, usage, fmt.Errorf("classify request to %s: %w", r.endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return classifier.Output{}, usage, fmt.Errorf("read classify response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return classifier.Output{}, usage, fmt.Errorf("classify HTTP %d: %s", resp.StatusCode, string(body))
	}

	var cr chatResp
	if err := json.Unmarshal(body, &cr); err != nil {
		return classifier.Output{}, usage, fmt.Errorf("parse classify response: %w", err)
	}
	if cr.Usage != nil {
		usage.prompt = cr.Usage.PromptTokens
		usage.completion = cr.Usage.CompletionTokens
	}
	if cr.Error != nil {
		return classifier.Output{}, usage, fmt.Errorf("classify API error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return classifier.Output{}, usage, fmt.Errorf("empty classify response")
	}

	out, err := parseClassifierOutput(cr.Choices[0].Message.Content)
	return out, usage, err
}

// Classifier input caps. The classifier answers a four-field
// multiple-choice question; it does not need whole documents. These
// bound the prefill it can be made to do on the critical path.
const (
	maxClassifyTurnChars = 600
	maxClassifyUserChars = 2000
	// classifyServedModelName is sent as the request's `model`. See the
	// note at its use site — it is not chain-resolved.
	classifyServedModelName = "gemma-4-26b-a4b"
)

// capClassifyText truncates to n runes, keeping the head. The head
// carries the intent ("look up the latest ...") which is what the
// verdict turns on; a trailing paste does not change it.
func capClassifyText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + " […]"
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
