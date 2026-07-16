package pipeline

import (
	"testing"

	"github.com/familiar/gateway/internal/memory"
)

func TestDedupeRelationships(t *testing.T) {
	rels := []memory.Relationship{
		{Subject: "operator", Predicate: "owns", Object: "gpu-host"},
		{Subject: "gpu-host", Predicate: "has_gpu", Object: "gpu-x"},
		{Subject: "operator", Predicate: "owns", Object: "gpu-host"}, // dup
		{Subject: "gpu-host", Predicate: "has_ip", Object: "1.1.1.1"},
		{Subject: "gpu-host", Predicate: "has_gpu", Object: "gpu-x"}, // dup
	}
	got := dedupeRelationships(rels)
	if len(got) != 3 {
		t.Fatalf("dedupe len = %d, want 3: %+v", len(got), got)
	}
	// Order should be preserved — first occurrence wins.
	if got[0].Object != "gpu-host" || got[1].Object != "gpu-x" || got[2].Object != "1.1.1.1" {
		t.Errorf("dedupe order wrong: %+v", got)
	}
}

func TestDedupeRelationships_Empty(t *testing.T) {
	if out := dedupeRelationships(nil); out != nil {
		t.Errorf("nil input should return nil, got %+v", out)
	}
	one := []memory.Relationship{{Subject: "a", Predicate: "b", Object: "c"}}
	if out := dedupeRelationships(one); len(out) != 1 {
		t.Errorf("single input should return unchanged, got %+v", out)
	}
}
