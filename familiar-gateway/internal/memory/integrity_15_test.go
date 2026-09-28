package memory

// Store tests for batch 15's memory fixes: entity case, re-embed queue
// on edits, branched chain collapse, backfill inserts and listing,
// word-bounded graph matching, and knowledge-only dashboard counts.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testutil"
)

func relStoreForTest(t *testing.T) *PgRelationshipStore {
	t.Helper()
	pool := testutil.PgScopedPool(t, "memory_rel_test")
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return s
}

// Objects are stored lowercase, like subjects, so every lookup reaches
// them: "Acme" was an entity no traversal, merge or delete could find.
// Existing mixed-case objects are lowercased by the migration.
func TestRelationships_ObjectsAreLowercase(t *testing.T) {
	s := relStoreForTest(t)
	ctx := context.Background()
	u := "case-user"
	if err := s.UpsertRelationships(ctx, []Relationship{
		{Subject: "drew", Predicate: "works_at", Object: "Acme", UserID: u},
		{Subject: "acme", Predicate: "located_in", Object: "boston", UserID: u},
	}); err != nil {
		t.Fatal(err)
	}
	if n := countRelsWhere(t, s, `user_id = $1 AND object = 'Acme'`, u); n != 0 {
		t.Errorf("%d objects kept their case", n)
	}
	rels, err := s.TraverseFrom(ctx, "drew", u, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 {
		t.Errorf("traversal from drew reached %d edges, want both (through acme): %+v", len(rels), rels)
	}
	if n, err := s.DeleteEntity(ctx, "Acme", u); err != nil || n != 2 {
		t.Errorf("DeleteEntity(Acme) = %d, %v; want both edges", n, err)
	}

	// The data fix.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO relationships (subject, predicate, object, user_id) VALUES ('rex', 'owned_by', 'Drew', $1)`, u); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	if n := countRelsWhere(t, s, `user_id = $1 AND object = 'drew'`, u); n != 1 {
		t.Errorf("migration left the object's case: %d lowercase rows", n)
	}
}

