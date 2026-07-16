package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQwen35BuildPromptBasic(t *testing.T) {
	f := NewQwen35Formatter()
	prompt := f.BuildPrompt("you are helpful",
		[]FormatterTurn{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello there", ReasoningContent: "user said hi"},
			{Role: "user", Content: "what's 2+2"},
		}, nil, true)

	for _, want := range []string{
		"<|im_start|>system\nyou are helpful<|im_end|>",
		"<|im_start|>user\nhi<|im_end|>",
		// The assistant turn sits BEFORE the last user turn, so its
		// <think> trace is stripped — only the bare content remains.
		"<|im_start|>assistant\nhello there<|im_end|>",
		"<|im_start|>user\nwhat's 2+2<|im_end|>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n--- full ---\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "user said hi") {
		t.Errorf("historical <think> content should have been stripped:\n%s", prompt)
	}
	// Must end with generation prompt opening <think>.
	if !strings.HasSuffix(prompt, "<|im_start|>assistant\n<think>\n") {
		t.Errorf("prompt should end with generation prompt + <think>; got:\n%s", prompt[len(prompt)-80:])
	}
}

// In an agentic tool loop the assistant turns sit AFTER the last user
// message, so their reasoning must be retained — the model needs to
// see the reasoning that led to each tool call it's continuing from.
func TestQwen35BuildPromptKeepsReasoningInToolLoop(t *testing.T) {
	f := NewQwen35Formatter()
	prompt := f.BuildPrompt("sys",
		[]FormatterTurn{
			{Role: "user", Content: "search for X"},
			{Role: "assistant", Content: "", ReasoningContent: "I should call search"},
			{Role: "tool", Content: "result blob"},
		}, nil, true)
	if !strings.Contains(prompt, "<think>\nI should call search\n</think>") {
		t.Errorf("in-loop assistant reasoning should be retained:\n%s", prompt)
	}
}

func TestQwen35BuildPromptThinkingDisabledPrefillsEmpty(t *testing.T) {
	f := NewQwen35Formatter()
	prompt := f.BuildPrompt("sys", []FormatterTurn{{Role: "user", Content: "hi"}}, nil, false)
	// Generation prompt should land an empty thinking block so the
	// model jumps straight to content.
	if !strings.HasSuffix(prompt, "<|im_start|>assistant\n<think>\n\n</think>\n\n") {
		t.Errorf("expected empty-thinking prefill; got:\n%s", prompt[len(prompt)-100:])
	}
}

func TestQwen35BuildPromptToolsInjected(t *testing.T) {
	f := NewQwen35Formatter()
	tools := []ToolSpec{
		{
			Name:        "search",
			Description: "search the web",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		},
	}
	prompt := f.BuildPrompt("sys", []FormatterTurn{{Role: "user", Content: "find x"}}, tools, true)
	for _, want := range []string{
		"# Tools",
		"<tools>",
		`"name":"search"`,
		"</tools>",
		"<tool_call>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q in tool policy block", want)
		}
	}
}

func TestQwen35BuildPromptToolResponseTurn(t *testing.T) {
	f := NewQwen35Formatter()
	turns := []FormatterTurn{
		{Role: "user", Content: "search"},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "search", Arguments: json.RawMessage(`{"q":"go"}`)}}},
		{Role: "tool", Content: `{"results":["a","b"]}`, Name: "search", ToolCallID: "1"},
	}
	prompt := f.BuildPrompt("sys", turns, nil, true)
	if !strings.Contains(prompt, "<tool_response>\n{\"results\":[\"a\",\"b\"]}\n</tool_response>") {
		t.Errorf("missing tool_response block: %s", prompt)
	}
	if !strings.Contains(prompt, "<tool_call>\n<function=search>") {
		t.Errorf("missing rendered tool_call echo")
	}
	if !strings.Contains(prompt, "<parameter=q>\ngo\n</parameter>") {
		t.Errorf("missing parameter render")
	}
}

func TestQwen35ParseResponseWithThinking(t *testing.T) {
	f := NewQwen35Formatter()
	raw := "let me think about this\n</think>\n\nThe answer is 42."
	reasoning, content, calls, err := f.ParseResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reasoning != "let me think about this" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if content != "The answer is 42." {
		t.Errorf("content = %q", content)
	}
	if len(calls) != 0 {
		t.Errorf("unexpected calls: %+v", calls)
	}
}

func TestQwen35ParseResponseNoThinking(t *testing.T) {
	f := NewQwen35Formatter()
	raw := "just an answer"
	reasoning, content, _, _ := f.ParseResponse(raw)
	if reasoning != "" {
		t.Errorf("reasoning should be empty, got %q", reasoning)
	}
	if content != "just an answer" {
		t.Errorf("content = %q", content)
	}
}

