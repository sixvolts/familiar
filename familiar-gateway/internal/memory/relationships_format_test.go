package memory

import (
	"strings"
	"testing"
)

func TestFormatLines_Empty(t *testing.T) {
	if got := FormatLines(nil); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
	if got := FormatLines([]Relationship{}); got != nil {
		t.Errorf("empty slice should return nil, got %v", got)
	}
}

func TestFormatLines_FewTriples_FlatFormat(t *testing.T) {
	rels := []Relationship{
		{Subject: "gpu-host", Predicate: "has_ip", Object: "10.0.0.10"},
		{Subject: "gpu-host", Predicate: "has_ip", Object: "10.0.0.1"},
	}
	got := FormatLines(rels)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != "gpu-host -> has_ip -> 10.0.0.10" {
		t.Errorf("line[0] should be bare triple: %q", got[0])
	}
}

func TestFormatLines_GroupsByPredicate(t *testing.T) {
	rels := []Relationship{
		{Subject: "gpu-host", Predicate: "has_ip", Object: "10.0.0.10"},
		{Subject: "host-b", Predicate: "has_ip", Object: "10.0.0.104"},
		{Subject: "host-c", Predicate: "has_ip", Object: "10.0.0.102"},
		{Subject: "gpu-host", Predicate: "has_gpu", Object: "example gpu"},
	}
	got := FormatLines(rels)
	if len(got) == 0 {
		t.Fatal("expected non-empty output")
	}
	if got[0] != "## has_ip" {
		t.Errorf("expected predicate header, got %q", got[0])
	}
	// Check that grouped lines use compact format
	if !strings.HasPrefix(got[1], "- gpu-host -> ") {
		t.Errorf("expected compact grouped line, got %q", got[1])
	}
	// Check second predicate group exists
	found := false
	for _, l := range got {
		if l == "## has_gpu" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected has_gpu predicate group in output: %v", got)
	}
}
