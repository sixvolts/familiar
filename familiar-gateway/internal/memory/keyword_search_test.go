package memory

import (
	"context"
	"testing"
)

// With no query vector (the embedder is down) HybridSearch runs its
// full-text arm alone, with the same visibility rules. It returned
// nothing, so an embedder outage removed memory from every turn.
func TestHybridSearch_NoVectorIsKeywordOnly(t *testing.T) {
	s := setupMemoryStore(t)
	insertFor(t, s, "u1", "the dog is named Biscuit", "", axisVec(1))
	insertFor(t, s, "u2", "the dog is named Pepper", "", axisVec(1))
	declareShardFor(t, s, "u1", "vet", "shard:vet", "isolated")
	insertFor(t, s, "u1", "the dog named Biscuit got shots at the vet", "shard:vet", axisVec(2))

	res, err := s.HybridSearch(context.Background(), "dog named", nil, 5, 0.5, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Content != "the dog is named Biscuit" {
		t.Fatalf("keyword-only results = %+v, want u1's top-level row alone", res)
	}
	if res[0].FusedScore <= 0 {
		t.Errorf("FusedScore = %v, want the text arm's rank score", res[0].FusedScore)
	}
	if res, err := s.HybridSearch(context.Background(), "", nil, 5, 0.5, "u1"); err != nil || len(res) != 0 {
		t.Errorf("empty query: %v, %v; want nothing", res, err)
	}
}
