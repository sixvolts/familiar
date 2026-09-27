package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/ctxbuild"
	"github.com/familiar/gateway/internal/llm"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
)

// budgetToolResult must (a) head+tail cap a single oversized result,
// (b) let results through while under the cumulative budget, and (c)
// once the running total would exceed the budget, drop the payload for
// a short synthesize notice so the tool loop can't overflow the context
// window into a hard provider 400.
func TestBudgetToolResult(t *testing.T) {
	const perResult = 50 // ~200 bytes
	const budget = 120   // ~480 bytes total across the turn

	// Under budget: a small result passes through and increments used.
	c, used, _ := budgetToolResult("notes", "a short note", false, perResult, budget, 0)
	if c != "a short note" {
		t.Errorf("small result altered: %q", c)
	}
	if used != ctxbuild.EstimateTokens("a short note") {
		t.Errorf("used = %d, want %d", used, ctxbuild.EstimateTokens("a short note"))
	}

	// A single huge result is head+tail capped to ~perResult tokens.
	huge := strings.Repeat("x", 10_000)
	c, _, _ = budgetToolResult("wiki", huge, false, perResult, budget, 0)
	if ctxbuild.EstimateTokens(c) > perResult+20 {
		t.Errorf("result not capped: %d tokens", ctxbuild.EstimateTokens(c))
	}
	if !strings.Contains(c, "elided") {
		t.Errorf("capped result missing elision marker: %q", c[:min(80, len(c))])
	}

	// Already at budget: the next result (larger than the always-kept
	// size) is dropped for a notice.
	c, used2, _ := budgetToolResult("search", strings.Repeat("y", 4000), false, 1000, budget, budget)
	if !strings.Contains(c, "output was omitted") || !strings.Contains(c, "search") {
		t.Errorf("over-budget result not collapsed to a notice: %q", c)
	}
	if used2 <= budget {
		t.Errorf("running total should still advance past budget, got %d", used2)
	}

	// budget <= 0 disables the cumulative check (per-result cap only).
	c, _, _ = budgetToolResult("x", "keep me", false, perResult, 0, 1_000_000)
	if c != "keep me" {
		t.Errorf("zero budget should not collapse: %q", c)
	}
}

// Over the budget, an error and a short result are still kept whole:
// the collapse used to replace a 10-token "409 version conflict" with
// the omission notice, and the model reported the write as done.
func TestBudgetToolResult_KeepsErrorsAndShortResultsOverBudget(t *testing.T) {
	const budget = 120
	errText := `tool "update_page" error: 409 version conflict ` + strings.Repeat("z", 1200)
	if c, _, _ := budgetToolResult("update_page", errText, true, 1000, budget, budget); c != errText {
		t.Errorf("error collapsed over budget: %q", c)
	}
	if c, _, _ := budgetToolResult("update_page", "updated", false, 1000, budget, budget); c != "updated" {
		t.Errorf("short result collapsed over budget: %q", c)
	}
	big := strings.Repeat("y", 4*(smallToolResultTokens+50))
	if c, _, _ := budgetToolResult("read_page", big, false, 1000, budget, budget); !strings.Contains(c, "ran, but its output was omitted") {
		t.Errorf("large result kept over budget: %d bytes", len(c))
	}
}

// sizedSkill serves read_a, read_b and write_c, returning a canned
// result per tool and recording what ran.
type sizedSkill struct {
	results map[string]string
	ran     []string
}

