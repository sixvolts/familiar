package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/familiar/gateway/internal/engine"
	mem "github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/skills"
	pb "github.com/familiar/gateway/proto/engine"
)

// testCtx returns a context carrying a minimal SessionContext with a
// canonical UserID set. Post-OWNER-MIGRATION the memory tools refuse
// to run when sc.UserID is empty (the old "fall back to 'owner'"
// path is gone) — so every test that exercises a tool must thread
// some user through. Tests asserting the explicit error case build
// their own bare context.
func testCtx() context.Context {
	return skills.WithContext(context.Background(), skills.SessionContext{
		UserID: "test-user",
	})
}

// --- fakes ------------------------------------------------------------------
//
// Only CommitFacts (engine) and Search (memstore) are exercised. The rest
// of each interface is stubbed with panics so any accidental new call
// site fails loud.

type fakeEngine struct {
	engine.Service // embed to auto-satisfy unused methods with nil panic
	mu             sync.Mutex
	committed      []*pb.FactProto
	commitErr      error
}

func (f *fakeEngine) QueryMemory(_ context.Context, _ *pb.MemoryQueryRequest) (*pb.MemoryQueryResponse, error) {
	return &pb.MemoryQueryResponse{}, nil
}

func (f *fakeEngine) CommitFacts(_ context.Context, _ string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commitErr != nil {
		return nil, f.commitErr
	}
	f.committed = append(f.committed, facts...)
	return &pb.CommitFactsResponse{}, nil
}

type fakeStore struct {
	mu         sync.Mutex
	results    []mem.MemoryResult
	searchErr  error
	lastVec    []float32
	lastLimit  int
	lastThr    float64
	lastUserID string
}

func (f *fakeStore) Search(_ context.Context, vec []float32, limit int, threshold float64, userID string) ([]mem.MemoryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastVec = vec
	f.lastLimit = limit
	f.lastThr = threshold
	f.lastUserID = userID
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.results, nil
}

func (f *fakeStore) HybridSearch(ctx context.Context, _ string, vec []float32, limit int, threshold float64, userID string) ([]mem.MemoryResult, error) {
	return f.Search(ctx, vec, limit, threshold, userID)
}

func (f *fakeStore) NearestSimilarity(_ context.Context, _ []float32, _ string, _ string) (float64, bool, error) {
	return 0, false, nil
}

func (f *fakeStore) NearestLiveFact(_ context.Context, _ []float32, _ string) (mem.NearestFact, bool, error) {
	return mem.NearestFact{}, false, nil
}

func (f *fakeStore) Close() error { return nil }

// --- save_fact --------------------------------------------------------------

