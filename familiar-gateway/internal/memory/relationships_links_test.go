package memory

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testutil"
)

// A page's links_to edges are one per link. The unique key was
// (subject, predicate, owner), so saving a page that links to a, b and
// c left only the edge to c. Other predicates keep one object per
// subject (a changed value replaces the old one).
func TestRelationships_PageLinksAreMultiValued(t *testing.T) {
	pool := testutil.PgScopedPool(t, "memory_rel_test")
	testutil.TruncateTables(t, pool, "relationships")
	ctx := context.Background()
	// An upgraded database still has the old key; the migration replaces
	// it (the table is empty, so recreating it here can't fail).
	if _, err := pool.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_rel_subject_pred_user
		ON relationships (subject, predicate, user_id_key)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const page, scope = "page:eng/deploy", "book:b1"
	links := func() string {
		rows, err := pool.QueryContext(ctx, `SELECT object || '@' || user_id_key FROM relationships
			WHERE subject = $1 AND predicate = 'links_to' ORDER BY 1`, page)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var o string
			_ = rows.Scan(&o)
			out = append(out, o)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	if err := s.ReplacePageLinks(ctx, page, "alice", scope, []string{"page:eng/a", "page:eng/b", "page:eng/c"}); err != nil {
		t.Fatal(err)
	}
	if got := links(); got != "page:eng/a@alice page:eng/b@alice page:eng/c@alice" {
		t.Fatalf("after first save: %q, want all three links", got)
	}

	// A link removed from the page loses its edge; another member's save
	// replaces the set rather than adding a second one.
	if err := s.ReplacePageLinks(ctx, page, "bob", scope, []string{"page:eng/a", "page:eng/d"}); err != nil {
		t.Fatal(err)
	}
	if got := links(); got != "page:eng/a@bob page:eng/d@bob" {
		t.Fatalf("after bob's save: %q, want a and d, bob's", got)
	}
	// Saving the same links again updates them in place.
	if err := s.ReplacePageLinks(ctx, page, "bob", scope, []string{"page:eng/a", "Page:Eng/D"}); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if got := links(); got != "page:eng/a@bob page:eng/d@bob" {
		t.Fatalf("after re-save: %q", got)
	}

	// No links left: no edges.
	if err := s.ReplacePageLinks(ctx, page, "bob", scope, nil); err != nil {
		t.Fatal(err)
	}
	if got := links(); got != "" {
		t.Fatalf("after removing every link: %q", got)
	}

	// A single-valued predicate still replaces its object.
	for _, obj := range []string{"march", "april"} {
		if err := s.UpsertRelationships(ctx, []Relationship{{Subject: "acme", Predicate: "deadline", Object: obj, UserID: "alice"}}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var obj string
	if err := pool.QueryRowContext(ctx, `SELECT count(*), max(object) FROM relationships WHERE subject = 'acme' AND predicate = 'deadline'`).Scan(&n, &obj); err != nil {
		t.Fatal(err)
	}
	if n != 1 || obj != "april" {
		t.Errorf("deadline rows = %d (%s), want one, april", n, obj)
	}

	// The insert-if-absent path (relationship backfill) follows the
	// same keys.
	added, err := s.InsertRelationshipsIfAbsent(ctx, []Relationship{
		{Subject: page, Predicate: "links_to", Object: "page:eng/x", UserID: "bob", ScopeTag: scope},
		{Subject: page, Predicate: "links_to", Object: "page:eng/y", UserID: "bob", ScopeTag: scope},
		{Subject: "acme", Predicate: "deadline", Object: "may", UserID: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("added = %d, want both links and not the existing deadline", added)
	}

	// The old index is gone after migration; its replacements exist.
	var idx []string
	rows, err := pool.QueryContext(ctx, `SELECT indexname FROM pg_indexes WHERE tablename = 'relationships' AND schemaname = current_schema() ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		idx = append(idx, name)
	}
	joined := strings.Join(idx, ",")
	if strings.Contains(joined, "idx_rel_subject_pred_user,") || !strings.Contains(joined, "idx_rel_links_to") || !strings.Contains(joined, "idx_rel_subject_pred_user_single") {
		t.Errorf("relationships indexes = %s", joined)
	}
}
