package llm

import (
	"context"
	"encoding/json"
)

// Message represents a single chat message.
//
// Role is one of "system", "user", "assistant", or "tool". Tool messages
// carry a ToolCallID referencing the assistant's tool_call that produced
// them; their Content is the serialized tool result (typically the
// ToolResult.Content string from the skill framework). Assistant
// messages may also carry ToolCalls, in which case Content may be empty.
type Message struct {
	Role    string
	Content string

	// ToolCalls is populated on assistant messages that invoked tools.
	// When echoed back to the provider as conversation history (as part
	// of a multi-step tool loop), both ToolCalls and any Content must be
	// included so the model sees its own prior decision.
	ToolCalls []ToolCall

	// ToolCallID is set on role == "tool" messages to bind the tool
	// result back to the assistant's tool_call that requested it.
	ToolCallID string

	// Name mirrors the tool/function name on tool-role messages. Some
	// providers require it alongside ToolCallID.
	Name string
}

// ToolSpec describes a single tool the model is allowed to invoke. It is
// the LLM-facing projection of skills.ToolDefinition — kept in this
// package so providers don't need to import internal/skills.
//
// Parameters is a JSON Schema object matching the OpenAI function-calling
// convention.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is a provider-agnostic representation of a model's request to
// invoke a tool. ID is the provider-supplied handle used to correlate
// tool results back to this call. Arguments is the raw JSON object the
// model produced for the tool's parameters — callers pass it directly to
// skills.Registry.Execute.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// CompletionRequest is the input to a completion call.
type CompletionRequest struct {
	Model          string
	Messages       []Message
	MaxTokens      int
	Stream         bool
	EnableThinking bool
	// MaxThinkingTokens is a soft budget for the model's reasoning tokens.
	// Zero means "no explicit budget" — providers pick their own default.
	// Providers that don't expose a thinking budget ignore this field.
	MaxThinkingTokens int
	OnReasoningChunk  func(string)

	// Temperature, when non-nil, pins the sampling temperature for this
	// request. Nil means "provider/model default." Shard invocations
	// propagate the shard's configured temperature here; the trusted
	// pipeline path leaves it nil so routing and per-model defaults win.
	// Pointer rather than float32 so a caller can explicitly ask for
	// deterministic sampling (Temperature = *new(float32)) without being
	// indistinguishable from "use default."
	Temperature *float32

	// Tools, when non-empty, is advertised to the model as the set of
	// callable functions. ToolChoice is the usual OpenAI-style hint:
	// "" / "auto" (model decides), "none" (disable), "required" (force
	// at least one call). Providers that don't support tools ignore
	// both fields.
	Tools      []ToolSpec
	ToolChoice string
}

// CompletionResponse is the output of a completion call.
type CompletionResponse struct {
	Content          string
	ReasoningContent string // populated by formatters that split reasoning post-hoc
	InputTokens      int
	OutputTokens     int
	DecodeMs         float64 // pure LLM generation time from server timings
	Model            string

	// ToolCalls is populated when the model chose to invoke one or more
	// tools instead of (or in addition to) producing text. The pipeline
	// tool loop checks len(ToolCalls) > 0 to decide whether to dispatch
	// and call the provider again.
	ToolCalls []ToolCall

	// FinishReason is the provider's terminal reason ("stop",
	// "tool_calls", "length", ...). Useful for deciding when to break
	// out of the tool loop.
	FinishReason string
}

// Provider is the interface all LLM backends must implement.
type Provider interface {
	// Complete sends messages and waits for the full response.
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
	// CompleteStream sends messages and calls onChunk for each text delta.
	// Returns the final accumulated response when streaming is done.
	CompleteStream(ctx context.Context, req CompletionRequest, onChunk func(string)) (*CompletionResponse, error)
	// Name returns a human-readable provider identifier.
	Name() string
	// HealthCheck verifies the provider is reachable and accepting requests.
	HealthCheck(ctx context.Context) error
}