// Only an edited fact's missing vector is queued for re-embedding; the
// sweep reads only the queue, so the row had no vector for good.
func TestUpdateMemoryContent_QueuesReembedWithoutVector(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	id := insertMemory(t, s, "I work at Acme", "user", axisVec(5))
	queued := func() bool {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pending_embeds WHERE memory_id = $1::uuid`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if err := s.UpdateMemoryContent(ctx, id, "I work at Globex", "console", axisVec(6)); err != nil {
		t.Fatal(err)
	}
	if queued() {
		t.Error("an edit with a vector was queued")
	}
	if err := s.UpdateMemoryContent(ctx, id, "I work at Initech", "console", nil); err != nil {
		t.Fatal(err)
	}
	if !queued() {
		t.Error("an edit without a vector wasn't queued for re-embedding")
	}
}

// A branched chain (two newer facts pointing at one older) keeps both
// live facts, from either entry point. The collapse deleted the other
// live head, and started from one head it failed on the other's
// pointer.
func TestCollapseChain_BranchedKeepsEveryLiveRow(t *testing.T) {
	for _, start := range []string{"old", "w1"} {
		t.Run("from "+start, func(t *testing.T) {
			s := setupMemoryStore(t)
			ctx := context.Background()
			ids := map[string]string{
				"old": insertMemory(t, s, "drew lives in nyc", "user", axisVec(1)),
				"w1":  insertMemory(t, s, "drew lives in new york city", "user", axisVec(2)),
				"w2":  insertMemory(t, s, "drew lives in new york", "user", axisVec(3)),
			}
			setSupersedes(t, s, ids["w1"], ids["old"])
			setSupersedes(t, s, ids["w2"], ids["old"])
			n, tip, err := s.CollapseChain(ctx, ids[start])
			if err != nil {
				t.Fatalf("CollapseChain: %v", err)
			}
			if n != 1 {
				t.Errorf("deleted %d rows, want 1 (the superseded one)", n)
			}
			if tip != ids["w1"] && tip != ids["w2"] {
				t.Errorf("tip = %s, want a live row", tip)
			}
			for _, k := range []string{"w1", "w2"} {
				sup, hidden, exists := chainState(t, s, ids[k])
				if !exists || hidden || sup != "" {
					t.Errorf("%s: exists=%v hidden=%v supersedes=%q", k, exists, hidden, sup)
				}
			}
			if _, _, exists := chainState(t, s, ids["old"]); exists {
				t.Error("the superseded row survived")
			}
		})
	}
}

// The backfill adds missing triples and leaves existing ones (which
// the user may have re-weighted or re-pointed) alone.
func TestInsertRelationshipsIfAbsent_KeepsExisting(t *testing.T) {
	s := relStoreForTest(t)
	ctx := context.Background()
	u := "backfill-user"
	if err := s.UpsertRelationships(ctx, []Relationship{
		{Subject: "rune", Predicate: "has_ip", Object: "10.0.0.9", UserID: u, Confidence: 0.3},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.InsertRelationshipsIfAbsent(ctx, []Relationship{
		{Subject: "rune", Predicate: "has_ip", Object: "10.0.0.5", UserID: u, Confidence: 0.9},
		{Subject: "Rune", Predicate: "runs_os", Object: "Debian", UserID: u, Confidence: 0.9, ScopeTag: "shard:lab"},
	})
	if err != nil || n != 1 {
		t.Fatalf("added %d, %v; want 1", n, err)
	}
	if c := countRelsWhere(t, s, `user_id = $1 AND subject = 'rune' AND predicate = 'has_ip' AND object = '10.0.0.9' AND abs(confidence - 0.3) < 0.001`, u); c != 1 {
		t.Error("the existing edge was changed")
	}
	if c := countRelsWhere(t, s, `user_id = $1 AND subject = 'rune' AND object = 'debian' AND scope_tag = 'shard:lab'`, u); c != 1 {
		t.Error("the new edge is missing, not lowercased, or lost its scope")
	}
}

// The backfill listing needs a user and carries scope tags.
func TestListForBackfill_UserAndScope(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	if _, err := s.ListForBackfill(ctx, ""); err == nil {
		t.Error("an empty user was accepted")
	}
	insertFor(t, s, "bf-user", "top-level fact", "", axisVec(1))
	insertFor(t, s, "bf-user", "shard fact", "shard:lab", axisVec(2))
	items, err := s.ListForBackfill(ctx, "bf-user")
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]string{}
	for _, it := range items {
		tags[it.Content] = it.ScopeTag
	}
	if tags["top-level fact"] != "" || tags["shard fact"] != "shard:lab" {
		t.Errorf("scope tags = %v", tags)
	}
}

// Graph context matches entity names as words: "art" isn't in "start".
func TestRelatedForContents_WholeWords(t *testing.T) {
	s := relStoreForTest(t)
	ctx := context.Background()
	u := "words-user"
	if err := s.UpsertRelationships(ctx, []Relationship{
		{Subject: "art", Predicate: "hangs_in", Object: "hall", UserID: u},
		{Subject: "acme", Predicate: "located_in", Object: "boston", UserID: u},
		{Subject: "gpu-host", Predicate: "has", Object: "4090", UserID: u},
		{Subject: "ai", Predicate: "is", Object: "short", UserID: u},
	}); err != nil {
		t.Fatal(err)
	}
	rels, err := s.RelatedForContents(ctx, []string{"We start at Acme.", "Said the gpu-host owner: fine."}, u, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rels {
		got[r.Subject] = true
	}
	if !got["acme"] || !got["gpu-host"] || got["art"] || got["ai"] || len(got) != 2 {
		t.Errorf("matched %v, want acme and gpu-host only", got)
	}
}

// Dashboard counts and recent writes are knowledge, not transcript.
func TestDashboardQueries_ExcludeConversationChunks(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	u := "dash-user"
	insertFor(t, s, u, "drew likes tea", "", axisVec(1))
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (agent_id, scope, content, source_type, user_id)
		VALUES ('test', 'session', 'user: hi assistant: hello', 'conversation', $1),
		       ('test', 'session', 'user: tea? assistant: sure', 'conversation', $1)`, u); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountFactsForUser(ctx, u, false); err != nil || n != 1 {
		t.Errorf("CountFactsForUser = %d, %v; want 1", n, err)
	}
	recent, err := s.RecentFactsForUser(ctx, u, 5, false)
	if err != nil || len(recent) != 1 || recent[0].Content != "drew likes tea" {
		t.Errorf("RecentFactsForUser = %+v, %v", recent, err)
	}
	points, err := s.GrowthSparkline(ctx, u, 1)
	if err != nil || len(points) != 1 || points[0].FactCount != 1 {
		t.Errorf("GrowthSparkline = %+v, %v; want 1 fact today", points, err)
	}
}

