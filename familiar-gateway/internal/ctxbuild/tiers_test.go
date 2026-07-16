package ctxbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTierFor(t *testing.T) {
	cases := map[string]string{
		"trivial":        "trivial",
		"knowledge":      "knowledge",
		"analytical":     "reasoning",
		"deep_reasoning": "deep",
		"":               "knowledge", // unknown falls back
		"garbage":        "knowledge",
	}
	for complexity, want := range cases {
		if got := TierFor(complexity).Name; got != want {
			t.Errorf("TierFor(%q).Name = %q, want %q", complexity, got, want)
		}
	}

	// Trivial must not inject tools or burn a thinking budget.
	triv := TierFor("trivial")
	if triv.InjectTools || triv.IncludeToolPolicy || triv.ThinkingBudget != 0 {
		t.Errorf("trivial tier leaks tools/thinking: %+v", triv)
	}

	// Deep must inject tools and carry the largest thinking budget.
	deep := TierFor("deep_reasoning")
	if !deep.InjectTools || deep.ThinkingBudget < TierFor("analytical").ThinkingBudget {
		t.Errorf("deep tier budget regression: %+v", deep)
	}
}

func writePromptDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"base.md":           "BASE",
		"tier_trivial.md":   "TRIVIAL",
		"tier_knowledge.md": "KNOWLEDGE",
		"tier_reasoning.md": "REASONING",
		"tier_deep.md":      "DEEP",
		"tool_policy.md":    "TOOLS",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestPromptStoreAssemble(t *testing.T) {
	dir := writePromptDir(t)
	store, err := NewPromptStore(dir, "LEGACY")
	if err != nil {
		t.Fatalf("NewPromptStore: %v", err)
	}
	if !store.Loaded() {
		t.Fatal("store should report Loaded after successful read")
	}

	trivial := store.Assemble(TierFor("trivial"))
	if !strings.Contains(trivial, "BASE") || !strings.Contains(trivial, "TRIVIAL") {
		t.Errorf("trivial missing base/overlay: %q", trivial)
	}
	if strings.Contains(trivial, "TOOLS") {
		t.Errorf("trivial must not include tool policy: %q", trivial)
	}

	knowledge := store.Assemble(TierFor("knowledge"))
	for _, want := range []string{"BASE", "KNOWLEDGE", "TOOLS"} {
		if !strings.Contains(knowledge, want) {
			t.Errorf("knowledge missing %q: %q", want, knowledge)
		}
	}

	deep := store.Assemble(TierFor("deep_reasoning"))
	for _, want := range []string{"BASE", "DEEP", "TOOLS"} {
		if !strings.Contains(deep, want) {
			t.Errorf("deep missing %q: %q", want, deep)
		}
	}
}

func TestPromptStoreMissingDirFallsBack(t *testing.T) {
	store, err := NewPromptStore("/nonexistent/familiar/prompts/xyz", "LEGACY PROMPT")
	if err != nil {
		t.Fatalf("NewPromptStore: %v", err)
	}
	if store.Loaded() {
		t.Fatal("Loaded should be false when dir does not exist")
	}
	if got := store.Assemble(TierFor("knowledge")); got != "LEGACY PROMPT" {
		t.Errorf("fallback = %q, want %q", got, "LEGACY PROMPT")
	}
}

func TestPromptStoreEmptyDirReturnsFallback(t *testing.T) {
	store, err := NewPromptStore("", "LEGACY")
	if err != nil {
		t.Fatalf("NewPromptStore: %v", err)
	}
	if store.Loaded() {
		t.Fatal("Loaded should be false when dir empty")
	}
	if got := store.Assemble(TierFor("trivial")); got != "LEGACY" {
		t.Errorf("fallback = %q, want LEGACY", got)
	}
}

func TestPromptStoreMissingBaseErrors(t *testing.T) {
	dir := t.TempDir()
	// Directory exists but base.md does not.
	_, err := NewPromptStore(dir, "FB")
	if err == nil {
		t.Fatal("expected error when base.md missing")
	}
}

// TestShippedPromptsIncludeWikiGuidance guards against the
// stranded-guidance bug (2026-06-13): the wiki "go direct, don't
// browse" guidance once lived ONLY in system_prompt.md, which is the
// monolithic FALLBACK — never used once a tiered prompt dir loads. A
// tiered deploy then gave the model zero wiki guidance and it burned
// tool iterations browsing. The guidance must live in the tiered
// assembly (tool_policy.md). This loads the REAL shipped prompts and
// asserts every tool-bearing tier carries it.
func TestShippedPromptsIncludeWikiGuidance(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "prompts", "tiers")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("shipped prompts not found at %s: %v", dir, err)
	}
	store, err := NewPromptStore(dir, "FALLBACK-SENTINEL")
	if err != nil {
		t.Fatalf("NewPromptStore: %v", err)
	}
	if !store.Loaded() {
		t.Fatal("tiered prompt store did not load the shipped prompts")
	}
	for _, complexity := range []string{"knowledge", "technical", "deep_reasoning"} {
		assembled := store.Assemble(TierFor(complexity))
		if strings.Contains(assembled, "FALLBACK-SENTINEL") {
			t.Fatalf("%s: assembled the fallback, not the tiered prompts", complexity)
		}
		// The model must learn to go direct on wiki pages from the
		// TIERED prompt, not the stranded fallback.
		for _, marker := range []string{"read_page", "don't browse", "grocery-list"} {
			if !strings.Contains(assembled, marker) {
				t.Errorf("%s tier prompt is missing wiki guidance %q — is it stranded in system_prompt.md again?", complexity, marker)
			}
		}
	}
}
