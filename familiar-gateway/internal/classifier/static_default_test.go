package classifier

import (
	"encoding/json"
	"testing"
)

// StaticDefault is what every "we learned nothing" path now uses. The
// point of it is to be CHEAP: using ConservativeFallback there meant a
// saturated box answered "too busy to classify" by demanding maximum
// work from itself.
func TestStaticDefaultIsCheaperThanConservative(t *testing.T) {
	s, c := StaticDefault(), ConservativeFallback()
	r := DefaultResolver()

	if !s.Validate() {
		t.Fatal("StaticDefault must validate")
	}

	// Thinking headroom must be strictly smaller.
	if got, want := r.ThinkingFor(s.Thinking).TokenBudget, r.ThinkingFor(c.Thinking).TokenBudget; got >= want {
		t.Errorf("static thinking budget %d must be < conservative %d", got, want)
	}
	// Retrieval must be narrower, since memory depth is where the
	// round-trip cost actually is.
	sm, cm := r.MemoryFor(s.MemoryDepth), r.MemoryFor(c.MemoryDepth)
	if sm.Skip {
		t.Error("static default should still retrieve — skipping memory loses grounding")
	}
	if sm.TopK >= cm.TopK {
		t.Errorf("static TopK %d must be < conservative %d", sm.TopK, cm.TopK)
	}
}

// SearchNone is a hard veto downstream (it sets webSearchDisabled), so a
// classifier outage must not silently make web search impossible — that
// turns "look up the latest X" into a confidently stale answer. One
// permitted call lets the chat model decide; it does not force a search.
func TestStaticDefaultDoesNotVetoSearch(t *testing.T) {
	s := StaticDefault()
	if s.SearchDepth == SearchNone {
		t.Fatal("StaticDefault must not veto web search")
	}
	if b := DefaultResolver().SearchFor(s.SearchDepth); b.Skip {
		t.Fatal("StaticDefault's search depth must not resolve to Skip")
	}
	// ...but it must stay modest.
	if b := DefaultResolver().SearchFor(s.SearchDepth); b.MaxSearches > 1 {
		t.Errorf("static search budget %d is too generous for a fallback", b.MaxSearches)
	}
}

// Provenance must be distinguishable, or the fallback rate stays
// unknowable — which is the state this shipped in.
func TestVerdictSourcesAreDistinguishable(t *testing.T) {
	if StaticDefault().Source != SourceStatic {
		t.Error("StaticDefault must be stamped SourceStatic")
	}
	if ConservativeFallback().Source != SourceUnparsed {
		t.Error("ConservativeFallback must be stamped SourceUnparsed")
	}
	if StaticDefault().FromModel() || ConservativeFallback().FromModel() {
		t.Error("no fallback may claim to be a model verdict")
	}
	if (Output{Source: SourceModel}).FromModel() != true {
		t.Error("SourceModel must report FromModel")
	}
}

// Source must never be decodable from model output — otherwise a
// classifier could claim its own guess was authoritative.
func TestSourceIsNotWireDecodable(t *testing.T) {
	var o Output
	if err := jsonUnmarshal([]byte(`{"thinking":"low","memory_depth":"none","search_depth":"none","Source":"model","source":"model"}`), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if o.Source != "" {
		t.Errorf("Source decoded from the wire as %q; it must be server-stamped only", o.Source)
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
