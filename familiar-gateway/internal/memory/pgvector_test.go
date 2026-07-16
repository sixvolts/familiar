package memory

import (
	"context"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testutil"
)

// These tests opt in via FAMILIAR_TEST_DSN and exercise the dedup
// signal that gateway's summarize.go relies on to skip re-committing
// facts that already exist as live memories. The queries under test
// are NearestSimilarity's scope-filtered and scope-less branches plus
// the Search method's superseded-row exclusion.
//
// We insert pre-computed 1024-dim unit vectors on specific axes so
// cosine similarity is analytic: same axis → 1.0, orthogonal → 0.0.
// That avoids any dependency on a running embedder and keeps asserts
// tight.

const testDim = 1024

// axisVec returns a 1024-dim unit vector with 1.0 on the given axis.
// Cosine similarity between two axis vectors is 1 iff they share an
// axis, 0 otherwise.
func axisVec(axis int) []float32 {
	v := make([]float32, testDim)
	v[axis] = 1.0
	return v
}

// mixVec blends two axes with weights (a, b). Caller is responsible
// for picking a normalized pair; we don't enforce it.
func mixVec(axisA, axisB int, a, b float32) []float32 {
	v := make([]float32, testDim)
	v[axisA] = a
	v[axisB] = b
	return v
}

// setupMemoryStore migrates and returns a store pinned to a dedicated
// `memory_test` schema rather than public. Two reasons:
//
//   - The schema is owned by db.Migrate now (the old per-test CREATE
//     TABLE bootstrap predates memories_base), so tests must run the
//     real migrations to see the real table shape.
//
//   - Several tests below exercise the legacy "user_id IS NULL means
//     global" retrieval predicate, but memories_user_id_constraints
//     re-asserts SET NOT NULL on every Migrate run. We relax the
//     column after migrating to emulate a pre-multi-user deployment;
//     isolating that relaxation in its own schema keeps public
//     faithful to production, and truncating BEFORE Migrate keeps the
//     re-asserted SET NOT NULL from tripping over the NULL rows a
//     previous test left behind.
func setupMemoryStore(t *testing.T) *PgVectorStore {
	t.Helper()
	dsn := os.Getenv(testutil.EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s not set", testutil.EnvDSN)
	}
	ctx := context.Background()

	admin := testutil.PgTestDB(t)
	if _, err := admin.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Skipf("pgvector extension unavailable: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS memory_test`); err != nil {
		t.Fatalf("create memory_test schema: %v", err)
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := db.Open(dsn + sep + "options=" + url.QueryEscape("-csearch_path=memory_test,public"))
	if err != nil {
		t.Fatalf("db.Open (memory_test schema): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	if _, err := pool.ExecContext(ctx, `
		DO $$ BEGIN
		    IF to_regclass('memory_test.memories') IS NOT NULL THEN
		        EXECUTE 'TRUNCATE memory_test.memories CASCADE';
		    END IF;
		END $$`); err != nil {
		t.Fatalf("pre-migrate truncate: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`ALTER TABLE memories ALTER COLUMN user_id DROP NOT NULL`); err != nil {
		t.Fatalf("relax user_id for legacy-row tests: %v", err)
	}

	return &PgVectorStore{db: pool}
}

// insertMemory adds one row and returns its uuid so tests can wire up
// supersedes chains.
func insertMemory(t *testing.T, s *PgVectorStore, content, scope string, vec []float32) string {
	t.Helper()
	var id string
	err := s.db.QueryRowContext(context.Background(),
		`INSERT INTO memories (agent_id, scope, content, embedding, source_type)
		 VALUES ('test', $1, $2, $3::vector, 'test')
		 RETURNING id`,
		scope, content, vectorToString(vec)).Scan(&id)
	if err != nil {
		t.Fatalf("insert %q: %v", content, err)
	}
	return id
}

func insertSupersedingMemory(t *testing.T, s *PgVectorStore, content, scope string, vec []float32, supersedes string) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO memories (agent_id, scope, content, embedding, source_type, supersedes)
		 VALUES ('test', $1, $2, $3::vector, 'test', $4)`,
		scope, content, vectorToString(vec), supersedes)
	if err != nil {
		t.Fatalf("insert superseding %q: %v", content, err)
	}
}

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) < tol
}

// --- NearestSimilarity empty-input branches --------------------------------

func TestNearestSimilarity_EmptyStore(t *testing.T) {
	s := setupMemoryStore(t)
	sim, ok, err := s.NearestSimilarity(context.Background(), axisVec(0), "", "")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if ok {
		t.Errorf("empty store returned ok=true, sim=%v", sim)
	}
	if sim != 0 {
		t.Errorf("empty store sim = %v, want 0", sim)
	}
}

