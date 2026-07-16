package wikiknowledge

// Pipeline tests run against fake collaborators — no engine, no
// sidecar, no DB. They pin down the contract that matters:
//   * Stale cleanup runs first, with the right (sourceType,
//     sourceRef, scopeTag) tuple.
//   * Each extracted fact arrives at CommitFacts carrying the
//     book scope_tag, source_ref, source_type.
//   * Extracted relationships and resolved-link triples both go
//     through UpsertRelationships with scope_tag set.
//   * Broken outbound links don't emit links_to triples.
//   * OnPageDeleted fires DeleteMemoriesBySource; relationships
//     are NOT swept (cross-page, intentional).

import (
	"context"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/sidecar"

	pb "github.com/familiar/gateway/proto/engine"
)

// ── Fakes ─────────────────────────────────────────────────────────

type fakeEngine struct {
	commits [][]*pb.FactProto
}

func (f *fakeEngine) CommitFacts(_ context.Context, _ string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error) {
	cp := make([]*pb.FactProto, len(facts))
	copy(cp, facts)
	f.commits = append(f.commits, cp)
	return &pb.CommitFactsResponse{}, nil
}

type fakeSidecar struct {
	result     sidecar.ExtractionResult
	calls      int
	largeCalls int
}

func (f *fakeSidecar) ExtractFacts(_ context.Context, _ []sidecar.Turn) (sidecar.ExtractionResult, error) {
	f.calls++
	return f.result, nil
}

func (f *fakeSidecar) ExtractFactsLarge(_ context.Context, _ []sidecar.Turn) (sidecar.ExtractionResult, error) {
	f.largeCalls++
	return f.result, nil
}

type fakeMem struct {
	deletes []deleteCall
}
type deleteCall struct{ sourceType, sourceRef, scopeTag string }

func (f *fakeMem) DeleteMemoriesBySource(_ context.Context, st, sr, sg string) (int64, error) {
	f.deletes = append(f.deletes, deleteCall{st, sr, sg})
	return 0, nil
}

type fakeRel struct {
	upserts [][]memory.Relationship
}

func (f *fakeRel) UpsertRelationships(_ context.Context, rels []memory.Relationship) error {
	cp := make([]memory.Relationship, len(rels))
	copy(cp, rels)
	f.upserts = append(f.upserts, cp)
	return nil
}

func newPipelineWith(eng *fakeEngine, sc *fakeSidecar, mem *fakeMem, rel *fakeRel) *Pipeline {
	deps := Deps{}
	if eng != nil {
		deps.Engine = eng
	}
	if sc != nil {
		deps.Sidecar = sc
	}
	if mem != nil {
		deps.MemoryStore = mem
	}
	if rel != nil {
		deps.RelStore = rel
	}
	deps.Embedder = func(_ context.Context, _ string) ([]float32, error) {
		return []float32{0.1, 0.2}, nil
	}
	return New(deps)
}

func sampleEvent() SaveEvent {
	pageID := "target-page-id"
	resolved := pageID
	return SaveEvent{
		BookID:   "book-uuid",
		BookSlug: "engineering",
		PageID:   "page-uuid",
		PageSlug: "deploy-process",
		UserID:   "operator",
		Title:    "Deploy process",
		Content: "We deploy via GitHub Actions to staging first, then production. " +
			"The pipeline runs on every push to main.",
		Links: []admin.PageLink{
			{TargetBookSlug: "", TargetPageSlug: "ci-pipeline", TargetPageID: &resolved},
			{TargetBookSlug: "ops", TargetPageSlug: "rollback", TargetPageID: nil}, // broken
		},
	}
}

// ── Tests ─────────────────────────────────────────────────────────

func TestNew_ReturnsNilWhenNoCollaborators(t *testing.T) {
	if p := New(Deps{}); p != nil {
		t.Errorf("expected nil pipeline when no deps wired, got %#v", p)
	}
}

func TestOnPageSaved_StaleCleanupRunsFirst(t *testing.T) {
	mem := &fakeMem{}
	p := newPipelineWith(nil, nil, mem, nil)
	p.OnPageSaved(context.Background(), sampleEvent())
	if len(mem.deletes) != 1 {
		t.Fatalf("expected 1 stale-cleanup call, got %d", len(mem.deletes))
	}
	d := mem.deletes[0]
	if d.sourceType != "wiki_page" {
		t.Errorf("source_type = %q, want wiki_page", d.sourceType)
	}
	if d.sourceRef != "engineering/deploy-process" {
		t.Errorf("source_ref = %q, want bookSlug/pageSlug form", d.sourceRef)
	}
	if d.scopeTag != "book:book-uuid" {
		t.Errorf("scope_tag = %q, want book:book-uuid", d.scopeTag)
	}
}

