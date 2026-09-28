package memory

import (
	"context"
	"testing"

	"github.com/familiar/gateway/internal/testutil"
)

// Triples extracted from an ISOLATED shard's turns must not leak into
// the top-level retrieval path (RelatedForContents / TraverseFrom) that
// feeds the chat prompt — mirroring how the memories from those same
// turns are already excluded. A global triple (no scope_tag) and a
// triple scoped to a NON-isolated shard both stay visible.
func TestRelationshipRetrieval_ExcludesIsolatedShardTriples(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := context.Background()

	// A user + one isolated shard and one promoted (visible) shard.
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO users (id, display_name, status)
	          VALUES ('iso-owner', 'Iso', 'approved') ON CONFLICT (id) DO NOTHING`)
	mustExec(`INSERT INTO shards (id, owner_id, name, persistence, visibility, scope_tag, system_prompt)
	          VALUES ('iso-shard', 'iso-owner', 'Iso', 'persistent', 'isolated', 'shard:iso', 'p')
	          ON CONFLICT (id) DO NOTHING`)
	mustExec(`INSERT INTO shards (id, owner_id, name, persistence, visibility, scope_tag, system_prompt)
	          VALUES ('vis-shard', 'iso-owner', 'Vis', 'persistent', 'promoted', 'shard:vis', 'p')
	          ON CONFLICT (id) DO NOTHING`)
	t.Cleanup(func() {
		_, _ = pool.ExecContext(context.Background(), `DELETE FROM shards WHERE id IN ('iso-shard','vis-shard')`)
		_, _ = pool.ExecContext(context.Background(), `DELETE FROM users WHERE id = 'iso-owner'`)
	})

	// Three edges off "operator": global, isolated-shard, visible-shard.
	if err := s.UpsertRelationships(ctx, []Relationship{
		{Subject: "operator", Predicate: "owns", Object: "homelab", UserID: "iso-owner"},
		{Subject: "operator", Predicate: "secretly_owns", Object: "vault", UserID: "iso-owner", ScopeTag: "shard:iso"},
		{Subject: "operator", Predicate: "publicly_owns", Object: "blog", UserID: "iso-owner", ScopeTag: "shard:vis"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	assertVisible := func(label string, got []Relationship, gotErr error) {
		t.Helper()
		if gotErr != nil {
			t.Fatalf("%s: %v", label, gotErr)
		}
		keys := map[string]bool{}
		for _, r := range got {
			keys[r.Predicate] = true
		}
		if keys["secretly_owns"] {
			t.Errorf("%s leaked an isolated-shard triple: %+v", label, got)
		}
		if !keys["owns"] {
			t.Errorf("%s dropped the global triple: %+v", label, got)
		}
		if !keys["publicly_owns"] {
			t.Errorf("%s dropped the visible-shard triple: %+v", label, got)
		}
	}

	rc, rcErr := s.RelatedForContents(ctx, []string{"operator is here"}, "iso-owner", 20)
	assertVisible("RelatedForContents", rc, rcErr)

	tv, tvErr := s.TraverseFrom(ctx, "operator", "iso-owner", 1, 20)
	assertVisible("TraverseFrom", tv, tvErr)
}

// A shard restating one of the owner's triples must not take it over:
// before the guard the upsert rewrote the object and stamped the shard's
// isolated tag on it, hiding the owner's triple from top-level
// retrieval. A trusted-path write may still take over a shard triple.
func TestUpsertRelationships_ShardCannotClobberTopLevelTriple(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO users (id, display_name, status) VALUES ('clob-owner', 'C', 'approved') ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO shards (id, owner_id, name, persistence, visibility, scope_tag, system_prompt)
		 VALUES ('clob-shard', 'clob-owner', 'C', 'persistent', 'isolated', 'shard:clob', 'p') ON CONFLICT (id) DO NOTHING`,
	} {
		if _, err := pool.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.ExecContext(context.Background(), `DELETE FROM shards WHERE id = 'clob-shard'`)
		_, _ = pool.ExecContext(context.Background(), `DELETE FROM users WHERE id = 'clob-owner'`)
	})

	upsert := func(object, scope string) {
		t.Helper()
		if err := s.UpsertRelationships(ctx, []Relationship{
			{Subject: "appointment", Predicate: "scheduled_on", Object: object, UserID: "clob-owner", ScopeTag: scope},
		}); err != nil {
			t.Fatalf("upsert %s: %v", object, err)
		}
	}
	visibleObject := func() string {
		t.Helper()
		got, err := s.RelatedForContents(ctx, []string{"the appointment"}, "clob-owner", 10)
		if err != nil {
			t.Fatalf("RelatedForContents: %v", err)
		}
		for _, r := range got {
			if r.Predicate == "scheduled_on" {
				return r.Object
			}
		}
		return ""
	}

	upsert("tuesday", "")
	upsert("thursday", "shard:clob")
	if got := visibleObject(); got != "tuesday" {
		t.Fatalf("after an isolated shard restated it, the owner sees %q, want tuesday", got)
	}
	upsert("friday", "")
	if got := visibleObject(); got != "friday" {
		t.Fatalf("a trusted-path update was dropped: owner sees %q, want friday", got)
	}
}
