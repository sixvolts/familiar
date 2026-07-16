package classifier

import (
	"testing"

	"github.com/familiar/gateway/internal/config"
)

// TestDefaultResolver_PinsSpecValues verifies the spec defaults
// from CHAT-REARCH §"Effort Level Configuration" match what
// DefaultResolver returns. If the spec changes, this is the
// failing test that flags the drift.
func TestDefaultResolver_PinsSpecValues(t *testing.T) {
	r := DefaultResolver()

	if r.ThinkingFor(ThinkingOff).Enabled {
		t.Error("thinking.off should be Enabled=false")
	}
	if got := r.ThinkingFor(ThinkingHigh).TokenBudget; got != 8000 {
		t.Errorf("thinking.high token_budget = %d, want 8000", got)
	}
	if got := r.MemoryFor(MemoryDeep).TopK; got != 20 {
		t.Errorf("memory.deep top_k = %d, want 20", got)
	}
	if !r.MemoryFor(MemoryNone).Skip {
		t.Error("memory.none should Skip")
	}
	if got := r.SearchFor(SearchShallow).MaxSearches; got != 1 {
		t.Errorf("search.shallow max_searches = %d, want 1", got)
	}
}

// TestResolverFromConfig_PartialOverride exercises the
// merge-onto-defaults path: an operator configuring only one
// level should leave every other level at its default.
func TestResolverFromConfig_PartialOverride(t *testing.T) {
	cfg := config.EffortConfig{
		Thinking: config.EffortThinkingConfig{
			High: config.EffortThinkingLevel{TokenBudget: 12000},
		},
		MemoryDepth: config.EffortMemoryDepthConfig{
			Deep: config.EffortMemoryDepthLevel{TopK: 30},
		},
	}
	r := ResolverFromConfig(cfg)

	if got := r.ThinkingFor(ThinkingHigh).TokenBudget; got != 12000 {
		t.Errorf("override thinking.high = %d, want 12000", got)
	}
	if got := r.ThinkingFor(ThinkingMedium).TokenBudget; got != 2000 {
		t.Errorf("untouched thinking.medium drifted: got %d, want default 2000", got)
	}
	if got := r.MemoryFor(MemoryDeep).TopK; got != 30 {
		t.Errorf("override memory.deep top_k = %d, want 30", got)
	}
	if got := r.MemoryFor(MemoryShallow).TopK; got != 5 {
		t.Errorf("untouched memory.shallow drifted: got %d, want default 5", got)
	}
}

// TestOutput_Validate_RejectsUnknown ensures the parser-side
// validate logic flags malformed levels so the pipeline can fall
// back to ConservativeFallback rather than booting bad data
// through the rest of the turn.
func TestOutput_Validate_RejectsUnknown(t *testing.T) {
	bad := Output{Thinking: "EXTREME", MemoryDepth: MemoryShallow, SearchDepth: SearchNone}
	if bad.Validate() {
		t.Error("should reject unknown thinking level")
	}
	good := ConservativeFallback()
	if !good.Validate() {
		t.Error("ConservativeFallback should validate")
	}
}
