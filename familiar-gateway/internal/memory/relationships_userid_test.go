package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/testutil"
)

// The relationships graph is read with pgvector-style visibility
// (user_id IS NULL OR user_id = $n), so a NULL/empty user_id edge is
// visible to EVERY tenant. The relationships_user_id_nonempty CHECK
// (migrate.go) is the DB backstop that fails such a write closed. This
// pins it: an empty-owner upsert must be rejected, a real-owner one
// must succeed. (DB-gated like the other relationships suites.)
func TestUpsertRelationships_RejectsEmptyUserID(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := context.Background()

	// Empty owner → rejected by the CHECK, no globally-visible edge.
	err = s.UpsertRelationships(ctx, []Relationship{
		{Subject: "a", Predicate: "leaks_to", Object: "b", UserID: ""},
	})
	if err == nil {
		t.Fatal("empty user_id edge was accepted — tenant-leak backstop missing")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "user_id") &&
		!strings.Contains(strings.ToLower(err.Error()), "constraint") &&
		!strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Errorf("unexpected error (want CHECK violation): %v", err)
	}

	// A real owner still writes fine.
	if err := s.UpsertRelationships(ctx, []Relationship{
		{Subject: "a", Predicate: "owned_edge", Object: "b", UserID: "real-user"},
	}); err != nil {
		t.Fatalf("owned upsert should succeed: %v", err)
	}
}
