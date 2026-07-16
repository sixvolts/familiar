package sidecar

import (
	"testing"

	"github.com/familiar/gateway/internal/classifier"
)

// parseClassifierOutput mirrors parseRoutingDecision's lenient
// shape: model output may be wrapped in prose, fences, or have
// trailing text. Extract from first `{` to last `}` and decode.

func TestParseClassifierOutput_PlainJSON(t *testing.T) {
	raw := `{"thinking":"medium","memory_depth":"shallow","search_depth":"none","tools":["notes_read"]}`
	out, err := parseClassifierOutput(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if out.Thinking != classifier.ThinkingMedium {
		t.Errorf("thinking = %q, want medium", out.Thinking)
	}
	if out.MemoryDepth != classifier.MemoryShallow {
		t.Errorf("memory_depth = %q, want shallow", out.MemoryDepth)
	}
	if out.SearchDepth != classifier.SearchNone {
		t.Errorf("search_depth = %q, want none", out.SearchDepth)
	}
	if len(out.Tools) != 1 || out.Tools[0] != "notes_read" {
		t.Errorf("tools = %v, want [notes_read]", out.Tools)
	}
}

func TestParseClassifierOutput_FencedAndProseWrapped(t *testing.T) {
	raw := "Here you go:\n```json\n{\"thinking\":\"high\",\"memory_depth\":\"deep\",\"search_depth\":\"shallow\",\"tools\":[]}\n```\n"
	out, err := parseClassifierOutput(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if out.Thinking != classifier.ThinkingHigh {
		t.Errorf("thinking = %q, want high", out.Thinking)
	}
	if !out.Validate() {
		t.Errorf("expected valid output, got %+v", out)
	}
}

func TestParseClassifierOutput_NoBraces(t *testing.T) {
	if _, err := parseClassifierOutput("nope"); err == nil {
		t.Error("expected error on no JSON object")
	}
}

func TestParseClassifierOutput_MalformedFallsThroughValidator(t *testing.T) {
	// Parses fine but Validate() rejects unknown level. The
	// Client.Classify wrapper falls back to ConservativeFallback
	// when this happens; this test just locks in that the parser
	// itself doesn't reject unknowns (so the validator is the
	// single source of truth on level acceptance).
	raw := `{"thinking":"EXTREME","memory_depth":"shallow","search_depth":"none","tools":[]}`
	out, err := parseClassifierOutput(raw)
	if err != nil {
		t.Fatalf("parser should accept structurally-valid JSON: %v", err)
	}
	if out.Validate() {
		t.Error("Validate should reject unknown thinking level")
	}
}