func TestNearestSimilarity_EmptyVector(t *testing.T) {
	s := setupMemoryStore(t)
	insertMemory(t, s, "anything", "session", axisVec(0))
	sim, ok, err := s.NearestSimilarity(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("NearestSimilarity(nil): %v", err)
	}
	if ok || sim != 0 {
		t.Errorf("nil vector should short-circuit: got ok=%v sim=%v", ok, sim)
	}
}

// A knowledge row whose embedding is NULL (embedder was down at write
// time) must not break HybridSearch: it should still surface via the
// FTS arm with a 0 similarity, not fail the whole query when the fused
// projection scans a NULL cosine into a float64.
func TestHybridSearch_NullEmbeddingFTSHitDoesNotCrash(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (agent_id, scope, content, embedding, source_type)
		 VALUES ('test', 'user', 'quantum entanglement briefing notes', NULL, 'explicit')`); err != nil {
		t.Fatalf("insert null-embedding row: %v", err)
	}
	// A populated dense candidate too, so the fused set mixes both arms.
	insertMemory(t, s, "unrelated dense fact", "user", axisVec(2))

	results, err := s.HybridSearch(ctx, "quantum entanglement", axisVec(1), 10, 0.0, "")
	if err != nil {
		t.Fatalf("HybridSearch errored on a NULL-embedding FTS hit: %v", err)
	}
	var found bool
	for _, r := range results {
		if r.Content == "quantum entanglement briefing notes" {
			found = true
			if r.Similarity != 0 {
				t.Errorf("NULL-embedding row similarity = %v, want 0", r.Similarity)
			}
		}
	}
	if !found {
		t.Error("NULL-embedding FTS hit was dropped from hybrid results")
	}
}

// --- Similarity math -------------------------------------------------------

// The write-time conflict resolver must never pick a wiki_page or raw
// conversation row as a supersede target — a wiki target would set a
// pointer the page's next clean-replace trips over (23503). Even when a
// wiki/chunk row is the closest match, NearestLiveFact returns the
// nearest KNOWLEDGE row and NearestSimilarity ignores the excluded
// kinds entirely.
func TestNearestLiveFact_ExcludesWikiAndChunks(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	insertTyped := func(content, sourceType string, vec []float32) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO memories (agent_id, scope, content, embedding, source_type, user_id)
			 VALUES ('test', 'user', $1, $2::vector, $3, 'u1')`,
			content, vectorToString(vec), sourceType); err != nil {
			t.Fatalf("insert %s: %v", sourceType, err)
		}
	}
	q := axisVec(1)
	insertTyped("wiki says the sky is teal", "wiki_page", q)            // exact match, excluded
	insertTyped("raw chunk about the sky", "conversation", q)           // exact match, excluded
	insertTyped("the sky is blue", "explicit", mixVec(1, 2, 0.9, 0.44)) // knowledge, slightly off

	nf, ok, err := s.NearestLiveFact(ctx, q, "u1")
	if err != nil {
		t.Fatalf("NearestLiveFact: %v", err)
	}
	if !ok {
		t.Fatal("expected a knowledge match")
	}
	if nf.Content != "the sky is blue" {
		t.Errorf("conflict target = %q, want the knowledge row (wiki/chunk must be excluded)", nf.Content)
	}

	// NearestSimilarity (NOOP-skip dedup) must not report a ~1.0 match
	// against the excluded wiki/chunk rows — the nearest eligible row is
	// the slightly-off knowledge fact.
	sim, ok, err := s.NearestSimilarity(ctx, q, "", "u1")
	if err != nil || !ok {
		t.Fatalf("NearestSimilarity: ok=%v err=%v", ok, err)
	}
	if sim > 0.999 {
		t.Errorf("NearestSimilarity = %v — matched an excluded wiki/chunk row", sim)
	}
}

func TestNearestSimilarity_IdenticalVectorIsOne(t *testing.T) {
	s := setupMemoryStore(t)
	vec := axisVec(3)
	insertMemory(t, s, "favorite color is blue", "session", vec)

	sim, ok, err := s.NearestSimilarity(context.Background(), vec, "", "")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for populated store")
	}
	if !approxEqual(sim, 1.0, 1e-5) {
		t.Errorf("identical vec sim = %v, want ~1.0", sim)
	}
}

func TestNearestSimilarity_OrthogonalIsZero(t *testing.T) {
	s := setupMemoryStore(t)
	insertMemory(t, s, "stored", "session", axisVec(0))

	sim, ok, err := s.NearestSimilarity(context.Background(), axisVec(1), "", "")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if !ok {
		t.Fatal("ok should be true when at least one row exists")
	}
	if !approxEqual(sim, 0.0, 1e-5) {
		t.Errorf("orthogonal sim = %v, want ~0.0", sim)
	}
}

