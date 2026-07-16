package admin

// Memory graph endpoint tests (FAMILIAR-CONSOLE-SPEC Phase D).
//
//   GET /console/api/memory/graph?center=<entity>&depth=N&limit=M
//   GET /console/api/memory/graph?limit=M               (top-N path)
//   GET /console/api/memory/entities?q=<substr>&limit=L
//   GET /console/api/memory/relationship/{id}
//
// Coverage:
//   - Top-N path returns nodes derived from edges, with degree counts
//     reflecting how many returned edges touch each node.
//   - Center path threads the right ownerID into the store call.
//   - Non-admin's `?user_id=` query param is ignored — they always
//     see only their own graph.
//   - Admin's `?user_id=` IS honored and reaches the store.
//   - Edge detail joins the supporting fact via memoryBrowser.
//   - GetRelationship returning nil yields 404 (existence-hiding).
//   - 503 when graph store isn't wired.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/familiar/gateway/internal/memory"
)

// fakeGraphStore captures the call args so tests can assert that the
// handler passed the right ownerID etc., and returns canned data.
type fakeGraphStore struct {
	mu sync.Mutex

	// What the next call returns:
	graphAroundEdges []memory.GraphEdge
	graphTopEdges    []memory.GraphEdge
	searchMatches    []memory.EntityMatch
	relationship     *memory.GraphEdge
	relationshipErr  error

	// Recorded args from the most recent call:
	lastAroundCenter string
	lastAroundOwner  string
	lastAroundDepth  int
	lastAroundLimit  int

	lastTopOwner string
	lastTopLimit int

	lastSearchQ     string
	lastSearchOwner string
	lastSearchLimit int

	lastRelID    string
	lastRelOwner string

	entityFacts     []memory.MemoryRow
	lastFactsEntity string
	lastFactsOwner  string
	lastFactsLimit  int

	mergeRewritten      int64
	lastMergeFrom       string
	lastMergeTo         string
	lastMergeOwner      string
	lastPatchPredicate  string
	lastPatchConfidence *float64
}

func (f *fakeGraphStore) GraphAround(_ context.Context, center, ownerID string, depth, limit int) ([]memory.GraphEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAroundCenter, f.lastAroundOwner, f.lastAroundDepth, f.lastAroundLimit = center, ownerID, depth, limit
	return append([]memory.GraphEdge{}, f.graphAroundEdges...), nil
}

func (f *fakeGraphStore) GraphTop(_ context.Context, ownerID string, limit int) ([]memory.GraphEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastTopOwner, f.lastTopLimit = ownerID, limit
	return append([]memory.GraphEdge{}, f.graphTopEdges...), nil
}

func (f *fakeGraphStore) SearchEntities(_ context.Context, q, ownerID string, limit int) ([]memory.EntityMatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSearchQ, f.lastSearchOwner, f.lastSearchLimit = q, ownerID, limit
	return append([]memory.EntityMatch{}, f.searchMatches...), nil
}

func (f *fakeGraphStore) ListBySourceFact(context.Context, string) ([]memory.GraphEdge, error) {
	return nil, nil
}

func (f *fakeGraphStore) FactsForEntity(_ context.Context, entity, ownerID string, limit int) ([]memory.MemoryRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFactsEntity, f.lastFactsOwner, f.lastFactsLimit = entity, ownerID, limit
	return append([]memory.MemoryRow{}, f.entityFacts...), nil
}

func (f *fakeGraphStore) MergeEntities(_ context.Context, from, to, ownerID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMergeFrom, f.lastMergeTo, f.lastMergeOwner = from, to, ownerID
	return f.mergeRewritten, nil
}

func (f *fakeGraphStore) UpdateRelationship(_ context.Context, id, ownerID, predicate string, confidence *float64) (*memory.GraphEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRelID, f.lastRelOwner = id, ownerID
	f.lastPatchPredicate = predicate
	f.lastPatchConfidence = confidence
	if f.relationshipErr != nil {
		return nil, f.relationshipErr
	}
	return f.relationship, nil
}

func (f *fakeGraphStore) OrphanEdges(context.Context, string) (int, error) {
	return 0, nil
}