func TestOnPageSaved_FactsCarryScopeAndSource(t *testing.T) {
	eng := &fakeEngine{}
	sc := &fakeSidecar{result: sidecar.ExtractionResult{
		Facts: []sidecar.ExtractedFact{
			{Content: "Deploys go to staging first.", Category: "process"},
			{Content: "GitHub Actions runs the pipeline.", Category: "tools"},
		},
	}}
	p := newPipelineWith(eng, sc, nil, nil)
	p.OnPageSaved(context.Background(), sampleEvent())

	if len(eng.commits) != 1 {
		t.Fatalf("expected 1 CommitFacts call, got %d", len(eng.commits))
	}
	facts := eng.commits[0]
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts committed, got %d", len(facts))
	}
	for _, f := range facts {
		if f.ScopeTag != "book:book-uuid" {
			t.Errorf("fact scope_tag = %q, want book:book-uuid", f.ScopeTag)
		}
		if f.SourceType != "wiki_page" {
			t.Errorf("fact source_type = %q, want wiki_page", f.SourceType)
		}
		if f.SourceRef != "engineering/deploy-process" {
			t.Errorf("fact source_ref = %q, want bookSlug/pageSlug", f.SourceRef)
		}
		if f.UserId != "operator" {
			t.Errorf("fact user_id = %q, want operator", f.UserId)
		}
		if len(f.Embedding) == 0 {
			t.Errorf("fact embedding should be populated by the test embedder")
		}
	}
}

func TestOnPageSaved_SkipsExtractionForShortBody(t *testing.T) {
	// Bodies under 32 chars don't make the LLM round-trip — the
	// signal-to-noise ratio is poor and the cost adds up across a
	// migration. Stale cleanup still runs.
	sc := &fakeSidecar{}
	mem := &fakeMem{}
	p := newPipelineWith(nil, sc, mem, nil)
	evt := sampleEvent()
	evt.Content = "tiny"
	p.OnPageSaved(context.Background(), evt)
	if sc.calls != 0 {
		t.Errorf("sidecar should NOT be called for short bodies; called %d times", sc.calls)
	}
	if len(mem.deletes) != 1 {
		t.Errorf("stale cleanup should still run for short bodies; got %d", len(mem.deletes))
	}
}

func TestOnPageSaved_RoutesBySize(t *testing.T) {
	// A normal-sized page uses the small extract model; a large one (a
	// research write-up) routes to the big-model extract so it doesn't
	// overrun the small model's context and blow the client timeout.
	t.Run("small body uses ExtractFacts", func(t *testing.T) {
		sc := &fakeSidecar{}
		p := newPipelineWith(nil, sc, nil, nil)
		p.OnPageSaved(context.Background(), sampleEvent())
		if sc.calls != 1 || sc.largeCalls != 0 {
			t.Errorf("small body should use ExtractFacts; calls=%d largeCalls=%d", sc.calls, sc.largeCalls)
		}
	})
	t.Run("large body uses ExtractFactsLarge", func(t *testing.T) {
		sc := &fakeSidecar{}
		p := newPipelineWith(nil, sc, nil, nil)
		evt := sampleEvent()
		evt.Content = strings.Repeat("This is a long research write-up. ", 200) // ~6.6K chars
		p.OnPageSaved(context.Background(), evt)
		if sc.largeCalls != 1 || sc.calls != 0 {
			t.Errorf("large body should use ExtractFactsLarge; calls=%d largeCalls=%d", sc.calls, sc.largeCalls)
		}
	})
}