func (s *sizedSkill) Name() string        { return "sized" }
func (s *sizedSkill) Description() string { return "sized results" }
func (s *sizedSkill) Version() string     { return "0.0.1" }
func (s *sizedSkill) Tools() []skills.ToolDefinition {
	var defs []skills.ToolDefinition
	for _, n := range []string{"read_a", "read_b", "write_c"} {
		defs = append(defs, skills.ToolDefinition{Name: n, Description: n, Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	return defs
}
func (s *sizedSkill) Init(json.RawMessage) error { return nil }
func (s *sizedSkill) Close() error               { return nil }
func (s *sizedSkill) Execute(_ context.Context, name string, _ json.RawMessage) (skills.ToolResult, error) {
	s.ran = append(s.ran, name)
	return skills.ToolResult{Content: s.results[name]}, nil
}

// budgetLoop runs the tool loop with a ~614-token tool budget and the
// given completions, one per iteration, then a final text answer.
func budgetLoop(t *testing.T, sk *sizedSkill, iterations ...[]llm.ToolCall) []llm.Message {
	t.Helper()
	reg := skills.NewRegistry()
	if err := reg.Register(sk); err != nil {
		t.Fatal(err)
	}
	pl := makePipelineWithIters(&mockEngine{}, testutil.NewMockLLM(t), reg, 5)
	pl.ctxCfg = ctxbuild.Config{WindowSize: 8192, OutputReservation: 2048, SystemPromptRatio: 0.1,
		MemoryRatio: 0.1, ToolResultRatio: 0.1, MaxToolResultTokens: 2000}
	if got := pl.ctxCfg.Resolve().Tools; got != 614 {
		t.Fatalf("tool budget = %d, want 614", got)
	}
	call := 0
	complete := func(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
		call++
		if call <= len(iterations) {
			return &llm.CompletionResponse{ToolCalls: iterations[call-1]}, nil
		}
		return &llm.CompletionResponse{Content: "done"}, nil
	}
	baseReq := llm.CompletionRequest{Model: "mock-model", Tools: []llm.ToolSpec{{Name: "read_a"}}}
	_, msgs, err := pl.runToolLoop(context.Background(), baseReq, baseReq.Model, "standard", classifier.SearchNone, 0, complete, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func toolResultFor(msgs []llm.Message, id string) string {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

// Parallel calls [read A, read B, write C] where B overflows the budget:
// C used to run anyway, with its outcome hidden behind the omission
// notice. Once a result has been collapsed, later calls are refused.
func TestToolLoop_RefusesCallsOnceTheBudgetIsSpent(t *testing.T) {
	sk := &sizedSkill{results: map[string]string{
		"read_a": strings.Repeat("a", 1600), "read_b": strings.Repeat("b", 1600), "write_c": "updated",
	}}
	msgs := budgetLoop(t, sk, []llm.ToolCall{
		{ID: "1", Name: "read_a", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "read_b", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "write_c", Arguments: json.RawMessage(`{}`)},
	})
	if got := strings.Join(sk.ran, ","); got != "read_a,read_b" {
		t.Errorf("ran %s, want read_a,read_b (write_c refused)", got)
	}
	if r := toolResultFor(msgs, "3"); !strings.Contains(r, "not run") {
		t.Errorf("write_c result = %q, want the not-run notice", r)
	}
}

// The arguments of a tool call are resent with every later iteration
// and count against the budget: a 1000-token update_page body used to
// count as nothing, so the loop kept reading into a context that no
// longer fit. The call itself still runs.
func TestToolLoop_CountsToolCallArguments(t *testing.T) {
	sk := &sizedSkill{results: map[string]string{"read_a": strings.Repeat("a", 800), "write_c": "updated"}}
	body, _ := json.Marshal(map[string]string{"body": strings.Repeat("p", 4000)})
	msgs := budgetLoop(t, sk,
		[]llm.ToolCall{{ID: "1", Name: "write_c", Arguments: body}},
		[]llm.ToolCall{{ID: "2", Name: "read_a", Arguments: json.RawMessage(`{}`)}},
	)
	if got := strings.Join(sk.ran, ","); got != "write_c" {
		t.Errorf("ran %s, want only write_c (its arguments spent the budget)", got)
	}
	if r := toolResultFor(msgs, "1"); r != "updated" {
		t.Errorf("write_c result = %q, want its output", r)
	}
}

// The loop's tool budget reserves the output the request asks for, as
// the prompt was packed. It reserved the 4k default while the trusted
// path grants ~12k, so it allowed more tool output than there was room
// for. Here: an 8k window, 4096 max tokens, a 10% tools zone of 409
// tokens (614 with the default reservation). A 500-token result no
// longer fits.
func TestToolLoop_BudgetReservesTheRequestedOutput(t *testing.T) {
	sk := &sizedSkill{results: map[string]string{"read_a": strings.Repeat("a", 2000)}}
	reg := skills.NewRegistry()
	if err := reg.Register(sk); err != nil {
		t.Fatal(err)
	}
	pl := makePipelineWithIters(&mockEngine{}, testutil.NewMockLLM(t), reg, 5)
	pl.ctxCfg = ctxbuild.Config{WindowSize: 8192, OutputReservation: 2048, SystemPromptRatio: 0.1,
		MemoryRatio: 0.1, ToolResultRatio: 0.1, MaxToolResultTokens: 2000}
	call := 0
	complete := func(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
		call++
		if call == 1 {
			return &llm.CompletionResponse{ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_a", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		return &llm.CompletionResponse{Content: "done"}, nil
	}
	baseReq := llm.CompletionRequest{Model: "mock-model", MaxTokens: 4096, Tools: []llm.ToolSpec{{Name: "read_a"}}}
	_, msgs, err := pl.runToolLoop(context.Background(), baseReq, baseReq.Model, "standard", classifier.SearchNone, 0, complete, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := toolResultFor(msgs, "1"); !strings.Contains(r, "output was omitted") {
		t.Errorf("a 500-token result was kept in a 409-token budget (%d bytes)", len(r))
	}
}

// Only a tool that changed a note or page tells open panels to reload;
// every tool with "note" or "page" in its name did, including reads and
// the web's fetch_page.
func TestToolLoop_NoteChangedOnlyForWrites(t *testing.T) {
	names := []string{"read_page", "search_notes", "fetch_page", "list_pages", "update_page", "append_to_note"}
	sk := &namedSkill{names: names}
	reg := skills.NewRegistry()
	if err := reg.Register(sk); err != nil {
		t.Fatal(err)
	}
	pl := makePipelineWithIters(&mockEngine{}, testutil.NewMockLLM(t), reg, 5)
	var calls []llm.ToolCall
	for i, n := range names {
		calls = append(calls, llm.ToolCall{ID: fmt.Sprint(i), Name: n, Arguments: json.RawMessage(`{}`)})
	}
	call := 0
	complete := func(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
		call++
		if call == 1 {
			return &llm.CompletionResponse{ToolCalls: calls}, nil
		}
		return &llm.CompletionResponse{Content: "done"}, nil
	}
	var effects []string
	onStatus := func(s string) {
		if strings.HasPrefix(s, "__TOOL_EFFECT__:note_changed:") {
			effects = append(effects, strings.TrimSpace(strings.TrimPrefix(s, "__TOOL_EFFECT__:note_changed:")))
		}
	}
	baseReq := llm.CompletionRequest{Model: "mock-model", Tools: []llm.ToolSpec{{Name: "read_page"}}}
	if _, _, err := pl.runToolLoop(context.Background(), baseReq, baseReq.Model, "standard", classifier.SearchNone, 0, complete, onStatus, nil, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(effects, ","); got != "update_page,append_to_note" {
		t.Errorf("note_changed fired for %s, want update_page,append_to_note", got)
	}
}

// namedSkill serves the given tool names, each returning "ok".
type namedSkill struct{ names []string }

func (s *namedSkill) Name() string        { return "named" }
func (s *namedSkill) Description() string { return "named tools" }
func (s *namedSkill) Version() string     { return "0.0.1" }
func (s *namedSkill) Tools() []skills.ToolDefinition {
	var defs []skills.ToolDefinition
	for _, n := range s.names {
		defs = append(defs, skills.ToolDefinition{Name: n, Description: n, Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	return defs
}
func (s *namedSkill) Init(json.RawMessage) error { return nil }
func (s *namedSkill) Close() error               { return nil }
func (s *namedSkill) Execute(context.Context, string, json.RawMessage) (skills.ToolResult, error) {
	return skills.ToolResult{Content: "ok"}, nil
}