// A row embedded with another dimension (the embedding model changed)
// is skipped by dense search instead of failing it: pgvector refuses to
// compare the two, and one such row turned off memory for every turn.
func TestDenseSearch_SkipsOtherDimensions(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	u := "dims-user"
	insertFor(t, s, u, "same dimension fact", "", axisVec(7))
	insertFor(t, s, u, "old model fact", "", []float32{1, 0, 0})
	if res, err := s.Search(ctx, axisVec(7), 5, 0.1, u); err != nil || len(res) != 1 {
		t.Errorf("Search = %d results, %v", len(res), err)
	}
	if res, err := s.HybridSearch(ctx, "fact", axisVec(7), 5, 0.1, u); err != nil || len(res) == 0 {
		t.Errorf("HybridSearch = %d results, %v", len(res), err)
	}
	if res, err := s.NearestLiveFacts(ctx, axisVec(7), u, "", 5); err != nil || len(res) != 1 {
		t.Errorf("NearestLiveFacts = %d results, %v", len(res), err)
	}
	if res, err := s.SearchInScope(ctx, axisVec(7), 5, 0.1, u, ""); err != nil {
		t.Errorf("SearchInScope: %v (%d results)", err, len(res))
	}
}

// Each user's turns find that user's entities: the vocabulary was the
// first admin's graph for everyone. It loads on first use (FindIn
// doesn't wait) and matches whole words.
func TestEntityVocab_PerUserWords(t *testing.T) {
	s := relStoreForTest(t)
	ctx := context.Background()
	if err := s.UpsertRelationships(ctx, []Relationship{
		{Subject: "alice-nas", Predicate: "stores", Object: "photos", UserID: "vocab-alice"},
		{Subject: "bob-nas", Predicate: "backs_up", Object: "laptop", UserID: "vocab-bob"},
		{Subject: "art", Predicate: "hangs_in", Object: "hall", UserID: "vocab-bob"},
	}); err != nil {
		t.Fatal(err)
	}
	v := NewEntityVocab(s, time.Hour)
	v.Start(ctx)
	find := func(user, text string) []string {
		var got []string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			got = v.FindIn(user, text)
			if v.Size(user) > 0 {
				return v.FindIn(user, text)
			}
			time.Sleep(20 * time.Millisecond)
		}
		return got
	}
	if got := find("vocab-bob", "is the bob-nas full? the art hangs"); strings.Join(got, ",") != "bob-nas,art" {
		t.Errorf("bob finds %v, want bob-nas and art", got)
	}
	if got := find("vocab-alice", "is the bob-nas full? alice-nas is"); strings.Join(got, ",") != "alice-nas" {
		t.Errorf("alice finds %v, want only her alice-nas", got)
	}
	if got := v.FindIn("vocab-bob", "we start the laptop backup"); strings.Join(got, ",") != "laptop" {
		t.Errorf("word matching: %v, want laptop only (not art in start)", got)
	}
}
