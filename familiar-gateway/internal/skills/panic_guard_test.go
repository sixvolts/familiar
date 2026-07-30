package skills

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// panickingSkill stands in for any of the thirteen real skills mishandling
// model-authored JSON — an unchecked index, a nil map write, a type
// assertion on a field the model omitted.
type panickingSkill struct{ tool string }

func (p *panickingSkill) Name() string               { return "panicky" }
func (p *panickingSkill) Description() string        { return "panics on demand" }
func (p *panickingSkill) Version() string            { return "1.0.0" }
func (p *panickingSkill) Init(json.RawMessage) error { return nil }
func (p *panickingSkill) Close() error               { return nil }
func (p *panickingSkill) Tools() []ToolDefinition {
	return []ToolDefinition{{Name: p.tool, Description: "d", Parameters: json.RawMessage(`{}`)}}
}
func (p *panickingSkill) Execute(context.Context, string, json.RawMessage) (ToolResult, error) {
	var m map[string]string // nil map
	m["boom"] = "x"         // panics
	return ToolResult{}, nil
}

// A panicking skill must surface as an ordinary error. On /api/chat
// net/http would absorb the panic, but a research worker, a scheduled
// action and the pipeline's own tool goroutine all run detached — there the
// same panic takes the whole gateway down. Guarding at the dispatch
// boundary covers every path at once.
func TestExecuteContainsSkillPanic(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&panickingSkill{tool: "explode"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	res, err := r.Execute(context.Background(), "explode", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a panicking skill must return an error, not take the process down")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("error should name the panic, got: %v", err)
	}
	if !strings.Contains(err.Error(), "explode") {
		t.Errorf("error should name the tool, got: %v", err)
	}
	if res.Content != "" || res.Error != "" {
		t.Errorf("result should be zero-valued after a panic, got %+v", res)
	}
}

// The registry must stay usable — one bad tool call cannot poison it.
func TestRegistryUsableAfterSkillPanic(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&panickingSkill{tool: "explode"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	healthy := &fakeSkill{
		name: "healthy", version: "1.0.0",
		tools:      []ToolDefinition{td("works")},
		execResult: ToolResult{Content: "fine"},
	}
	if err := r.Register(healthy); err != nil {
		t.Fatalf("Register healthy: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := r.Execute(context.Background(), "explode", json.RawMessage(`{}`)); err == nil {
			t.Fatal("expected an error")
		}
	}
	// A healthy tool still dispatches afterwards.
	res, err := r.Execute(context.Background(), "works", json.RawMessage(`{}`))
	if err != nil {
		t.Errorf("healthy tool failed after a panicking one: %v", err)
	}
	if res.Content != "fine" {
		t.Errorf("healthy tool returned %q, want \"fine\"", res.Content)
	}
}