func TestSaveFact_MissingEngineErrors(t *testing.T) {
	s := New(nil, nil, nil)
	res, err := s.Execute(testCtx(), "save_fact", json.RawMessage(`{"content":"x"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Error, "engine unavailable") {
		t.Errorf("expected engine-unavailable error, got %+v", res)
	}
}

func TestSaveFact_EmptyContentRejected(t *testing.T) {
	s := New(&fakeEngine{}, nil, nil)
	res, _ := s.Execute(testCtx(), "save_fact", json.RawMessage(`{"content":"  "}`))
	if !strings.Contains(res.Error, "content is required") {
		t.Errorf("expected required error, got %+v", res)
	}
}

func TestSaveFact_InvalidJSON(t *testing.T) {
	s := New(&fakeEngine{}, nil, nil)
	res, _ := s.Execute(testCtx(), "save_fact", json.RawMessage(`not json`))
	if !strings.Contains(res.Error, "invalid params") {
		t.Errorf("expected invalid params, got %+v", res)
	}
}

func TestSaveFact_DefaultsAndEmbeds(t *testing.T) {
	eng := &fakeEngine{}
	embedded := []float32{0.1, 0.2, 0.3}
	var gotEmbedText string
	embed := func(_ context.Context, text string) ([]float32, error) {
		gotEmbedText = text
		return embedded, nil
	}

	s := New(eng, nil, embed)
	res, err := s.Execute(testCtx(), "save_fact", json.RawMessage(`{"content":"the sky is blue"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error: %+v", res)
	}
	if len(eng.committed) != 1 {
		t.Fatalf("expected 1 committed fact, got %d", len(eng.committed))
	}
	fact := eng.committed[0]
	if fact.Content != "the sky is blue" {
		t.Errorf("content: %q", fact.Content)
	}
	if fact.Scope != "user" {
		t.Errorf("default scope should be user, got %q", fact.Scope)
	}
	if fact.Confidence != 0.9 {
		t.Errorf("default confidence = %v, want 0.9", fact.Confidence)
	}
	if len(fact.Embedding) != 3 || fact.Embedding[0] != 0.1 {
		t.Errorf("embedding not set: %v", fact.Embedding)
	}
	if gotEmbedText != "the sky is blue" {
		t.Errorf("embedder saw %q", gotEmbedText)
	}
	if !strings.Contains(res.Content, "Saved fact") || strings.Contains(res.Content, "no embedding") {
		t.Errorf("unexpected result content: %q", res.Content)
	}
}

func TestSaveFact_NoEmbedderAddsNote(t *testing.T) {
	eng := &fakeEngine{}
	s := New(eng, nil, nil) // embed == nil

	res, err := s.Execute(testCtx(), "save_fact", json.RawMessage(`{"content":"x","scope":"session","confidence":0.5}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "no embedding") {
		t.Errorf("expected no-embedding note, got %q", res.Content)
	}
	fact := eng.committed[0]
	if fact.Scope != "session" {
		t.Errorf("scope override: %q", fact.Scope)
	}
	if fact.Confidence != 0.5 {
		t.Errorf("confidence override: %v", fact.Confidence)
	}
}

func TestSaveFact_CommitError(t *testing.T) {
	eng := &fakeEngine{commitErr: errors.New("boom")}
	s := New(eng, nil, nil)
	_, err := s.Execute(testCtx(), "save_fact", json.RawMessage(`{"content":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "commit fact") {
		t.Errorf("expected commit error, got %v", err)
	}
}

// --- remember ---------------------------------------------------------------

func TestRemember_MissingEngineErrors(t *testing.T) {
	s := New(nil, nil, nil)
	res, _ := s.Execute(testCtx(), "remember", json.RawMessage(`{"content":"x"}`))
	if !strings.Contains(res.Error, "engine unavailable") {
		t.Errorf("expected engine-unavailable, got %+v", res)
	}
}

func TestRemember_EmptyContentRejected(t *testing.T) {
	s := New(&fakeEngine{}, nil, nil)
	res, _ := s.Execute(testCtx(), "remember", json.RawMessage(`{"content":"  "}`))
	if !strings.Contains(res.Error, "content is required") {
		t.Errorf("expected required error, got %+v", res)
	}
}

func TestRemember_InvalidImportanceRejected(t *testing.T) {
	s := New(&fakeEngine{}, nil, nil)
	res, _ := s.Execute(testCtx(), "remember",
		json.RawMessage(`{"content":"x","importance":"critical"}`))
	if !strings.Contains(res.Error, "importance must be") {
		t.Errorf("expected importance error, got %+v", res)
	}
}

func TestRemember_StoresExplicitHighConfidenceFact(t *testing.T) {
	eng := &fakeEngine{}
	embed := func(_ context.Context, _ string) ([]float32, error) {
		return []float32{0.5, 0.5}, nil
	}
	s := New(eng, nil, embed)

	res, err := s.Execute(testCtx(), "remember",
		json.RawMessage(`{"content":"my anniversary is March 15","importance":"high"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if !strings.Contains(res.Content, "Got it") {
		t.Errorf("expected confirmation message, got %q", res.Content)
	}
	if len(eng.committed) != 1 {
		t.Fatalf("expected 1 committed fact, got %d", len(eng.committed))
	}
	fact := eng.committed[0]
	if fact.SourceType != "explicit" {
		t.Errorf("source_type = %q, want explicit", fact.SourceType)
	}
	if fact.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", fact.Confidence)
	}
	if fact.Scope != "user" {
		t.Errorf("scope = %q, want user", fact.Scope)
	}
	if len(fact.Embedding) != 2 {
		t.Errorf("embedding not set: %v", fact.Embedding)
	}
	foundTag := false
	for _, tag := range fact.Tags {
		if tag == "importance:high" {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("expected importance:high tag, got %v", fact.Tags)
	}
}

func TestRemember_DefaultImportanceIsMedium(t *testing.T) {
	eng := &fakeEngine{}
	s := New(eng, nil, nil)

	_, err := s.Execute(testCtx(), "remember", json.RawMessage(`{"content":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	fact := eng.committed[0]
	foundTag := false
	for _, tag := range fact.Tags {
		if tag == "importance:medium" {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("expected importance:medium default tag, got %v", fact.Tags)
	}
}

func TestRemember_NoEmbedderAddsNote(t *testing.T) {
	eng := &fakeEngine{}
	s := New(eng, nil, nil)

	res, err := s.Execute(testCtx(), "remember", json.RawMessage(`{"content":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "no embedding") {
		t.Errorf("expected no-embedding note, got %q", res.Content)
	}
}

func TestRemember_CommitError(t *testing.T) {
	eng := &fakeEngine{commitErr: errors.New("boom")}
	s := New(eng, nil, nil)
	_, err := s.Execute(testCtx(), "remember", json.RawMessage(`{"content":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "commit fact") {
		t.Errorf("expected commit error, got %v", err)
	}
}

func TestRemember_ListedInTools(t *testing.T) {
	s := New(nil, nil, nil)
	var names []string
	for _, tool := range s.Tools() {
		names = append(names, tool.Name)
	}
	found := false
	for _, n := range names {
		if n == "remember" {
			found = true
		}
	}
	if !found {
		t.Errorf("remember missing from Tools(): %v", names)
	}
}

// --- search_memory ----------------------------------------------------------

func TestSearchMemory_MissingStoreErrors(t *testing.T) {
	s := New(&fakeEngine{}, nil, func(context.Context, string) ([]float32, error) { return nil, nil })
	res, _ := s.Execute(testCtx(), "search_memory", json.RawMessage(`{"query":"x"}`))
	if !strings.Contains(res.Error, "store unavailable") {
		t.Errorf("%+v", res)
	}
}

func TestSearchMemory_MissingEmbedderErrors(t *testing.T) {
	s := New(&fakeEngine{}, &fakeStore{}, nil)
	res, _ := s.Execute(testCtx(), "search_memory", json.RawMessage(`{"query":"x"}`))
	if !strings.Contains(res.Error, "embedder unavailable") {
		t.Errorf("%+v", res)
	}
}

func TestSearchMemory_EmptyQueryRejected(t *testing.T) {
	s := New(&fakeEngine{}, &fakeStore{}, func(context.Context, string) ([]float32, error) { return nil, nil })
	res, _ := s.Execute(testCtx(), "search_memory", json.RawMessage(`{"query":"   "}`))
	if !strings.Contains(res.Error, "query is required") {
		t.Errorf("%+v", res)
	}
}

func TestSearchMemory_DefaultLimitAndThreshold(t *testing.T) {
	store := &fakeStore{}
	embed := func(context.Context, string) ([]float32, error) { return []float32{1}, nil }

	s := New(nil, store, embed)
	_, err := s.Execute(testCtx(), "search_memory", json.RawMessage(`{"query":"editor"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if store.lastLimit != 5 {
		t.Errorf("default limit = %d, want 5", store.lastLimit)
	}
	if store.lastThr != 0.65 {
		t.Errorf("default threshold = %v, want 0.65", store.lastThr)
	}
}

func TestSearchMemory_OverrideLimitAndThreshold(t *testing.T) {
	store := &fakeStore{}
	embed := func(context.Context, string) ([]float32, error) { return []float32{1}, nil }
	s := New(nil, store, embed)

	_, err := s.Execute(testCtx(), "search_memory",
		json.RawMessage(`{"query":"q","limit":12,"threshold":0.8}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if store.lastLimit != 12 || store.lastThr != 0.8 {
		t.Errorf("overrides not honored: limit=%d thr=%v", store.lastLimit, store.lastThr)
	}
}

func TestSearchMemory_NoResultsReturnsFriendlyMessage(t *testing.T) {
	store := &fakeStore{results: nil}
	embed := func(context.Context, string) ([]float32, error) { return []float32{1}, nil }
	s := New(nil, store, embed)

	res, _ := s.Execute(testCtx(), "search_memory", json.RawMessage(`{"query":"ghost"}`))
	if !strings.Contains(res.Content, "No memories above threshold") {
		t.Errorf("expected no-results message, got %q", res.Content)
	}
}

func TestSearchMemory_FormatsResults(t *testing.T) {
	store := &fakeStore{results: []mem.MemoryResult{
		{Content: "user prefers vim", Scope: "user", Similarity: 0.91},
		{Content: "past slack bug", Scope: "session", Similarity: 0.72},
	}}
	embed := func(context.Context, string) ([]float32, error) { return []float32{1}, nil }
	s := New(nil, store, embed)

	res, err := s.Execute(testCtx(), "search_memory", json.RawMessage(`{"query":"editor"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "1. (sim: 0.910) user prefers vim") {
		t.Errorf("format missing first row: %s", res.Content)
	}
	if !strings.Contains(res.Content, "2. (sim: 0.720) past slack bug") {
		t.Errorf("format missing second row: %s", res.Content)
	}
	// Data field should be populated with the JSON of the results.
	if len(res.Data) == 0 || !strings.Contains(string(res.Data), "user prefers vim") {
		t.Errorf("expected JSON data payload, got %q", res.Data)
	}
}

func TestSearchMemory_EmbedErrorSurfaces(t *testing.T) {
	store := &fakeStore{}
	embed := func(context.Context, string) ([]float32, error) { return nil, errors.New("down") }
	s := New(nil, store, embed)

	_, err := s.Execute(testCtx(), "search_memory", json.RawMessage(`{"query":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "embed query") {
		t.Errorf("expected embed error, got %v", err)
	}
}

func TestSearchMemory_StoreErrorSurfaces(t *testing.T) {
	store := &fakeStore{searchErr: errors.New("db down")}
	embed := func(context.Context, string) ([]float32, error) { return []float32{1}, nil }
	s := New(nil, store, embed)

	_, err := s.Execute(testCtx(), "search_memory", json.RawMessage(`{"query":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "search memory") {
		t.Errorf("expected search error, got %v", err)
	}
}

// --- dispatch ---------------------------------------------------------------

func TestExecute_UnknownToolErrors(t *testing.T) {
	s := New(nil, nil, nil)
	_, err := s.Execute(testCtx(), "nope", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected unknown tool error, got %v", err)
	}
}

func TestSkillMetadata(t *testing.T) {
	s := New(nil, nil, nil)
	if s.Name() != "memory" {
		t.Errorf("name: %q", s.Name())
	}
	tools := s.Tools()
	if len(tools) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(tools))
	}
	seen := map[string]bool{}
	for _, tl := range tools {
		seen[tl.Name] = true
	}
	for _, want := range []string{"save_fact", "remember", "search_memory", "list_my_memories", "forget_fact", "correct_fact"} {
		if !seen[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

// --- interface compliance ---------------------------------------------------

var _ skills.Skill = (*Skill)(nil)

// Sanity: fakeStore fully satisfies mem.MemoryStore at compile time so
// the test file fails loud if the interface grows.
var _ mem.MemoryStore = (*fakeStore)(nil)

// Doc pointer for the reader: fakeEngine relies on struct embedding of
// engine.Service to satisfy the interface. Any unimplemented method
// will panic on nil dereference at call time — that's intentional so
// we catch accidental new call sites in skill code immediately.
var _ = fmt.Sprintf

// --- Shard scope_tag plumbing (FAMILIAR-SHARDS-PHASE1-SPEC Step 5) --------

// TestSaveFact_PropagatesScopeTagFromSessionContext asserts that when a
// skill dispatch carries a ScopeTag on its SessionContext, save_fact
// stamps it on the committed FactProto. This is the seam that makes
// shard-scoped memory writes isolate correctly — the engine persists
// scope_tag verbatim, and the gateway's retrieval query excludes
// isolated-shard tags from top-level views.
func TestSaveFact_PropagatesScopeTagFromSessionContext(t *testing.T) {
	eng := &fakeEngine{}
	s := New(eng, nil, func(_ context.Context, _ string) ([]float32, error) {
		return []float32{0.1, 0.2, 0.3}, nil
	})

	ctx := skills.WithContext(context.Background(), skills.SessionContext{
		SessionID: "sess-1",
		UserID:    "owner",
		ScopeTag:  "shard:tagged-writer",
	})
	if _, err := s.Execute(ctx, "save_fact", json.RawMessage(`{"content":"tagged fact"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(eng.committed) != 1 {
		t.Fatalf("committed = %d, want 1", len(eng.committed))
	}
	if eng.committed[0].ScopeTag != "shard:tagged-writer" {
		t.Errorf("FactProto.ScopeTag = %q, want %q", eng.committed[0].ScopeTag, "shard:tagged-writer")
	}
}

// TestSaveFact_TrustedPathLeavesScopeTagEmpty is the counterpart: when
// no ScopeTag is set on SessionContext (trusted path), the committed
// FactProto carries an empty ScopeTag — which the engine persists as
// NULL, keeping trusted writes visible to top-level retrieval.
func TestSaveFact_TrustedPathLeavesScopeTagEmpty(t *testing.T) {
	eng := &fakeEngine{}
	s := New(eng, nil, func(_ context.Context, _ string) ([]float32, error) {
		return []float32{0.1}, nil
	})
	if _, err := s.Execute(testCtx(), "save_fact", json.RawMessage(`{"content":"untagged"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(eng.committed) != 1 {
		t.Fatalf("committed = %d", len(eng.committed))
	}
	if eng.committed[0].ScopeTag != "" {
		t.Errorf("trusted-path ScopeTag = %q, want empty", eng.committed[0].ScopeTag)
	}
}

// TestSaveFact_PropagatesExcludeFromHot ensures the SessionContext
// flag (set by the pipeline only for isolated-visibility shards)
// reaches the FactProto so the engine routes the write past its RAM
// tier (FAMILIAR-SHARDS-PHASE1-FINDINGS Issue 3).
func TestSaveFact_PropagatesExcludeFromHot(t *testing.T) {
	eng := &fakeEngine{}
	s := New(eng, nil, func(_ context.Context, _ string) ([]float32, error) {
		return []float32{0.1}, nil
	})
	ctx := skills.WithContext(context.Background(), skills.SessionContext{
		UserID:         "owner",
		ScopeTag:       "shard:isolated",
		ExcludeFromHot: true,
	})
	if _, err := s.Execute(ctx, "save_fact", json.RawMessage(`{"content":"isolated fact"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(eng.committed) != 1 {
		t.Fatalf("committed = %d", len(eng.committed))
	}
	if !eng.committed[0].ExcludeFromHot {
		t.Errorf("FactProto.ExcludeFromHot = false, want true")
	}
}

// fakeManager records the args forget_fact/correct_fact pass to the
// manager and lets a test dictate which (id, user) pairs "own" a row.
type fakeManager struct {
	owners       map[string]string // memoryID -> ownerUserID
	lastDelID    string
	lastDelUser  string
	deleteCalled bool
}

func (m *fakeManager) ListMemories(context.Context, mem.MemoryFilter, int, int) ([]mem.MemoryRow, error) {
	return nil, nil
}
func (m *fakeManager) UpdateMemoryContent(context.Context, string, string, string, []float32) error {
	return nil
}
func (m *fakeManager) DeleteMemoryOwned(_ context.Context, id, userID string) (bool, error) {
	m.deleteCalled = true
	m.lastDelID, m.lastDelUser = id, userID
	return m.owners[id] == userID, nil
}

// forget_fact with an explicit id must pass the *caller's* user id into
// the owner-scoped delete and refuse a row owned by someone else — the
// authz fix for the cross-user delete-by-UUID hole.
func TestForgetFact_ExplicitIDIsOwnerScoped(t *testing.T) {
	mgr := &fakeManager{owners: map[string]string{
		"mine-123": "alice",
		"bobs-456": "bob",
	}}
	s := New(nil, nil, func(context.Context, string) ([]float32, error) { return nil, nil }, WithManager(mgr))
	ctx := skills.WithContext(context.Background(), skills.SessionContext{UserID: "alice"})

	// Alice forgets her own row → deleted, scoped to alice.
	res, err := s.Execute(ctx, "forget_fact", json.RawMessage(`{"id":"mine-123"}`))
	if err != nil {
		t.Fatalf("forget own: %v", err)
	}
	if mgr.lastDelUser != "alice" || mgr.lastDelID != "mine-123" {
		t.Errorf("delete args = (%q,%q), want (mine-123, alice)", mgr.lastDelID, mgr.lastDelUser)
	}
	if !strings.HasPrefix(res.Content, "Forgot") {
		t.Errorf("own delete content = %q, want a Forgot confirmation", res.Content)
	}

	// Alice names Bob's row → the scoped delete refuses (owner mismatch),
	// surfaced as a benign not-found rather than a cross-user delete.
	mgr.deleteCalled = false
	res, err = s.Execute(ctx, "forget_fact", json.RawMessage(`{"id":"bobs-456"}`))
	if err != nil {
		t.Fatalf("forget other: %v", err)
	}
	if !mgr.deleteCalled || mgr.lastDelUser != "alice" {
		t.Errorf("expected an owner-scoped delete attempt as alice, got called=%v user=%q", mgr.deleteCalled, mgr.lastDelUser)
	}
	if !strings.Contains(res.Content, "not found") {
		t.Errorf("cross-user delete content = %q, want a not-found message", res.Content)
	}
}

// TestRemember_PropagatesScopeTag mirrors the save_fact test for the
// remember tool so both commit paths stay in lockstep.
func TestRemember_PropagatesScopeTag(t *testing.T) {
	eng := &fakeEngine{}
	s := New(eng, nil, func(_ context.Context, _ string) ([]float32, error) {
		return []float32{0.1}, nil
	})
	ctx := skills.WithContext(context.Background(), skills.SessionContext{
		UserID:   "owner",
		ScopeTag: "shard:notes",
	})
	if _, err := s.Execute(ctx, "remember", json.RawMessage(`{"content":"don't forget"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(eng.committed) != 1 || eng.committed[0].ScopeTag != "shard:notes" {
		t.Errorf("remember ScopeTag = %q, want shard:notes", eng.committed[0].ScopeTag)
	}
}
