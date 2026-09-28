package wikiknowledge

// Pipeline tests run against fake collaborators — no engine, no
// sidecar, no DB. They pin down the contract that matters:
//   * A page's facts are replaced, keyed by page id, only after a
//     successful extraction.
//   * Each extracted fact arrives at ReplaceSourceFacts carrying the
//     book scope_tag, source_ref, source_type.
//   * Saves are debounced and a run a newer save or a delete
//     overtook never commits.
//   * Extracted relationships and resolved-link triples both go
//     through UpsertRelationships with scope_tag set.
//   * Broken outbound links don't emit links_to triples.
//   * OnPageDeleted fires DeleteMemoriesBySource; relationships
//     are NOT swept (cross-page, intentional).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/sidecar"

	pb "github.com/familiar/gateway/proto/engine"
)

// ── Fakes ─────────────────────────────────────────────────────────

type replaceCall struct {
	sourceType, sourceRef, scopeTag string
	facts                           []*pb.FactProto
}

type fakeEngine struct {
	mu       sync.Mutex
	replaces []replaceCall
}

func (f *fakeEngine) ReplaceSourceFacts(_ context.Context, _ string, sourceType, sourceRef, scopeTag string, facts []*pb.FactProto) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]*pb.FactProto, len(facts))
	copy(cp, facts)
	f.replaces = append(f.replaces, replaceCall{sourceType, sourceRef, scopeTag, cp})
	return 0, nil
}

func (f *fakeEngine) calls() []replaceCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]replaceCall(nil), f.replaces...)
}

type fakeSidecar struct {
	mu         sync.Mutex
	result     sidecar.ExtractionResult
	err        error
	calls      int
	largeCalls int
	bodies     []string
	deadline   time.Duration // time left on the ctx of the last call
	// gate, when set, holds every extraction until it is closed, and
	// started reports each call as it begins.
	gate    chan struct{}
	started chan string
	// failOn fails every extraction whose page body contains it.
	failOn string
}

func (f *fakeSidecar) extract(ctx context.Context, turns []sidecar.Turn, large bool) (sidecar.ExtractionResult, error) {
	f.mu.Lock()
	if large {
		f.largeCalls++
	} else {
		f.calls++
	}
	body := turns[0].Content
	f.bodies = append(f.bodies, body)
	if dl, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(dl)
	}
	gate, started := f.gate, f.started
	f.mu.Unlock()
	if started != nil {
		started <- body
	}
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return sidecar.ExtractionResult{}, f.err
	}
	if f.failOn != "" && strings.Contains(body, f.failOn) {
		return sidecar.ExtractionResult{}, errors.New("extractor rejected this page")
	}
	res := f.result
	if len(res.Facts) == 0 {
		// Echo the page body as its one fact, so a test can tell which
		// save a commit came from.
		res.Facts = []sidecar.ExtractedFact{{Content: body}}
	}
	return res, nil
}

// n is the number of extractions so far, safe to poll while runs are
// in flight.
func (f *fakeSidecar) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls + f.largeCalls
}

func (f *fakeSidecar) ExtractFacts(ctx context.Context, turns []sidecar.Turn) (sidecar.ExtractionResult, error) {
	return f.extract(ctx, turns, false)
}

func (f *fakeSidecar) ExtractFactsLarge(ctx context.Context, turns []sidecar.Turn) (sidecar.ExtractionResult, error) {
	return f.extract(ctx, turns, true)
}

type fakeMem struct {
	mu      sync.Mutex
	deletes []deleteCall
}
type deleteCall struct{ sourceType, sourceRef, scopeTag string }

