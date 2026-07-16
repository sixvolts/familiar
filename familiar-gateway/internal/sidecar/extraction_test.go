package sidecar

import (
	"reflect"
	"testing"
)

func TestParseExtractionResult_FactsAndRelationships(t *testing.T) {
	raw := `{
		"facts": [
			{"content":"Gpu-host IP address is 10.0.0.10","category":"configuration"},
			{"content":"Gpu-host runs Ubuntu 22.04","category":"technical_fact"}
		],
		"relationships": [
			{"subject":"gpu-host","predicate":"has_ip","object":"10.0.0.10"},
			{"subject":"gpu-host","predicate":"runs_os","object":"Ubuntu 22.04"}
		]
	}`
	got, err := parseExtractionResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Facts) != 2 {
		t.Errorf("facts count = %d, want 2", len(got.Facts))
	}
	if got.Facts[0].Content != "Gpu-host IP address is 10.0.0.10" {
		t.Errorf("fact[0] = %q", got.Facts[0].Content)
	}
	if len(got.Relationships) != 2 {
		t.Fatalf("relationships count = %d, want 2", len(got.Relationships))
	}
	if got.Relationships[0] != (ExtractedRelationship{Subject: "gpu-host", Predicate: "has_ip", Object: "10.0.0.10"}) {
		t.Errorf("rel[0] = %+v", got.Relationships[0])
	}
}

func TestParseExtractionResult_EmptyArrays(t *testing.T) {
	got, err := parseExtractionResult(`{"facts":[],"relationships":[]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Facts) != 0 || len(got.Relationships) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
}

func TestParseExtractionResult_WrappedInPreamble(t *testing.T) {
	raw := "Here's the extraction:\n```json\n" +
		`{"facts":[{"content":"x","category":"issue"}],"relationships":[]}` +
		"\n```\nlet me know if you need more."
	got, err := parseExtractionResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Facts) != 1 || got.Facts[0].Content != "x" {
		t.Errorf("unexpected facts: %+v", got.Facts)
	}
}

func TestParseExtractionResult_LegacyArrayFallback(t *testing.T) {
	raw := `[{"content":"legacy fact","category":"issue"}]`
	got, err := parseExtractionResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Facts) != 1 || got.Facts[0].Content != "legacy fact" {
		t.Errorf("legacy fallback failed: %+v", got.Facts)
	}
	if len(got.Relationships) != 0 {
		t.Errorf("legacy response should have no relationships")
	}
}

func TestParseExtractionResult_NoJSONErrors(t *testing.T) {
	_, err := parseExtractionResult("the model wandered off script")
	if err == nil {
		t.Fatal("expected error on non-JSON payload")
	}
}

func TestParseExtractionResult_MalformedJSONErrors(t *testing.T) {
	_, err := parseExtractionResult(`{"facts":[{"content":oops}]}`)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestParseRelationshipResult_Happy(t *testing.T) {
	raw := `{"relationships":[
		{"subject":"gpu-host","predicate":"has_ip","object":"10.0.0.10"},
		{"subject":"gpu-host","predicate":"runs_os","object":"Ubuntu 22.04"}
	]}`
	got, err := parseRelationshipResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Subject != "gpu-host" || got[0].Predicate != "has_ip" || got[0].Object != "10.0.0.10" {
		t.Errorf("triple[0] = %+v", got[0])
	}
}

func TestParseRelationshipResult_Empty(t *testing.T) {
	got, err := parseRelationshipResult(`{"relationships":[]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got == nil {
		t.Error("expected non-nil empty slice so callers can range without a nil check")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestParseRelationshipResult_Wrapped(t *testing.T) {
	raw := "Sure, here's the extraction:\n```json\n" +
		`{"relationships":[{"subject":"a","predicate":"has_ip","object":"1.1.1.1"}]}` +
		"\n```\n"
	got, err := parseRelationshipResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Subject != "a" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestParseRelationshipResult_NoJSONErrors(t *testing.T) {
	if _, err := parseRelationshipResult("the model went off script"); err == nil {
		t.Fatal("expected error on non-JSON")
	}
}

func TestParseRelationshipResult_MalformedErrors(t *testing.T) {
	if _, err := parseRelationshipResult(`{"relationships":[{"subject":oops}]}`); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestExtractionResult_StructRoundtrip(t *testing.T) {
	// Defensive: ensure Go's reflect-based DeepEqual still matches the
	// struct's comparison semantics so future refactors don't silently
	// break test assertions using ==.
	a := ExtractedRelationship{Subject: "a", Predicate: "b", Object: "c"}
	b := ExtractedRelationship{Subject: "a", Predicate: "b", Object: "c"}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("identical triples should compare equal")
	}
}
