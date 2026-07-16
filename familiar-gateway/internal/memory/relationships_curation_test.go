package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/familiar/gateway/internal/testutil"
)

// Phase C curation surface (MEMORY-UI-SPEC §5). DB-gated like the
// other relationships suites.

func countRelsWhere(t *testing.T, s *PgRelationshipStore, where string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM relationships WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestMergeEntities_RewritesDedupesAndDropsSelfLoops(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := context.Background()
	u, other := "merge-user", "other-user"

	seed := []Relationship{
		{Subject: "operator", Predicate: "owns", Object: "rivian", UserID: u},   // collides post-merge
		{Subject: "drew", Predicate: "owns", Object: "rivian", UserID: u},       // established row wins
		{Subject: "operator", Predicate: "likes", Object: "coffee", UserID: u},  // clean subject rewrite
		{Subject: "home", Predicate: "houses", Object: "operator", UserID: u},   // object rewrite
		{Subject: "operator", Predicate: "knows", Object: "drew", UserID: u},    // becomes self-loop → dropped
		{Subject: "operator", Predicate: "owns", Object: "boat", UserID: other}, // other tenant untouched
	}
	if err := s.UpsertRelationships(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := s.MergeEntities(ctx, "operator", "drew", u); err != nil {
		t.Fatalf("MergeEntities: %v", err)
	}

	if n := countRelsWhere(t, s, `user_id = $1 AND (subject = 'operator' OR object = 'operator')`, u); n != 0 {
		t.Errorf("%d rows still reference the merged-away entity", n)
	}
	if n := countRelsWhere(t, s, `user_id = $1 AND subject = 'drew' AND predicate = 'owns'`, u); n != 1 {
		t.Errorf("(drew, owns) rows = %d, want exactly 1 after collision dedupe", n)
	}
	if n := countRelsWhere(t, s, `user_id = $1 AND subject = 'drew' AND predicate = 'likes' AND object = 'coffee'`, u); n != 1 {
		t.Errorf("subject rewrite missing (drew likes coffee): %d", n)
	}
	if n := countRelsWhere(t, s, `user_id = $1 AND subject = 'home' AND object = 'drew'`, u); n != 1 {
		t.Errorf("object rewrite missing (home houses drew): %d", n)
	}
	if n := countRelsWhere(t, s, `user_id = $1 AND subject = object`, u); n != 0 {
		t.Errorf("self-loop survived the merge: %d", n)
	}
	if n := countRelsWhere(t, s, `user_id = $1 AND subject = 'operator'`, other); n != 1 {
		t.Errorf("another tenant's rows were touched (operator rows = %d, want 1)", n)
	}
}

func TestUpdateRelationship_EditsAndConflicts(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := context.Background()
	u := "edit-user"

	if err := s.UpsertRelationships(ctx, []Relationship{
		{Subject: "drew", Predicate: "owns", Object: "rivian", UserID: u},
		{Subject: "drew", Predicate: "drives", Object: "rivian", UserID: u},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var id string
	if err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM relationships WHERE subject = 'drew' AND predicate = 'owns'`).Scan(&id); err != nil {
		t.Fatalf("fetch id: %v", err)
	}

	conf := 0.42
	edge, err := s.UpdateRelationship(ctx, id, u, "commutes_in", &conf)
	if err != nil {
		t.Fatalf("UpdateRelationship: %v", err)
	}
	if edge == nil || edge.Predicate != "commutes_in" || edge.Confidence < 0.41 || edge.Confidence > 0.43 {
		t.Errorf("edit round-trip wrong: %+v", edge)
	}

	// Predicate collision with the (drew, drives) row → sentinel.
	if _, err := s.UpdateRelationship(ctx, id, u, "drives", nil); !errors.Is(err, ErrDuplicateRelationship) {
		t.Errorf("collision error = %v, want ErrDuplicateRelationship", err)
	}

	// Wrong tenant → invisible, nil/nil.
	edge, err = s.UpdateRelationship(ctx, id, "someone-else", "stole", nil)
	if err != nil || edge != nil {
		t.Errorf("cross-tenant edit: edge=%v err=%v, want nil/nil", edge, err)
	}
}
