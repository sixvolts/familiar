package skills

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeSkill is a configurable test double for exercising the Registry.
type fakeSkill struct {
	name        string
	description string
	version     string
	tools       []ToolDefinition
	initErr     error
	closeErr    error
	closed      bool
	execCalls   []string
	execResult  ToolResult
	execErr     error
	mu          sync.Mutex
}

func (f *fakeSkill) Name() string                 { return f.name }
func (f *fakeSkill) Description() string          { return f.description }
func (f *fakeSkill) Version() string              { return f.version }
func (f *fakeSkill) Tools() []ToolDefinition      { return f.tools }
func (f *fakeSkill) Init(_ json.RawMessage) error { return f.initErr }
func (f *fakeSkill) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.closeErr
}
func (f *fakeSkill) Execute(_ context.Context, tool string, _ json.RawMessage) (ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, tool)
	return f.execResult, f.execErr
}

func td(name string) ToolDefinition {
	return ToolDefinition{
		Name:        name,
		Description: name + " does a thing",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}
}

func TestRegisterAndDispatch(t *testing.T) {
	r := NewRegistry()
	s := &fakeSkill{
		name:       "weather",
		tools:      []ToolDefinition{td("get_current_weather"), td("get_forecast")},
		execResult: ToolResult{Content: "sunny"},
	}
	if err := r.Register(s); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := r.Execute(context.Background(), "get_current_weather", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Content != "sunny" {
		t.Fatalf("Content = %q", got.Content)
	}
	if len(s.execCalls) != 1 || s.execCalls[0] != "get_current_weather" {
		t.Fatalf("wrong dispatch: %v", s.execCalls)
	}
}

func TestRegisterRejectsDuplicateSkillName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSkill{name: "search", tools: []ToolDefinition{td("web_search")}}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(&fakeSkill{name: "search", tools: []ToolDefinition{td("other_tool")}})
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected duplicate skill error, got %v", err)
	}
}

func TestRegisterRejectsToolNameCollision(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSkill{name: "alpha", tools: []ToolDefinition{td("web_search")}}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(&fakeSkill{name: "beta", tools: []ToolDefinition{td("web_search")}})
	if err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("expected collision error, got %v", err)
	}
	// Critical: beta must NOT have been registered despite the failure.
	if _, ok := r.Get("beta"); ok {
		t.Fatal("beta was registered despite collision")
	}
}

func TestRegisterRejectsEmptyNamesAndNil(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("expected nil-skill error")
	}
	if err := r.Register(&fakeSkill{name: ""}); err == nil {
		t.Fatal("expected empty-name error")
	}
	if err := r.Register(&fakeSkill{name: "bad", tools: []ToolDefinition{{Name: ""}}}); err == nil {
		t.Fatal("expected empty-tool-name error")
	}
}

func TestRegisterPropagatesInitError(t *testing.T) {
	r := NewRegistry()
	boom := errors.New("boom")
	err := r.Register(&fakeSkill{name: "weather", initErr: boom})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("expected init error to propagate, got %v", err)
	}
	if _, ok := r.Get("weather"); ok {
		t.Fatal("failed-init skill should not be registered")
	}
}

func TestExecuteUnknownTool(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "nope", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown-tool error, got %v", err)
	}
}

func TestToolDefinitionsSortedAndCombined(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSkill{name: "weather", tools: []ToolDefinition{td("get_forecast"), td("get_current_weather")}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&fakeSkill{name: "search", tools: []ToolDefinition{td("web_search")}}); err != nil {
		t.Fatal(err)
	}
	defs := r.ToolDefinitions()
	if len(defs) != 3 {
		t.Fatalf("len=%d", len(defs))
	}
	// Alphabetical: get_current_weather, get_forecast, web_search
	want := []string{"get_current_weather", "get_forecast", "web_search"}
	for i, d := range defs {
		if d.Name != want[i] {
			t.Fatalf("defs[%d].Name = %q, want %q", i, d.Name, want[i])
		}
	}
}

