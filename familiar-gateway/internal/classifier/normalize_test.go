package classifier

import "testing"

// Near misses map onto the canonical levels instead of failing Validate.
func TestNormalize(t *testing.T) {
	got := Output{Thinking: " Low", MemoryDepth: "DEEP", SearchDepth: "Shallow "}.Normalize()
	if got.Thinking != ThinkingLow || got.MemoryDepth != MemoryDeep || got.SearchDepth != SearchShallow {
		t.Errorf("case/space not normalized: %+v", got)
	}
	got = Output{Thinking: "none", MemoryDepth: "off", SearchDepth: "off"}.Normalize()
	if got.Thinking != ThinkingOff || got.MemoryDepth != MemoryNone || got.SearchDepth != SearchNone {
		t.Errorf("the none/off swap not mapped: %+v", got)
	}
	if !got.Validate() {
		t.Error("a normalized near miss should validate")
	}
}

// One bad field no longer throws away the verdict: the valid fields and
// the condensed query survive, and the bad field errs expensive.
func TestRepairKeepsValidFields(t *testing.T) {
	in := Output{Thinking: "EXTREME", MemoryDepth: MemoryShallow, SearchDepth: SearchDeep, CondensedQuery: "go 1.26 release notes"}
	got, repaired := in.Repair()
	if !repaired || got.Source != SourceUnparsed {
		t.Errorf("repaired=%v source=%q; a repair must stay counted as a fallback", repaired, got.Source)
	}
	if got.Thinking != ThinkingHigh || got.MemoryDepth != MemoryShallow || got.SearchDepth != SearchDeep ||
		got.CondensedQuery != "go 1.26 release notes" {
		t.Errorf("repair = %+v, want thinking high and everything else kept", got)
	}
	if _, repaired := (Output{Thinking: ThinkingLow, MemoryDepth: MemoryNone, SearchDepth: SearchNone}).Repair(); repaired {
		t.Error("a valid verdict was repaired")
	}
}

// The conservative fallback must not veto web search: SearchNone sets
// webSearchDisabled.
func TestConservativeFallbackDoesNotVetoSearch(t *testing.T) {
	if s := ConservativeFallback().SearchDepth; s == SearchNone {
		t.Error("ConservativeFallback vetoes web search")
	}
}
