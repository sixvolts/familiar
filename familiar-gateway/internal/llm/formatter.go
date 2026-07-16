package llm

import "encoding/json"

// ModelFormatter renders conversation state into a model-family
// specific prompt string and parses the model's raw output back into
// reasoning / content / tool calls.
//
// Required because the turbo fork's `/v1/chat/completions` reasoning
// parser broke under ROCm 7.2.3 — the raw `/completion` endpoint
// still works, so the gateway now owns prompt construction for
// models that route through that endpoint. See
// familiar-raw-completion-design.md.
//
// Each formatter is one file per model family (qwen35.go,
// gemma.go, ...) so model-specific code stays contained. Provider
// selection picks the formatter from config; the wire layer
// (LlamaCompletionProvider) is formatter-agnostic.
type ModelFormatter interface {
	BuildPrompt(systemPrompt string, turns []FormatterTurn, tools []ToolSpec, enableThinking bool) string
	ParseResponse(raw string) (reasoning, content string, toolCalls []ToolCall, err error)
	StopSequences() []string
	DefaultSamplingParams() map[string]any
	Name() string

	// StreamTags returns the tag strings the streaming state machine
	// needs to separate reasoning from content in real time.
	StreamTags() StreamTagConfig
}

// StreamTagConfig tells the streaming state machine which tags
// delimit reasoning and tool-call blocks for a given model family.
type StreamTagConfig struct {
	ThinkOpen  string // e.g. "<think>" or "<|START_THINKING|>"
	ThinkClose string // e.g. "</think>" or "<|END_THINKING|>"
	ToolOpen   string // e.g. "<tool_call>" or "<|START_ACTION|>"
	ToolClose  string // e.g. "</tool_call>" or "<|END_ACTION|>"
	// DetectThinkingInBody means the model emits thinking-open tags
	// in the response body (not pre-filled in the prompt). The stream
	// state starts in content mode and detects tags on the fly.
	// Qwen: false (thinks tag is in the prompt prefix).
	// Cohere2: true (model emits START_THINKING in the response).
	DetectThinkingInBody bool
}

// FormatterTurn is the formatter's view of one conversation turn —
// a richer shape than llm.Message because the prompt builder needs
// access to the reasoning trace and the per-call tool responses.
// The provider derives this from the llm.Message slice it receives.
type FormatterTurn struct {
	Role             string // "user", "assistant", "tool"
	Content          string
	ReasoningContent string // assistant turns only
	ToolCalls        []ToolCall
	ToolCallID       string // tool turns only
	Name             string // tool turns only — function name
}

// MessagesToFormatterTurns converts the provider-facing
// []Message shape into the formatter's input shape. The two are
// 1:1 today; the conversion exists so a formatter can evolve its
// internal representation without rippling through every provider.
func MessagesToFormatterTurns(msgs []Message) []FormatterTurn {
	out := make([]FormatterTurn, 0, len(msgs))
	for _, m := range msgs {
		t := FormatterTurn{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		out = append(out, t)
	}
	return out
}

// toolSpecsJSON marshals a slice of ToolSpec into the JSON-lines
// shape Qwen3.5 (and most other open models) expect under
// <tools>...</tools> in the system prompt. Each spec gets one line.
// Used by formatter implementations.
func toolSpecsJSON(tools []ToolSpec) string {
	if len(tools) == 0 {
		return ""
	}
	var out []byte
	for i, t := range tools {
		if i > 0 {
			out = append(out, '\n')
		}
		entry := struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}{Type: "function"}
		entry.Function.Name = t.Name
		entry.Function.Description = t.Description
		entry.Function.Parameters = t.Parameters
		b, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		out = append(out, b...)
	}
	return string(out)
}
