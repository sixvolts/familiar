package llm

// Qwen3.5 ModelFormatter — renders conversation state into the
// model's chat-template string and parses the raw /completion
// output. Targets the turbo fork's /completion endpoint where the
// /v1/chat/completions reasoning parser is broken under ROCm 7.2.3.
// See familiar-raw-completion-design.md.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Qwen3.5 special tokens. Token IDs from the design doc; we emit
// them as text since llama.cpp's tokeniser handles the lookup.
const (
	qwenImStart    = "<|im_start|>"
	qwenImEnd      = "<|im_end|>"
	qwenThinkOpen  = "<think>"
	qwenThinkClose = "</think>"
	qwenEOS        = "<|endoftext|>"
)

// Qwen35Formatter implements ModelFormatter for Qwen3.5 chat models.
type Qwen35Formatter struct{}

// NewQwen35Formatter constructs the formatter. Stateless; instances
// are interchangeable.
func NewQwen35Formatter() *Qwen35Formatter { return &Qwen35Formatter{} }

// Name implements ModelFormatter.
func (f *Qwen35Formatter) Name() string { return "qwen35" }

// StopSequences implements ModelFormatter.
func (f *Qwen35Formatter) StopSequences() []string {
	return []string{qwenImEnd, qwenEOS}
}

// DefaultSamplingParams implements ModelFormatter. Values come from
// the Qwen3.5 GGUF metadata + the systemd service config the design
// doc references.
func (f *Qwen35Formatter) DefaultSamplingParams() map[string]any {
	return map[string]any{
		"temperature":    0.6,
		"top_p":          0.95,
		"top_k":          20,
		"repeat_penalty": 1.1,
		"repeat_last_n":  256,
	}
}

func (f *Qwen35Formatter) StreamTags() StreamTagConfig {
	return StreamTagConfig{
		ThinkOpen:  qwenThinkOpen,
		ThinkClose: qwenThinkClose,
		ToolOpen:   "<tool_call>",
		ToolClose:  "</tool_call>",
	}
}

// BuildPrompt implements ModelFormatter. Emits the standard
// <|im_start|>role\n...<|im_end|>\n pattern, with tool definitions
// folded into the system message when tools are present, and a
// trailing generation prompt that either opens <think> or pre-fills
// an empty thinking block to suppress reasoning.
func (f *Qwen35Formatter) BuildPrompt(systemPrompt string, turns []FormatterTurn, tools []ToolSpec, enableThinking bool) string {
	var b strings.Builder

	// System message — always emitted, even when systemPrompt is
	// blank, so the model sees a well-formed turn sequence. Tool
	// definitions get appended under a `# Tools` heading.
	b.WriteString(qwenImStart)
	b.WriteString("system\n")
	if systemPrompt != "" {
		b.WriteString(systemPrompt)
	}
	if len(tools) > 0 {
		if systemPrompt != "" {
			b.WriteString("\n\n")
		}
		b.WriteString(qwenToolPolicy(tools))
	}
	b.WriteString(qwenImEnd)
	b.WriteString("\n")

	// Qwen3's official chat template keeps <think> content only for
	// assistant turns in the current reasoning window — everything
	// after the last user message. Historical assistant turns (those
	// before the last user message) have their thinking stripped:
	// re-emitting it bloats the prompt, busts llama.cpp's KV-cache
	// prefix, and the model was trained expecting it absent. Within
	// an agentic tool loop the assistant turns all sit after the last
	// user message, so their reasoning is correctly retained.
	lastUserIdx := -1
	for i, t := range turns {
		if t.Role == "user" {
			lastUserIdx = i
		}
	}

	// Conversation turns. Tool turns become user turns wrapping the
	// result in <tool_response>.
	for i, t := range turns {
		switch t.Role {
		case "user":
			b.WriteString(qwenImStart)
			b.WriteString("user\n")
			b.WriteString(t.Content)
			b.WriteString(qwenImEnd)
			b.WriteString("\n")
		case "assistant":
			b.WriteString(qwenImStart)
			b.WriteString("assistant\n")
			if t.ReasoningContent != "" && i > lastUserIdx {
				b.WriteString(qwenThinkOpen)
				b.WriteString("\n")
				b.WriteString(t.ReasoningContent)
				b.WriteString("\n")
				b.WriteString(qwenThinkClose)
				b.WriteString("\n\n")
			}
			if t.Content != "" {
				b.WriteString(t.Content)
			}
			// Echo prior tool calls so the model sees its own
			// decision in a multi-turn tool loop.
			for _, tc := range t.ToolCalls {
				b.WriteString(renderToolCall(tc))
			}
			b.WriteString(qwenImEnd)
			b.WriteString("\n")
		case "tool":
			b.WriteString(qwenImStart)
			b.WriteString("user\n")
			b.WriteString("<tool_response>\n")
			b.WriteString(t.Content)
			b.WriteString("\n</tool_response>")
			b.WriteString(qwenImEnd)
			b.WriteString("\n")
		case "system":
			// Mid-conversation system messages are rare; fold
			// them into the prompt in order so a caller that
			// uses them for soft prompts gets the expected
			// effect.
			b.WriteString(qwenImStart)
			b.WriteString("system\n")
			b.WriteString(t.Content)
			b.WriteString(qwenImEnd)
			b.WriteString("\n")
		}
	}

	// Generation prompt. With thinking on, leave the model to fill
	// in its <think>...</think> block before content; with thinking
	// off, prefill an empty thinking block so the model jumps
	// straight to the answer.
	b.WriteString(qwenImStart)
	b.WriteString("assistant\n")
	if enableThinking {
		b.WriteString(qwenThinkOpen)
		b.WriteString("\n")
	} else {
		b.WriteString(qwenThinkOpen)
		b.WriteString("\n\n")
		b.WriteString(qwenThinkClose)
		b.WriteString("\n\n")
	}
	return b.String()
}