func TestOnPageSaved_ExtractedRelationshipsCarryScope(t *testing.T) {
	rel := &fakeRel{}
	sc := &fakeSidecar{result: sidecar.ExtractionResult{
		Relationships: []sidecar.ExtractedRelationship{
			{Subject: "deploy-process", Predicate: "uses_tool", Object: "github-actions"},
		},
	}}
	p := newPipelineWith(nil, sc, nil, rel)
	p.OnPageSaved(context.Background(), sampleEvent())

	// Expect 2 batches: one for the extracted relationship, one
	// for the wiki link triples. Or possibly one combined — order-
	// agnostic assertion.
	var found memory.Relationship
	for _, batch := range rel.upserts {
		for _, r := range batch {
			if r.Predicate == "uses_tool" {
				found = r
			}
		}
	}
	if found.Subject == "" {
		t.Fatalf("extracted uses_tool triple not found in upserts: %+v", rel.upserts)
	}
	if found.ScopeTag != "book:book-uuid" {
		t.Errorf("extracted relationship scope_tag = %q, want book:book-uuid", found.ScopeTag)
	}
	if found.UserID != "operator" {
		t.Errorf("extracted relationship user_id = %q, want operator", found.UserID)
	}
}

func TestOnPageSaved_ResolvedLinksEmitLinksToTriples(t *testing.T) {
	rel := &fakeRel{}
	p := newPipelineWith(nil, nil, nil, rel)
	p.OnPageSaved(context.Background(), sampleEvent())

	// Find the links_to row. Sample event has one resolved (ci-
	// pipeline) and one broken (ops/rollback) — only the resolved
	// one should appear.
	var triples []memory.Relationship
	for _, batch := range rel.upserts {
		for _, r := range batch {
			if r.Predicate == "links_to" {
				triples = append(triples, r)
			}
		}
	}
	if len(triples) != 1 {
		t.Fatalf("expected exactly 1 links_to triple (broken links skipped); got %d: %+v", len(triples), triples)
	}
	tr := triples[0]
	if tr.Subject != "page:engineering/deploy-process" {
		t.Errorf("subject = %q, want page:engineering/deploy-process", tr.Subject)
	}
	if tr.Object != "page:engineering/ci-pipeline" {
		t.Errorf("object = %q, want page:engineering/ci-pipeline (same-book defaults to source book)", tr.Object)
	}
	if tr.ScopeTag != "book:book-uuid" {
		t.Errorf("scope_tag = %q, want book:book-uuid", tr.ScopeTag)
	}
}

func TestOnPageSaved_BrokenLinkSkipped(t *testing.T) {
	rel := &fakeRel{}
	p := newPipelineWith(nil, nil, nil, rel)
	evt := sampleEvent()
	// All links broken now.
	for i := range evt.Links {
		evt.Links[i].TargetPageID = nil
	}
	p.OnPageSaved(context.Background(), evt)
	for _, batch := range rel.upserts {
		for _, r := range batch {
			if r.Predicate == "links_to" {
				t.Errorf("broken-link-only event emitted a links_to triple: %+v", r)
			}
		}
	}
}

func TestOnPageSaved_CrossBookLinkUsesTargetBook(t *testing.T) {
	rel := &fakeRel{}
	p := newPipelineWith(nil, nil, nil, rel)
	evt := sampleEvent()
	id := "x"
	evt.Links = []admin.PageLink{
		{TargetBookSlug: "ops", TargetPageSlug: "incident-101", TargetPageID: &id},
	}
	p.OnPageSaved(context.Background(), evt)
	var got memory.Relationship
	for _, batch := range rel.upserts {
		for _, r := range batch {
			if r.Predicate == "links_to" {
				got = r
			}
		}
	}
	if got.Object != "page:ops/incident-101" {
		t.Errorf("cross-book object = %q, want page:ops/incident-101", got.Object)
	}
}

func TestOnPageDeleted_SweepsMemoriesNotRelationships(t *testing.T) {
	mem := &fakeMem{}
	rel := &fakeRel{}
	p := newPipelineWith(nil, nil, mem, rel)
	p.OnPageDeleted(context.Background(), DeleteEvent{
		BookID: "book-uuid", BookSlug: "engineering",
		PageID: "page-uuid", PageSlug: "deploy-process",
	})
	if len(mem.deletes) != 1 {
		t.Errorf("expected 1 memory cleanup call; got %d", len(mem.deletes))
	}
	if len(rel.upserts) != 0 {
		t.Errorf("delete must NOT touch relationships (cross-page); got %d upsert batches", len(rel.upserts))
	}
}

func TestPipeline_NilSafe(t *testing.T) {
	// A nil pipeline (when New returns nil because deps are empty)
	// must be safe to call into so wiring code can pass it
	// unconditionally.
	var p *Pipeline
	p.OnPageSaved(context.Background(), sampleEvent())
	p.OnPageDeleted(context.Background(), DeleteEvent{})
}
