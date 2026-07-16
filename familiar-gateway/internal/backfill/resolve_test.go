package backfill

import (
	"context"
	"errors"
	"testing"

	"github.com/familiar/gateway/internal/sidecar"
)

type fakeEntitySource struct {
	entities map[string]int
	err      error
}

func (f *fakeEntitySource) ListDistinctEntities(_ context.Context, _ string) (map[string]int, error) {
	return f.entities, f.err
}

type fakeGrouper struct {
	calls   int
	results [][]sidecar.EntityGroup
	errAt   map[int]error
	seen    [][]string
}

func (f *fakeGrouper) GroupEntities(_ context.Context, names []string) ([]sidecar.EntityGroup, error) {
	idx := f.calls
	f.calls++
	f.seen = append(f.seen, append([]string{}, names...))
	if err, ok := f.errAt[idx]; ok {
		return nil, err
	}
	if idx < len(f.results) {
		return f.results[idx], nil
	}
	return nil, nil
}

type fakeMerger struct {
	merges       []mergeCall
	rowsPerMerge int64
	errOn        string // canonical name that should error
}

type mergeCall struct {
	canonical string
	aliases   []string
}

func (f *fakeMerger) MergeEntity(_ context.Context, _ string, canonical string, aliases []string) (int64, error) {
	if f.errOn != "" && canonical == f.errOn {
		return 0, errors.New("merge failed")
	}
	f.merges = append(f.merges, mergeCall{canonical: canonical, aliases: append([]string{}, aliases...)})
	n := f.rowsPerMerge
	if n == 0 {
		n = int64(len(aliases) * 2)
	}
	return n, nil
}

func TestResolve_HappyPath(t *testing.T) {
	src := &fakeEntitySource{entities: map[string]int{
		"host-a": 30, "rune": 5, "workstation": 3,
		"gpu-host": 20, "inference server": 2,
	}}
	grouper := &fakeGrouper{results: [][]sidecar.EntityGroup{
		{
			{Canonical: "host-a", Aliases: []string{"rune", "workstation"}},
			{Canonical: "gpu-host", Aliases: []string{"inference server"}},
		},
	}}
	merger := &fakeMerger{rowsPerMerge: 4}

	got, err := Resolve(context.Background(), ResolveDeps{src, grouper, merger}, ResolveOptions{UserID: "owner", ChunkSize: 100}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.TotalEntities != 5 {
		t.Errorf("TotalEntities = %d, want 5", got.TotalEntities)
	}
	if got.GroupsProposed != 2 {
		t.Errorf("GroupsProposed = %d, want 2", got.GroupsProposed)
	}
	if got.EntitiesMerged != 3 {
		t.Errorf("EntitiesMerged = %d, want 3 (2+1 aliases)", got.EntitiesMerged)
	}
	if got.RowsRewritten != 8 {
		t.Errorf("RowsRewritten = %d, want 8", got.RowsRewritten)
	}
	if len(merger.merges) != 2 {
		t.Errorf("merger saw %d calls, want 2", len(merger.merges))
	}
	// Most-referenced entity (host-a, 30) should appear in the first chunk.
	if len(grouper.seen) == 0 || grouper.seen[0][0] != "host-a" {
		t.Errorf("expected host-a as first entity in first chunk, got %v", grouper.seen)
	}
}

func TestResolve_MergeErrorContinues(t *testing.T) {
	src := &fakeEntitySource{entities: map[string]int{"a": 5, "b": 3, "c": 2}}
	grouper := &fakeGrouper{results: [][]sidecar.EntityGroup{{
		{Canonical: "a", Aliases: []string{"b"}},
		{Canonical: "c", Aliases: []string{"x"}},
	}}}
	merger := &fakeMerger{errOn: "a"}

	got, err := Resolve(context.Background(), ResolveDeps{src, grouper, merger}, ResolveOptions{ChunkSize: 100}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Errors != 1 {
		t.Errorf("Errors = %d, want 1", got.Errors)
	}
	if got.EntitiesMerged != 1 {
		t.Errorf("EntitiesMerged = %d, want 1 (only c landed)", got.EntitiesMerged)
	}
	if len(merger.merges) != 1 || merger.merges[0].canonical != "c" {
		t.Errorf("wanted single successful merge of c, got %+v", merger.merges)
	}
}

func TestResolve_MinRefsFilter(t *testing.T) {
	src := &fakeEntitySource{entities: map[string]int{
		"host-a": 30, "rune": 5, "noise1": 1, "noise2": 1,
	}}
	grouper := &fakeGrouper{}
	merger := &fakeMerger{}

	_, err := Resolve(context.Background(), ResolveDeps{src, grouper, merger}, ResolveOptions{MinRefs: 2, ChunkSize: 100}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(grouper.seen) != 1 {
		t.Fatalf("expected one grouping call, got %d", len(grouper.seen))
	}
	for _, n := range grouper.seen[0] {
		if n == "noise1" || n == "noise2" {
			t.Errorf("MinRefs=2 should have dropped %q", n)
		}
	}
}

func TestResolve_Chunking(t *testing.T) {
	ents := make(map[string]int, 250)
	for i := 0; i < 250; i++ {
		ents[string(rune('a'+(i%26)))+string(rune('0'+(i/26)))] = i + 1
	}
	src := &fakeEntitySource{entities: ents}
	grouper := &fakeGrouper{}
	merger := &fakeMerger{}

	_, err := Resolve(context.Background(), ResolveDeps{src, grouper, merger}, ResolveOptions{ChunkSize: 60}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// 250 entities / 60 per chunk = 5 chunks.
	if grouper.calls != 5 {
		t.Errorf("expected 5 grouper calls, got %d", grouper.calls)
	}
}