// qwenToolPolicy renders the system-prompt block that advertises
// the tool schemas + the expected tool_call shape Qwen3.5 emits.
func qwenToolPolicy(tools []ToolSpec) string {
	var b strings.Builder
	b.WriteString("# Tools\n\nYou have access to the following functions:\n\n<tools>\n")
	b.WriteString(toolSpecsJSON(tools))
	b.WriteString("\n</tools>\n\n")
	b.WriteString("If you choose to call a function ONLY reply in the following format with NO suffix:\n\n")
	b.WriteString("<tool_call>\n<function=example_function_name>\n<parameter=example_parameter_1>\nvalue_1\n</parameter>\n</function>\n</tool_call>")
	return b.String()
}

// renderToolCall serialises an assistant-emitted ToolCall back into
// Qwen3.5's text format so re-rendered history matches what the
// model produced. Arguments is a JSON object — we expand each key
// into a <parameter=key>value</parameter> block.
func renderToolCall(tc ToolCall) string {
	var b strings.Builder
	b.WriteString("\n<tool_call>\n<function=")
	b.WriteString(tc.Name)
	b.WriteString(">\n")
	var args map[string]any
	if len(tc.Arguments) > 0 {
		_ = json.Unmarshal(tc.Arguments, &args)
	}
	for k, v := range args {
		b.WriteString("<parameter=")
		b.WriteString(k)
		b.WriteString(">\n")
		switch val := v.(type) {
		case string:
			b.WriteString(val)
		default:
			out, _ := json.Marshal(val)
			b.WriteString(string(out))
		}
		b.WriteString("\n</parameter>\n")
	}
	b.WriteString("</function>\n</tool_call>\n")
	return b.String()
}

// ParseResponse implements ModelFormatter. The /completion endpoint
// returns the model's output starting right after the generation
// prompt; the opening <think> token lives in the prompt itself, so
// the raw response begins with thinking content directly when
// thinking is on, or with content directly when thinking is off.
//
// Layout when thinking is on:
//
//	{reasoning}</think>\n\n{content}[<tool_call>...</tool_call>]
//
// Layout when thinking is off (prefilled empty <think></think>):
//
//	{content}[<tool_call>...</tool_call>]
func (f *Qwen35Formatter) ParseResponse(raw string) (reasoning, content string, toolCalls []ToolCall, err error) {
	// Strip trailing stop tokens the engine sometimes leaks through.
	raw = strings.TrimSuffix(raw, qwenImEnd)
	raw = strings.TrimSuffix(raw, qwenEOS)
	raw = strings.TrimRight(raw, "\n\r\t ")

	rest := raw
	if idx := strings.Index(raw, qwenThinkClose); idx >= 0 {
		reasoning = strings.TrimSpace(raw[:idx])
		rest = strings.TrimSpace(raw[idx+len(qwenThinkClose):])
	}

	// Extract tool calls (if any), and return the prose surrounding
	// them as the visible content. Multiple calls are concatenated;
	// the orchestrator dispatches them sequentially.
	calls, residual := extractQwenToolCalls(rest)
	toolCalls = calls
	// Protocol tags must NEVER reach the user. A model that emits a
	// stray "</tool_call>" (orphan closer), a JSON tool body we
	// couldn't parse, or any other malformed tag salad would
	// otherwise leak raw markup into the reply (a real Slack incident
	// 2026-06-13: a message that was just "</tool_call>"). Scrub any
	// tool-protocol tags left in the residual; the inner text (if any)
	// survives so we never silently drop genuine prose.
	content = strings.TrimSpace(scrubToolTags(residual))
	return reasoning, content, toolCalls, nil
}

// toolTagRe matches every tool-protocol wrapper tag (Hermes/Qwen
// XML dialect). Used to scrub orphans from user-visible content.
var toolTagRe = regexp.MustCompile(`(?i)</?tool_call>|</?function(=[^>]*)?>|</?parameter(=[^>]*)?>`)

func scrubToolTags(s string) string {
	return toolTagRe.ReplaceAllString(s, "")
}