func TestNearestSimilarity_PicksClosestOfMany(t *testing.T) {
	s := setupMemoryStore(t)
	// Three memories on distinct axes.
	insertMemory(t, s, "a", "session", axisVec(0))
	insertMemory(t, s, "b", "session", axisVec(1))
	insertMemory(t, s, "c", "session", axisVec(2))

	// Query a vector that leans heavily toward axis 1. Nearest should
	// be memory b with sim ~= the axis-1 component.
	q := mixVec(1, 2, 0.99, 0.14)
	sim, ok, err := s.NearestSimilarity(context.Background(), q, "", "")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if !ok {
		t.Fatal("ok should be true")
	}
	// Cosine sim between normalized q and axisVec(1) = 0.99 / sqrt(.99^2 + .14^2)
	// which is ~= 0.9902. We just assert we landed in the right ballpark.
	if sim < 0.9 {
		t.Errorf("expected nearest to be the axis-1 memory (sim ~0.99), got %v", sim)
	}
}

// --- Scope filter ----------------------------------------------------------

func TestNearestSimilarity_ScopeFilterIncludes(t *testing.T) {
	s := setupMemoryStore(t)
	insertMemory(t, s, "session fact", "session", axisVec(0))
	insertMemory(t, s, "permanent fact", "permanent", axisVec(0))

	sim, ok, err := s.NearestSimilarity(context.Background(), axisVec(0), "session", "")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if !ok || !approxEqual(sim, 1.0, 1e-5) {
		t.Errorf("scope=session should match: ok=%v sim=%v", ok, sim)
	}
}

func TestNearestSimilarity_ScopeFilterExcludes(t *testing.T) {
	s := setupMemoryStore(t)
	// Only a "permanent" row exists; a "session"-scoped query should
	// return no matches even though the vector is identical.
	insertMemory(t, s, "permanent fact", "permanent", axisVec(0))

	sim, ok, err := s.NearestSimilarity(context.Background(), axisVec(0), "session", "")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if ok {
		t.Errorf("scope=session should not match permanent-only store: sim=%v", sim)
	}
}

// --- Superseded-row exclusion (the dedup-path guarantee) -------------------

func TestNearestSimilarity_SkipsSupersededRow(t *testing.T) {
	s := setupMemoryStore(t)
	// A fact on axis 0 is later superseded by a newer fact on axis 1.
	// A dedup query on axis 0 must NOT see the old superseded row.
	oldID := insertMemory(t, s, "old fact", "session", axisVec(0))
	insertSupersedingMemory(t, s, "new fact", "session", axisVec(1), oldID)

	// Query on axis 0 — the only remaining match should be the new row
	// on axis 1, so sim ~= 0 (orthogonal), not ~= 1.
	sim, ok, err := s.NearestSimilarity(context.Background(), axisVec(0), "session", "")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true from the live row")
	}
	if sim > 0.5 {
		t.Errorf("superseded row leaked into dedup query: sim=%v", sim)
	}
}

func TestNearestSimilarity_LiveRowStillMatches(t *testing.T) {
	// Inverse of the above: the NEW row's vector should still match a
	// query that lines up with IT.
	s := setupMemoryStore(t)
	oldID := insertMemory(t, s, "old fact", "session", axisVec(0))
	insertSupersedingMemory(t, s, "new fact", "session", axisVec(1), oldID)

	sim, ok, err := s.NearestSimilarity(context.Background(), axisVec(1), "session", "")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if !ok || !approxEqual(sim, 1.0, 1e-5) {
		t.Errorf("live row should match: ok=%v sim=%v", ok, sim)
	}
}

// --- Search() superseded exclusion ----------------------------------------
//
// Search shares the same supersedes filter clause, so a focused test
// here guards the gateway's memory retrieval path from regressing when
// someone edits the SQL.

func TestSearch_ExcludesSupersededRows(t *testing.T) {
	s := setupMemoryStore(t)
	oldID := insertMemory(t, s, "old content", "session", axisVec(0))
	insertSupersedingMemory(t, s, "new content", "session", axisVec(0), oldID)

	results, err := s.Search(context.Background(), axisVec(0), 10, 0.5, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Only the new row should come back — the old one is superseded.
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1: %+v", len(results), results)
	}
	if results[0].Content != "new content" {
		t.Errorf("leaked superseded content: %s", results[0].Content)
	}
}