func TestQwen35ParseResponseToolCall(t *testing.T) {
	f := NewQwen35Formatter()
	raw := "thinking...\n</think>\n\nlet me search\n<tool_call>\n<function=search>\n<parameter=q>\ngolang generics\n</parameter>\n<parameter=limit>\n5\n</parameter>\n</function>\n</tool_call>"
	reasoning, content, calls, err := f.ParseResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reasoning != "thinking..." {
		t.Errorf("reasoning = %q", reasoning)
	}
	if content != "let me search" {
		t.Errorf("content = %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "search" {
		t.Errorf("call name = %q", calls[0].Name)
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("args unmarshal: %v", err)
	}
	if args["q"] != "golang generics" {
		t.Errorf("q = %v", args["q"])
	}
	// 5 should have parsed as a number, not a string.
	if n, ok := args["limit"].(float64); !ok || n != 5 {
		t.Errorf("limit = %v (%T), want number 5", args["limit"], args["limit"])
	}
}

func TestQwen35ParseResponseStripsStopTokens(t *testing.T) {
	f := NewQwen35Formatter()
	raw := "answer<|im_end|>"
	_, content, _, _ := f.ParseResponse(raw)
	if content != "answer" {
		t.Errorf("content = %q", content)
	}
}

func TestQwen35ParseResponseMultipleToolCalls(t *testing.T) {
	f := NewQwen35Formatter()
	raw := "</think>\n\n<tool_call>\n<function=a>\n<parameter=x>\n1\n</parameter>\n</function>\n</tool_call>\n<tool_call>\n<function=b>\n<parameter=y>\n2\n</parameter>\n</function>\n</tool_call>"
	_, _, calls, _ := f.ParseResponse(raw)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "a" || calls[1].Name != "b" {
		t.Errorf("call names = %q %q", calls[0].Name, calls[1].Name)
	}
}

func TestQwen35StopSequencesAndDefaults(t *testing.T) {
	f := NewQwen35Formatter()
	stops := f.StopSequences()
	if len(stops) != 2 || stops[0] != "<|im_end|>" || stops[1] != "<|endoftext|>" {
		t.Errorf("stops = %v", stops)
	}
	p := f.DefaultSamplingParams()
	if p["temperature"] != 0.6 || p["top_p"] != 0.95 || p["top_k"] != 20 {
		t.Errorf("defaults = %v", p)
	}
}

func TestParseResponse_OrphanCloserScrubbed(t *testing.T) {
	f := &Qwen35Formatter{}
	// One valid call consumed, plus a stray closer the model emitted.
	raw := "<tool_call>\n<function=read_page>\n<parameter=slug>\nx\n</parameter>\n</function>\n</tool_call>\n</tool_call>"
	_, content, calls, err := f.ParseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "read_page" {
		t.Fatalf("calls = %+v, want one read_page", calls)
	}
	if strings.Contains(content, "tool_call") {
		t.Errorf("leaked protocol tag in content: %q", content)
	}
	if content != "" {
		t.Errorf("content should be empty after scrub, got %q", content)
	}
}

func TestParseResponse_JSONToolCall(t *testing.T) {
	f := &Qwen35Formatter{}
	raw := `<tool_call>{"name":"read_page","arguments":{"book_slug":"family-wiki","page_slug":"biscuits"}}</tool_call>`
	_, content, calls, err := f.ParseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "read_page" {
		t.Fatalf("calls = %+v, want one read_page", calls)
	}
	var got map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &got); err != nil {
		t.Fatalf("args not JSON: %v", err)
	}
	if got["book_slug"] != "family-wiki" || got["page_slug"] != "biscuits" {
		t.Errorf("args = %v", got)
	}
	if content != "" {
		t.Errorf("content should be empty, got %q", content)
	}
}

func TestParseResponse_JSONToolCall_ParametersAlias(t *testing.T) {
	f := &Qwen35Formatter{}
	raw := `prose before <tool_call>{"name":"append_to_page","parameters":{"text":"flour"}}</tool_call>`
	_, content, calls, err := f.ParseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "append_to_page" {
		t.Fatalf("calls = %+v", calls)
	}
	if content != "prose before" {
		t.Errorf("content = %q, want %q", content, "prose before")
	}
}

func TestParseResponse_UnparseableBlockStripsTags(t *testing.T) {
	f := &Qwen35Formatter{}
	// Garbage inside the wrapper: no call extracted, but the wrapper
	// tags must not leak — only the inner text survives.
	raw := "<tool_call>totally not a call</tool_call>"
	_, content, calls, _ := f.ParseResponse(raw)
	if len(calls) != 0 {
		t.Fatalf("expected no calls, got %+v", calls)
	}
	if strings.Contains(content, "tool_call") {
		t.Errorf("leaked tag: %q", content)
	}
}