func (f *fakeGraphStore) GetRelationship(_ context.Context, id, ownerID string) (*memory.GraphEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRelID, f.lastRelOwner = id, ownerID
	if f.relationshipErr != nil {
		return nil, f.relationshipErr
	}
	if f.relationship == nil {
		return nil, nil
	}
	cp := *f.relationship
	return &cp, nil
}

// Phase F dashboard-aggregate stubs — existing graph tests don't
// exercise these; the dashboard tests use dedicated fakes.
func (f *fakeGraphStore) CountEntities(_ context.Context, userID string) (int, error) {
	return 0, nil
}
func (f *fakeGraphStore) CountRelationships(_ context.Context, userID string) (int, error) {
	return 0, nil
}
func (f *fakeGraphStore) EntityBreakdown(_ context.Context, userID string) ([]memory.EntityTypeCount, error) {
	return nil, nil
}
func (f *fakeGraphStore) DeleteRelationship(_ context.Context, id, userID string) error {
	return nil
}
func (f *fakeGraphStore) DeleteEntity(_ context.Context, name, userID string) (int64, error) {
	return 0, nil
}

// ──────────────────────────────────────────────────────────────────
// Top-N (no center) path
// ──────────────────────────────────────────────────────────────────

func TestGraph_TopReturnsNodesAndEdges(t *testing.T) {
	g := &fakeGraphStore{
		graphTopEdges: []memory.GraphEdge{
			{ID: "r1", Subject: "operator", Predicate: "owns", Object: "rivian", Confidence: 0.95},
			{ID: "r2", Subject: "operator", Predicate: "lives_in", Object: "menlo park", Confidence: 0.9},
			{ID: "r3", Subject: "rivian", Predicate: "located_in", Object: "menlo park", Confidence: 0.85},
		},
	}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("GET", "/console/api/memory/graph?limit=50", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.listGraph(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp graphResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 3 {
		t.Errorf("nodes = %d, want 3 (operator/rivian/menlo park)", len(resp.Nodes))
	}
	if len(resp.Edges) != 3 {
		t.Errorf("edges = %d, want 3", len(resp.Edges))
	}
	// Operator touches 2 edges, rivian touches 2, menlo park touches 2.
	for _, n := range resp.Nodes {
		if n.Degree != 2 {
			t.Errorf("node %s degree = %d, want 2", n.ID, n.Degree)
		}
	}
}

func TestGraph_TopOwnerScoping_NonAdmin(t *testing.T) {
	g := &fakeGraphStore{}
	h := &Handler{}
	h.AttachGraphStore(g)

	// Non-admin tries to spoof user_id; handler must ignore.
	req := httptest.NewRequest("GET", "/console/api/memory/graph?user_id=operator", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listGraph(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if g.lastTopOwner != "alison" {
		t.Errorf("ownerID passed to store = %q, want 'alison' (user_id query must be ignored for non-admin)",
			g.lastTopOwner)
	}
}

func TestGraph_TopOwnerScoping_AdminUserIDHonored(t *testing.T) {
	g := &fakeGraphStore{}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("GET", "/console/api/memory/graph?user_id=alison", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.listGraph(rec, req)

	if g.lastTopOwner != "alison" {
		t.Errorf("admin's user_id override = %q, want 'alison'", g.lastTopOwner)
	}
}

// ──────────────────────────────────────────────────────────────────
// Center path
// ──────────────────────────────────────────────────────────────────

func TestGraph_CenterRoutesToGraphAround(t *testing.T) {
	g := &fakeGraphStore{
		graphAroundEdges: []memory.GraphEdge{
			{ID: "r1", Subject: "operator", Predicate: "owns", Object: "rivian", Confidence: 1.0},
		},
	}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("GET", "/console/api/memory/graph?center=operator&depth=2&limit=20", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listGraph(rec, req)

	if g.lastAroundCenter != "operator" {
		t.Errorf("center = %q, want operator", g.lastAroundCenter)
	}
	if g.lastAroundOwner != "alison" {
		t.Errorf("ownerID = %q, want alison", g.lastAroundOwner)
	}
	if g.lastAroundDepth != 2 {
		t.Errorf("depth = %d, want 2", g.lastAroundDepth)
	}
	// Handler inflates limit by *5 so a popular node's edges aren't cut.
	if g.lastAroundLimit != 100 {
		t.Errorf("limit passed to store = %d, want 100 (limit*5)", g.lastAroundLimit)
	}
	// Top-path should NOT have been called.
	if g.lastTopOwner != "" {
		t.Errorf("graphTop was called for a center request: lastTopOwner=%q", g.lastTopOwner)
	}
}

// ──────────────────────────────────────────────────────────────────
// Entity search
// ──────────────────────────────────────────────────────────────────

func TestEntities_AutocompleteScoped(t *testing.T) {
	g := &fakeGraphStore{
		searchMatches: []memory.EntityMatch{
			{Name: "operator", Degree: 14},
			{Name: "alison", Degree: 9},
		},
	}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("GET", "/console/api/memory/entities?q=an&limit=5", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listEntities(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if g.lastSearchOwner != "alison" {
		t.Errorf("search owner = %q, want 'alison'", g.lastSearchOwner)
	}
	if g.lastSearchQ != "an" {
		t.Errorf("search q = %q, want 'an'", g.lastSearchQ)
	}
	if g.lastSearchLimit != 5 {
		t.Errorf("search limit = %d, want 5", g.lastSearchLimit)
	}
}

func TestEntityFacts_ScopedAndReturnsRows(t *testing.T) {
	g := &fakeGraphStore{
		entityFacts: []memory.MemoryRow{
			{ID: "f1", Content: "alison prefers pages over modals", SourceType: "explicit"},
			{ID: "f2", Content: "alison runs familiar on a NUC", SourceType: "conversation_extraction"},
		},
	}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("GET", "/console/api/memory/entity/alison/facts?limit=10", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req.SetPathValue("name", "alison")
	rec := httptest.NewRecorder()
	h.entityFacts(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if g.lastFactsEntity != "alison" || g.lastFactsOwner != "alison" {
		t.Errorf("scope: entity=%q owner=%q, want alison/alison", g.lastFactsEntity, g.lastFactsOwner)
	}
	if g.lastFactsLimit != 10 {
		t.Errorf("limit = %d, want 10", g.lastFactsLimit)
	}
	var out struct {
		Items []memoryRowDTO `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 2 || out.Items[0].ID != "f1" {
		t.Errorf("items round-trip wrong: %+v", out.Items)
	}
}

func TestMergeEntity_Scoped(t *testing.T) {
	g := &fakeGraphStore{mergeRewritten: 3}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("POST", "/console/api/memory/entity/operator/merge",
		strings.NewReader(`{"into":"drew"}`)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req.SetPathValue("name", "operator")
	rec := httptest.NewRecorder()
	h.mergeEntity(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if g.lastMergeFrom != "operator" || g.lastMergeTo != "drew" {
		t.Errorf("merge args = %q→%q, want operator→drew", g.lastMergeFrom, g.lastMergeTo)
	}
	if g.lastMergeOwner != "alison" {
		t.Errorf("merge owner = %q, want 'alison' (non-admin must stay self-scoped)", g.lastMergeOwner)
	}
}

func TestMergeEntity_MissingTargetRejected(t *testing.T) {
	h := &Handler{}
	h.AttachGraphStore(&fakeGraphStore{})
	req := httptest.NewRequest("POST", "/console/api/memory/entity/operator/merge",
		strings.NewReader(`{}`)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req.SetPathValue("name", "operator")
	rec := httptest.NewRecorder()
	h.mergeEntity(rec, req)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400 for empty into", rec.Code)
	}
}

func TestPatchRelationship_EditsAndValidates(t *testing.T) {
	g := &fakeGraphStore{
		relationship: &memory.GraphEdge{ID: "r1", Subject: "drew", Predicate: "drives", Object: "rivian", Confidence: 0.9},
	}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("PATCH", "/console/api/memory/relationship/r1",
		strings.NewReader(`{"predicate":"drives","confidence":0.9}`)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req.SetPathValue("id", "r1")
	rec := httptest.NewRecorder()
	h.patchRelationship(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if g.lastPatchPredicate != "drives" || g.lastPatchConfidence == nil || *g.lastPatchConfidence != 0.9 {
		t.Errorf("patch args wrong: pred=%q conf=%v", g.lastPatchPredicate, g.lastPatchConfidence)
	}
	if g.lastRelOwner != "alison" {
		t.Errorf("patch owner = %q, want 'alison'", g.lastRelOwner)
	}

	// Nothing to update → 400.
	req = httptest.NewRequest("PATCH", "/console/api/memory/relationship/r1",
		strings.NewReader(`{}`)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req.SetPathValue("id", "r1")
	rec = httptest.NewRecorder()
	h.patchRelationship(rec, req)
	if rec.Code != 400 {
		t.Errorf("empty patch status = %d, want 400", rec.Code)
	}

	// Out-of-range confidence → 400.
	req = httptest.NewRequest("PATCH", "/console/api/memory/relationship/r1",
		strings.NewReader(`{"confidence":1.5}`)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req.SetPathValue("id", "r1")
	rec = httptest.NewRecorder()
	h.patchRelationship(rec, req)
	if rec.Code != 400 {
		t.Errorf("bad confidence status = %d, want 400", rec.Code)
	}

	// Duplicate-key sentinel from the store → 409.
	g.relationshipErr = memory.ErrDuplicateRelationship
	req = httptest.NewRequest("PATCH", "/console/api/memory/relationship/r1",
		strings.NewReader(`{"predicate":"owns"}`)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req.SetPathValue("id", "r1")
	rec = httptest.NewRecorder()
	h.patchRelationship(rec, req)
	if rec.Code != 409 {
		t.Errorf("duplicate predicate status = %d, want 409", rec.Code)
	}
}

// ──────────────────────────────────────────────────────────────────
// Relationship detail
// ──────────────────────────────────────────────────────────────────

func TestRelationship_FoundReturnsTriple(t *testing.T) {
	g := &fakeGraphStore{
		relationship: &memory.GraphEdge{
			ID: "r-uuid", Subject: "operator", Predicate: "owns",
			Object: "rivian", Confidence: 0.92,
		},
	}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("GET", "/console/api/memory/relationship/r-uuid", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	req.SetPathValue("id", "r-uuid")
	rec := httptest.NewRecorder()
	h.getRelationship(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var out relationshipDetailDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Predicate != "owns" || out.Subject != "operator" {
		t.Errorf("triple round-trip wrong: %+v", out)
	}
}

// A global edge is visible to every user, but its supporting fact is
// only included when the caller's scope can see that memory row —
// otherwise a foreign fact's content would ride along with the edge.
func TestRelationship_ForeignSupportingFactHidden(t *testing.T) {
	g := &fakeGraphStore{
		relationship: &memory.GraphEdge{
			ID: "r-uuid", Subject: "shared-topic", Predicate: "relates_to",
			Object: "other-topic", Confidence: 0.8, SourceFact: "f-foreign",
		},
	}
	h := &Handler{memoryBrowser: &fakeMemoryBrowser{rows: map[string]*memory.MemoryRow{
		"f-foreign": {ID: "f-foreign", UserID: "operator", Content: "operator's private fact"},
	}}}
	h.AttachGraphStore(g)

	fetch := func(au AuthUser) relationshipDetailDTO {
		t.Helper()
		req := httptest.NewRequest("GET", "/console/api/memory/relationship/r-uuid", nil).WithContext(
			ctxWithAuth(context.Background(), au))
		req.SetPathValue("id", "r-uuid")
		rec := httptest.NewRecorder()
		h.getRelationship(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		var out relationshipDetailDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	if out := fetch(alisonUser()); out.Supporting != nil {
		t.Errorf("foreign supporting fact leaked to alison: %+v", out.Supporting)
	}
	if out := fetch(operatorAdmin()); out.Supporting == nil {
		t.Error("owner (operator) should still see the supporting fact")
	}
}

func TestRelationship_NotFoundReturns404(t *testing.T) {
	// nil + nil from the store maps to 404 — same existence-hiding
	// pattern memories and shards use.
	g := &fakeGraphStore{relationship: nil}
	h := &Handler{}
	h.AttachGraphStore(g)

	req := httptest.NewRequest("GET", "/console/api/memory/relationship/missing", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req.SetPathValue("id", "missing")
	rec := httptest.NewRecorder()
	h.getRelationship(rec, req)

	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ──────────────────────────────────────────────────────────────────
// Wiring guard
// ──────────────────────────────────────────────────────────────────

func TestGraph_UnwiredReturns503(t *testing.T) {
	h := &Handler{} // no AttachGraphStore
	req := httptest.NewRequest("GET", "/console/api/memory/graph", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listGraph(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
