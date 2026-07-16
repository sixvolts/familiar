package backfill

import (
	"context"
	"errors"
	"testing"

	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/sidecar"
)

type fakeSource struct {
	items []Item
	err   error
}

func (f *fakeSource) ListForBackfill(_ context.Context, _ string) ([]Item, error) {
	return f.items, f.err
}

type fakeExtractor struct {
	calls  int
	result [][]sidecar.ExtractedRelationship
	errAt  map[int]error
}

func (f *fakeExtractor) ExtractRelationshipsFromFacts(_ context.Context, _ []string) ([]sidecar.ExtractedRelationship, error) {
	idx := f.calls
	f.calls++
	if err, ok := f.errAt[idx]; ok {
		return nil, err
	}
	if idx < len(f.result) {
		return f.result[idx], nil
	}
	return nil, nil
}

type fakeSink struct {
	rels []memory.Relationship
}

func (f *fakeSink) UpsertRelationships(_ context.Context, rels []memory.Relationship) error {
	f.rels = append(f.rels, rels...)
	return nil
}

func makeItems(n int) []Item {
	out := make([]Item, n)
	for i := 0; i < n; i++ {
		out[i] = Item{
			ID:      "id-" + string(rune('a'+i)),
			Content: "fact " + string(rune('a'+i)),
			UserID:  "owner",
		}
	}
	return out
}

func TestRun_HappyPath(t *testing.T) {
	src := &fakeSource{items: makeItems(5)}
	ext := &fakeExtractor{result: [][]sidecar.ExtractedRelationship{
		{
			{Subject: "a", Predicate: "has_ip", Object: "1.1.1.1"},
			{Subject: "a", Predicate: "runs_os", Object: "ubuntu"},
		},
		{
			{Subject: "b", Predicate: "has_gpu", Object: "gpu-x"},
		},
	}}
	sink := &fakeSink{}

	var snapshots []Progress
	got, err := Run(context.Background(), Deps{src, ext, sink}, Options{UserID: "owner", BatchSize: 3}, func(p Progress) {
		snapshots = append(snapshots, p)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Total != 5 {
		t.Errorf("Total = %d, want 5", got.Total)
	}
	if got.Processed != 5 {
		t.Errorf("Processed = %d, want 5", got.Processed)
	}
	if got.Extracted != 3 {
		t.Errorf("Extracted = %d, want 3", got.Extracted)
	}
	if got.Errors != 0 {
		t.Errorf("Errors = %d, want 0", got.Errors)
	}
	if got.Running {
		t.Error("Running should be false when Run returns")
	}
	if ext.calls != 2 {
		t.Errorf("extractor invoked %d times, want 2 (batch size 3 over 5 items)", ext.calls)
	}
	if len(sink.rels) != 3 {
		t.Errorf("sink saw %d relationships, want 3", len(sink.rels))
	}
	// First batch should anchor provenance at the first item in the batch.
	if sink.rels[0].SourceFact != "id-a" || sink.rels[0].UserID != "owner" {
		t.Errorf("provenance wrong on rel[0]: %+v", sink.rels[0])
	}
	if len(snapshots) < 3 {
		t.Errorf("expected at least 3 progress snapshots (start + 2 batches + final), got %d", len(snapshots))
	}
}

func TestRun_BatchErrorContinues(t *testing.T) {
	src := &fakeSource{items: makeItems(4)}
	ext := &fakeExtractor{
		errAt: map[int]error{0: errors.New("sidecar hiccup")},
		result: [][]sidecar.ExtractedRelationship{
			nil, // first batch errors
			{{Subject: "c", Predicate: "has_ip", Object: "2.2.2.2"}},
		},
	}
	sink := &fakeSink{}

	got, err := Run(context.Background(), Deps{src, ext, sink}, Options{BatchSize: 2}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Processed != 4 {
		t.Errorf("Processed = %d, want 4 (loop must continue past batch error)", got.Processed)
	}
	if got.Errors != 1 {
		t.Errorf("Errors = %d, want 1", got.Errors)
	}
	if got.Extracted != 1 {
		t.Errorf("Extracted = %d, want 1 (only the successful batch landed)", got.Extracted)
	}
	if got.LastError == "" {
		t.Error("LastError should carry the sidecar failure message")
	}
}

func TestRun_ZeroMemories(t *testing.T) {
	src := &fakeSource{items: nil}
	ext := &fakeExtractor{}
	sink := &fakeSink{}

	got, err := Run(context.Background(), Deps{src, ext, sink}, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Total != 0 || got.Processed != 0 || got.Extracted != 0 {
		t.Errorf("expected all zeros, got %+v", got)
	}
	if ext.calls != 0 {
		t.Errorf("extractor should not be called when there are no items; got %d", ext.calls)
	}
	if got.Running {
		t.Error("Running should be false")
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	src := &fakeSource{items: makeItems(10)}
	ext := &fakeExtractor{}
	sink := &fakeSink{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Deps{src, ext, sink}, Options{BatchSize: 2}, nil)
	if err == nil {
		t.Fatal("expected ctx error")
	}
}