func TestSkillNamesSorted(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"weather", "memory", "search"} {
		if err := r.Register(&fakeSkill{name: n, tools: []ToolDefinition{td(n + "_tool")}}); err != nil {
			t.Fatal(err)
		}
	}
	got := r.SkillNames()
	want := []string{"memory", "search", "weather"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SkillNames = %v, want %v", got, want)
		}
	}
}

func TestCloseCallsAllSkills(t *testing.T) {
	r := NewRegistry()
	a := &fakeSkill{name: "a", tools: []ToolDefinition{td("a_tool")}}
	b := &fakeSkill{name: "b", tools: []ToolDefinition{td("b_tool")}}
	_ = r.Register(a)
	_ = r.Register(b)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !a.closed || !b.closed {
		t.Fatalf("both skills should be closed: a=%v b=%v", a.closed, b.closed)
	}
}

func TestCloseReturnsFirstErrorButClosesAll(t *testing.T) {
	r := NewRegistry()
	boom := errors.New("boom")
	a := &fakeSkill{name: "a", tools: []ToolDefinition{td("a_tool")}, closeErr: boom}
	b := &fakeSkill{name: "b", tools: []ToolDefinition{td("b_tool")}}
	_ = r.Register(a)
	_ = r.Register(b)
	err := r.Close()
	if err == nil {
		t.Fatal("expected error from Close")
	}
	if !b.closed {
		t.Fatal("b should still be closed even though a errored")
	}
}

func TestConcurrentExecuteIsSafe(t *testing.T) {
	r := NewRegistry()
	s := &fakeSkill{name: "search", tools: []ToolDefinition{td("web_search")}, execResult: ToolResult{Content: "ok"}}
	_ = r.Register(s)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Execute(context.Background(), "web_search", nil)
			if err != nil {
				t.Errorf("Execute: %v", err)
			}
		}()
	}
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.execCalls) != 50 {
		t.Fatalf("expected 50 calls, got %d", len(s.execCalls))
	}
}

// ── Allowlist filtering (shard support) ──────────────────────────────

func TestFilterToolDefinitions_Basic(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSkill{
		name:  "search",
		tools: []ToolDefinition{td("web_search"), td("news_search")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&fakeSkill{
		name:  "memory",
		tools: []ToolDefinition{td("remember"), td("search_memory")},
	}); err != nil {
		t.Fatal(err)
	}

	got := r.FilterToolDefinitions([]string{"web_search", "search_memory"})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Result is sorted alphabetically.
	if got[0].Name != "search_memory" || got[1].Name != "web_search" {
		t.Errorf("order = %q, %q; want search_memory, web_search", got[0].Name, got[1].Name)
	}
}

func TestFilterToolDefinitions_UnknownSkipped(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSkill{
		name:  "search",
		tools: []ToolDefinition{td("web_search")},
	}); err != nil {
		t.Fatal(err)
	}
	got := r.FilterToolDefinitions([]string{"web_search", "ghost_tool"})
	if len(got) != 1 || got[0].Name != "web_search" {
		t.Errorf("filter with unknown: got %+v, want only web_search", got)
	}
}

func TestFilterToolDefinitions_EmptyOrNil(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSkill{
		name:  "search",
		tools: []ToolDefinition{td("web_search")},
	}); err != nil {
		t.Fatal(err)
	}
	if got := r.FilterToolDefinitions(nil); got != nil {
		t.Errorf("nil allowlist: got %+v, want nil", got)
	}
	if got := r.FilterToolDefinitions([]string{}); got != nil {
		t.Errorf("empty allowlist: got %+v, want nil", got)
	}
}

func TestKnownToolNames(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSkill{
		name:  "search",
		tools: []ToolDefinition{td("web_search"), td("news_search")},
	}); err != nil {
		t.Fatal(err)
	}
	known := r.KnownToolNames()
	if len(known) != 2 {
		t.Fatalf("len = %d, want 2", len(known))
	}
	for _, n := range []string{"web_search", "news_search"} {
		if !known[n] {
			t.Errorf("missing %q", n)
		}
	}
	// Caller mutation must not affect registry state. The returned map
	// is a fresh copy; use it, mutate it, throw it away.
	known["injected"] = true
	if r.KnownToolNames()["injected"] {
		t.Error("KnownToolNames returned the internal map — caller mutation leaked into registry")
	}
}