func TestSearch_FiltersConversationSourceType(t *testing.T) {
	// Raw conversation turns are written into memories with
	// source_type='conversation' and must not show up in semantic
	// retrieval — the gateway already loads them verbatim from the
	// session manager.
	s := setupMemoryStore(t)
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO memories (agent_id, scope, content, embedding, source_type)
		 VALUES ('test', 'session', 'raw turn', $1::vector, 'conversation')`,
		vectorToString(axisVec(0)))
	if err != nil {
		t.Fatalf("insert raw turn: %v", err)
	}

	results, err := s.Search(context.Background(), axisVec(0), 10, 0.5, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("conversation source_type should be filtered, got %+v", results)
	}
}

// --- Multi-user scoping (Phase C.2) ---------------------------------------

// insertUserMemory writes a row with user_id set so the multi-user
// tests can exercise the (user_id IS NULL OR user_id = $n) predicate.
// An empty userID inserts a NULL (= global) row, matching the helper's
// "no owner supplied" semantics.
func insertUserMemory(t *testing.T, s *PgVectorStore, content, scope string, vec []float32, userID string) {
	t.Helper()
	var err error
	if userID == "" {
		_, err = s.db.ExecContext(context.Background(),
			`INSERT INTO memories (agent_id, scope, content, embedding, source_type, user_id)
			 VALUES ('test', $1, $2, $3::vector, 'test', NULL)`,
			scope, content, vectorToString(vec))
	} else {
		_, err = s.db.ExecContext(context.Background(),
			`INSERT INTO memories (agent_id, scope, content, embedding, source_type, user_id)
			 VALUES ('test', $1, $2, $3::vector, 'test', $4)`,
			scope, content, vectorToString(vec), userID)
	}
	if err != nil {
		t.Fatalf("insert %q (user=%q): %v", content, userID, err)
	}
}

func TestSearch_UserAndGlobalVisible(t *testing.T) {
	s := setupMemoryStore(t)
	insertUserMemory(t, s, "global fact", "user", axisVec(0), "")
	insertUserMemory(t, s, "alice private", "user", axisVec(0), "alice")
	insertUserMemory(t, s, "bob private", "user", axisVec(0), "bob")

	// Alice sees global + her own, never bob.
	results, err := s.Search(context.Background(), axisVec(0), 10, 0.5, "alice")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[string]bool{}
	for _, r := range results {
		got[r.Content] = true
	}
	if !got["global fact"] || !got["alice private"] {
		t.Errorf("alice missing global/own: %+v", got)
	}
	if got["bob private"] {
		t.Errorf("alice saw bob's row: %+v", got)
	}
}

func TestSearch_EmptyUserIDReturnsGlobalOnly(t *testing.T) {
	s := setupMemoryStore(t)
	insertUserMemory(t, s, "global fact", "user", axisVec(0), "")
	insertUserMemory(t, s, "alice private", "user", axisVec(0), "alice")

	// No userID → only NULL-owner rows are returned.
	results, err := s.Search(context.Background(), axisVec(0), 10, 0.5, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Content != "global fact" {
		t.Errorf("empty userID should return only global: %+v", results)
	}
}

func TestNearestSimilarity_UserScoped(t *testing.T) {
	s := setupMemoryStore(t)
	// Bob's row on axis 0, global on axis 1. Alice querying axis 0
	// must not see Bob's row — the nearest live match from Alice's
	// perspective is the orthogonal global fact on axis 1.
	insertUserMemory(t, s, "bob axis0", "session", axisVec(0), "bob")
	insertUserMemory(t, s, "global axis1", "session", axisVec(1), "")

	sim, ok, err := s.NearestSimilarity(context.Background(), axisVec(0), "", "alice")
	if err != nil {
		t.Fatalf("NearestSimilarity: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true (global row is visible)")
	}
	if sim > 0.5 {
		t.Errorf("alice saw bob's high-similarity row: sim=%v", sim)
	}
}

// --- Plumbing sanity -------------------------------------------------------

func TestVectorToString(t *testing.T) {
	v := []float32{0.1, -0.5, 1.0}
	got := vectorToString(v)
	if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
		t.Errorf("missing brackets: %s", got)
	}
	// %g may emit "0.1" or "0.100000"; assert the commas are present.
	if strings.Count(got, ",") != 2 {
		t.Errorf("expected 2 commas, got %s", got)
	}
}

func TestSanitizeDSN(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"postgres://user:secret@host/db", "postgres://user:***@host/db"},
		{"postgres://user@host/db", "postgres://user@host/db"},
		{"host/db", "host/db"},
	}
	for _, tc := range cases {
		if got := db.SanitizeDSN(tc.in); got != tc.want {
			t.Errorf("SanitizeDSN(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