// extractQwenToolCalls pulls every <tool_call>...</tool_call> block
// out of text, returns the parsed calls + the residual prose with
// the blocks removed. Tolerant of whitespace inside the blocks but
// strict about the wrapping tag shape — anything malformed gets
// left as prose for the user to see (better than dropping output).
func extractQwenToolCalls(text string) ([]ToolCall, string) {
	if !strings.Contains(text, "<tool_call>") {
		return nil, text
	}
	var calls []ToolCall
	var residual strings.Builder
	cursor := 0
	for {
		start := strings.Index(text[cursor:], "<tool_call>")
		if start < 0 {
			residual.WriteString(text[cursor:])
			break
		}
		absStart := cursor + start
		residual.WriteString(text[cursor:absStart])
		end := strings.Index(text[absStart:], "</tool_call>")
		if end < 0 {
			// Malformed — bail out, append the rest as prose.
			residual.WriteString(text[absStart:])
			break
		}
		absEnd := absStart + end + len("</tool_call>")
		block := text[absStart:absEnd]
		if tc, ok := parseQwenToolCall(block); ok {
			calls = append(calls, tc)
		} else {
			// Couldn't parse — keep the raw block visible rather
			// than silently dropping output.
			residual.WriteString(block)
		}
		cursor = absEnd
	}
	return calls, residual.String()
}

var (
	qwenFuncRe  = regexp.MustCompile(`(?s)<function=([^>]+)>(.*)</function>`)
	qwenParamRe = regexp.MustCompile(`(?s)<parameter=([^>]+)>(.*?)</parameter>`)
)

// parseQwenToolCall converts one full <tool_call>...</tool_call>
// block into a structured ToolCall. Arguments are serialized as a
// JSON object so downstream consumers can treat the call the same
// way they do an OpenAI tool_call.
func parseQwenToolCall(block string) (ToolCall, bool) {
	fn := qwenFuncRe.FindStringSubmatch(block)
	if fn == nil {
		// Not the <function=…> XML dialect — many models (Hermes,
		// Gemma, some Qwen builds) put a JSON object inside the
		// <tool_call> wrapper instead: {"name":…,"arguments":{…}}.
		// Try that before giving up so their calls actually execute.
		if tc, ok := parseJSONToolCall(block); ok {
			return tc, true
		}
		return ToolCall{}, false
	}
	name := strings.TrimSpace(fn[1])
	inner := fn[2]

	args := make(map[string]any)
	for _, p := range qwenParamRe.FindAllStringSubmatch(inner, -1) {
		key := strings.TrimSpace(p[1])
		val := strings.TrimSpace(p[2])
		// Try to decode as JSON first (bool, number, array, object).
		// Fall back to the raw string when that fails — the model
		// often emits unquoted strings even for stringly-typed
		// parameters.
		var parsed any
		if err := json.Unmarshal([]byte(val), &parsed); err == nil {
			args[key] = parsed
		} else {
			args[key] = val
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ToolCall{}, false
	}
	return ToolCall{
		ID:        toolCallID(name, raw),
		Name:      name,
		Arguments: raw,
	}, true
}

// parseJSONToolCall handles the JSON-body dialect:
//
//	<tool_call>{"name":"read_page","arguments":{"book_slug":"x"}}</tool_call>
//
// "parameters" is accepted as an alias for "arguments"; arguments may
// arrive as an object or as a JSON-encoded string (some models
// double-encode). Returns false when the body isn't a usable call.
func parseJSONToolCall(block string) (ToolCall, bool) {
	body := block
	if i := strings.Index(body, "<tool_call>"); i >= 0 {
		body = body[i+len("<tool_call>"):]
	}
	if i := strings.Index(body, "</tool_call>"); i >= 0 {
		body = body[:i]
	}
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "{") {
		return ToolCall{}, false
	}
	var parsed struct {
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Name == "" {
		return ToolCall{}, false
	}
	args := parsed.Arguments
	if len(args) == 0 {
		args = parsed.Parameters
	}
	// Normalize: a double-encoded string ("{\"x\":1}") → its object;
	// missing args → empty object.
	if len(args) == 0 {
		args = json.RawMessage("{}")
	} else if args[0] == '"' {
		var inner string
		if err := json.Unmarshal(args, &inner); err == nil {
			args = json.RawMessage(inner)
		}
	}
	if !json.Valid(args) {
		return ToolCall{}, false
	}
	return ToolCall{
		ID:        toolCallID(parsed.Name, args),
		Name:      parsed.Name,
		Arguments: args,
	}, true
}

// toolCallID synthesizes a stable-ish handle. /completion doesn't
// give us IDs; the pipeline tool loop matches results back by
// position anyway, but the ID is non-empty for log clarity.
func toolCallID(name string, args []byte) string {
	return fmt.Sprintf("qwen_%s_%x", name, fnv32(args))
}

// fnv32 is a tiny non-crypto hash used only for tool-call IDs.
// Kept inline so we don't pull hash/fnv just for this.
func fnv32(b []byte) uint32 {
	var h uint32 = 2166136261
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}