func (f *fakeMem) DeleteMemoriesBySource(_ context.Context, st, sr, sg string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func TestOnPageSaved_ReplacesFactsKeyedByPageID(t *testing.T) {
	eng := &fakeEngine{}
	p := newPipelineWith(eng, &fakeSidecar{}, nil, nil)
	p.OnPageSaved(context.Background(), sampleEvent())
	calls := eng.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 replace call, got %d", len(calls))
	}
	c := calls[0]
	if c.sourceType != "wiki_page" {
		t.Errorf("source_type = %q, want wiki_page", c.sourceType)
	}
	// By page id: slugs change on every title edit, and facts keyed by
	// slug were orphaned by a rename.
	if c.sourceRef != "page:page-uuid" {
		t.Errorf("source_ref = %q, want page:{page id}", c.sourceRef)
	}
	if c.scopeTag != "book:book-uuid" {
		t.Errorf("scope_tag = %q, want book:book-uuid", c.scopeTag)
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

	calls := eng.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 replace call, got %d", len(calls))
	}
	facts := calls[0].facts
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
		if f.SourceRef != "page:page-uuid" {
			t.Errorf("fact source_ref = %q, want page:page-uuid", f.SourceRef)
		}
		if f.UserId != "operator" {
			t.Errorf("fact user_id = %q, want operator", f.UserId)
		}
		if len(f.Embedding) == 0 {
			t.Errorf("fact embedding should be populated by the test embedder")
		}
	}
}

// A failed extraction must leave the page's existing facts alone. The
// old order deleted them first, so a sidecar that was down, busy, or
// slower than the deadline wiped the page's knowledge on every save.
func TestOnPageSaved_FailedExtractionKeepsFacts(t *testing.T) {
	eng := &fakeEngine{}
	mem := &fakeMem{}
	sc := &fakeSidecar{err: errors.New("context deadline exceeded")}
	p := newPipelineWith(eng, sc, mem, nil)
	p.OnPageSaved(context.Background(), sampleEvent())
	if n := len(eng.calls()); n != 0 {
		t.Errorf("a failed extraction replaced the page's facts (%d replace calls)", n)
	}
	if n := len(mem.deletes); n != 0 {
		t.Errorf("a failed extraction deleted the page's facts (%d deletes)", n)
	}
}

func TestOnPageSaved_ShortBodyClearsFacts(t *testing.T) {
	// Bodies under 32 chars don't make the LLM round-trip — the
	// signal-to-noise ratio is poor and the cost adds up across a
	// migration. The page holds no knowledge any more, so its facts
	// are replaced with none.
	sc := &fakeSidecar{}
	eng := &fakeEngine{}
	p := newPipelineWith(eng, sc, nil, nil)
	evt := sampleEvent()
	evt.Content = "tiny"
	p.OnPageSaved(context.Background(), evt)
	if sc.calls != 0 {
		t.Errorf("sidecar should NOT be called for short bodies; called %d times", sc.calls)
	}
	calls := eng.calls()
	if len(calls) != 1 || len(calls[0].facts) != 0 {
		t.Errorf("a short body should replace the page's facts with none; got %+v", calls)
	}
}

// The deadline has to fit the large route, which takes minutes. The old
// 30s cap cancelled every large page's extraction.
func TestOnPageSaved_DeadlineFitsLargeExtraction(t *testing.T) {
	sc := &fakeSidecar{}
	p := newPipelineWith(&fakeEngine{}, sc, nil, nil)
	evt := sampleEvent()
	evt.Content = strings.Repeat("This is a long research write-up. ", 200)
	p.OnPageSaved(context.Background(), evt)
	if sc.largeCalls != 1 {
		t.Fatalf("large body should use ExtractFactsLarge; largeCalls=%d", sc.largeCalls)
	}
	if sc.deadline < sidecar.LargeExtractTimeout {
		t.Errorf("extraction deadline = %v, shorter than the large route's own %v ceiling",
			sc.deadline.Round(time.Second), sidecar.LargeExtractTimeout)
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
		t.Fatalf("expected 1 memory cleanup call; got %d", len(mem.deletes))
	}
	if got := mem.deletes[0].sourceRef; got != "page:page-uuid" {
		t.Errorf("delete source_ref = %q, want page:page-uuid (the id the facts were stored under)", got)
	}
	if len(rel.upserts) != 0 {
		t.Errorf("delete must NOT touch relationships (cross-page); got %d upsert batches", len(rel.upserts))
	}
}

// debounced builds a pipeline whose saves go through PageSaved.
func debounced(eng *fakeEngine, sc *fakeSidecar, mem *fakeMem) *Pipeline {
	p := newPipelineWith(eng, sc, mem, nil)
	p.deps.Debounce = 20 * time.Millisecond
	return p
}

func saveWith(content string) SaveEvent {
	evt := sampleEvent()
	evt.Content = content
	return evt
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func idle(p *Pipeline) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pages) == 0
}

// A burst of autosaves is one extraction, of the last save.
func TestPageSaved_DebouncesAutosaves(t *testing.T) {
	eng := &fakeEngine{}
	sc := &fakeSidecar{}
	p := debounced(eng, sc, nil)
	for _, c := range []string{"The dentist appointment is on Tue", "The dentist appointment is on Thursday", "The dentist appointment is on Thursday at 3pm"} {
		p.PageSaved(saveWith(c))
	}
	waitFor(t, "the run to finish", func() bool { return len(eng.calls()) > 0 && idle(p) })
	if sc.n() != 1 {
		t.Errorf("3 quick saves ran %d extractions, want 1", sc.n())
	}
	calls := eng.calls()
	if len(calls) != 1 || !strings.Contains(calls[0].facts[0].Content, "Thursday at 3pm") {
		t.Errorf("want one commit of the last save, got %+v", calls)
	}
}

// A run that a newer save overtook while it was extracting must not
// commit: the older draft's facts would land after (or alongside) the
// newer ones. The newer save runs once the older run finishes.
func TestPageSaved_SupersededRunDiscarded(t *testing.T) {
	eng := &fakeEngine{}
	sc := &fakeSidecar{gate: make(chan struct{}), started: make(chan string, 4)}
	p := debounced(eng, sc, nil)

	p.PageSaved(saveWith("The dentist appointment is on Tuesday at 3pm"))
	<-sc.started // run 1 is extracting
	p.PageSaved(saveWith("The dentist appointment is on Thursday at 3pm"))
	time.Sleep(60 * time.Millisecond) // run 2's debounce fires; it waits for run 1
	close(sc.gate)

	waitFor(t, "both runs to finish", func() bool { return sc.n() == 2 && idle(p) })
	calls := eng.calls()
	if len(calls) != 1 {
		t.Fatalf("want exactly one commit (the newer save's), got %d: %+v", len(calls), calls)
	}
	if got := calls[0].facts[0].Content; !strings.Contains(got, "Thursday") {
		t.Errorf("committed %q; the superseded Tuesday draft won", got)
	}
}

// A delete during an extraction removes the page's facts, and the
// extraction that was running doesn't bring them back.
func TestPageSaved_DeleteBeatsInFlightRun(t *testing.T) {
	eng := &fakeEngine{}
	mem := &fakeMem{}
	sc := &fakeSidecar{gate: make(chan struct{}), started: make(chan string, 4)}
	p := debounced(eng, sc, mem)

	p.PageSaved(saveWith("The dentist appointment is on Tuesday at 3pm"))
	<-sc.started
	p.OnPageDeleted(context.Background(), DeleteEvent{BookID: "book-uuid", PageID: "page-uuid"})
	close(sc.gate)

	waitFor(t, "the run to finish", func() bool { return idle(p) && sc.n() == 1 })
	time.Sleep(20 * time.Millisecond)
	if calls := eng.calls(); len(calls) != 0 {
		t.Errorf("a run overtaken by the page's delete committed %+v", calls)
	}
	if len(mem.deletes) != 1 {
		t.Errorf("want the delete's cleanup, got %d deletes", len(mem.deletes))
	}
}

// A pending save for a deleted page never runs.
func TestPageSaved_DeleteCancelsPendingSave(t *testing.T) {
	eng := &fakeEngine{}
	sc := &fakeSidecar{}
	p := debounced(eng, sc, &fakeMem{})
	p.deps.Debounce = 50 * time.Millisecond
	p.PageSaved(saveWith("The dentist appointment is on Tuesday at 3pm"))
	p.OnPageDeleted(context.Background(), DeleteEvent{BookID: "book-uuid", PageID: "page-uuid"})
	time.Sleep(150 * time.Millisecond)
	if sc.n() != 0 || len(eng.calls()) != 0 {
		t.Errorf("a save pending when its page was deleted still ran: extractions=%d commits=%d", sc.n(), len(eng.calls()))
	}
}

func TestPageSaved_SkipsResearchBooks(t *testing.T) {
	sc := &fakeSidecar{}
	p := debounced(&fakeEngine{}, sc, nil)
	evt := sampleEvent()
	evt.BookSlug = "research:operator"
	p.PageSaved(evt)
	time.Sleep(60 * time.Millisecond)
	if sc.n() != 0 || !idle(p) {
		t.Errorf("a research evidence page was scheduled for ingestion")
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
