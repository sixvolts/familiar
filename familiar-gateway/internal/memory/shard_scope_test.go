package memory

// Shard scope boundaries in the memory store. A shard turn runs with its
// OWNER's user id, so these are the checks that keep one scope's rows
// out of another's reach.

import (
	"context"
	"testing"

	"github.com/familiar/gateway/internal/db"
)

// insertFor adds a knowledge row for owner with scopeTag ("" = top-level)
// and returns its id.
func insertFor(t *testing.T, s *PgVectorStore, owner, content, scopeTag string, vec []float32) string {
	t.Helper()
	var tag any
	if scopeTag != "" {
		tag = scopeTag
	}
	var id string
	if err := s.db.QueryRowContext(context.Background(),
		`INSERT INTO memories (agent_id, scope, content, embedding, source_type, user_id, scope_tag)
		 VALUES ('test', 'user', $1, $2::vector, 'explicit', $3, $4) RETURNING id::text`,
		content, vectorToString(vec), owner, tag).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", content, err)
	}
	return id
}

// declareShardFor registers a shard owned by owner.
func declareShardFor(t *testing.T, s *PgVectorStore, owner, id, scopeTag, visibility string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, display_name, status, role) VALUES ($1, $1, 'approved', 'user')
		 ON CONFLICT (id) DO NOTHING`, owner); err != nil {
		t.Fatalf("seed owner %s: %v", owner, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO shards (id, owner_id, name, system_prompt, scope_tag, visibility, persistence)
		 VALUES ($1, $2, $1, 'p', $3, $4, 'persistent')
		 ON CONFLICT (id) DO UPDATE SET owner_id = $2, scope_tag = $3, visibility = $4`,
		id, owner, scopeTag, visibility); err != nil {
		t.Fatalf("insert shard %s: %v", id, err)
	}
	t.Cleanup(func() { _, _ = s.db.ExecContext(context.Background(), `DELETE FROM shards WHERE id = $1`, id) })
}

// An isolated shard's extraction must not pick the owner's top-level
// fact as its supersede target: the replacement would be invisible to
// top-level retrieval and the original hidden by the supersede, erasing
// the owner's fact.
func TestNearestLiveFact_IsolatedShardCannotReachTopLevel(t *testing.T) {
	s := setupMemoryStore(t)
	q := axisVec(1)
	insertFor(t, s, "u1", "my dog's name is Rex", "", q)
	declareShardFor(t, s, "u1", "vet-intake", "shard:vet", "isolated")

	nf, ok, err := s.NearestLiveFact(context.Background(), q, "u1", "shard:vet")
	if err != nil {
		t.Fatalf("NearestLiveFact: %v", err)
	}
	if ok {
		t.Fatalf("isolated shard got the owner's top-level fact %q as a supersede target", nf.Content)
	}
}

// Isolation is per owner. Another user's isolated shard that happens to
// use the same tag must not hide this user's rows.
func TestSearch_OtherOwnersIsolatedShardDoesNotHideRows(t *testing.T) {
	s := setupMemoryStore(t)
	q := axisVec(2)
	declareShardFor(t, s, "u1", "u1-work", "shard:work", "promoted")
	insertFor(t, s, "u1", "u1's promoted work fact", "shard:work", q)
	declareShardFor(t, s, "u2", "u2-work", "shard:work", "isolated")

	found := func(label string, res []MemoryResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		for _, r := range res {
			if r.Content == "u1's promoted work fact" {
				return
			}
		}
		t.Errorf("%s: u2's isolated shard hid u1's promoted row (isolation matched the tag across owners)", label)
	}
	res, err := s.Search(context.Background(), q, 5, 0.5, "u1")
	found("Search", res, err)
	// HybridSearch fuses a vector arm and a full-text arm, and a row
	// found by either survives, so probe each arm alone: nonsense text
	// leaves only the vector arm, an unrelated vector only the text arm.
	res, err = s.HybridSearch(context.Background(), "xylophone", q, 5, 0.5, "u1")
	found("HybridSearch vector arm", res, err)
	res, err = s.HybridSearch(context.Background(), "promoted work fact", axisVec(9), 5, 0.99, "u1")
	found("HybridSearch text arm", res, err)

	// And for the write-time supersede target.
	if nf, ok, err := s.NearestLiveFact(context.Background(), q, "u1", ""); err != nil || !ok || nf.Content != "u1's promoted work fact" {
		t.Errorf("NearestLiveFact: u2's isolated shard hid u1's row (got %q, ok=%v, err=%v)", nf.Content, ok, err)
	}

	// The same holds for the triples graph.
	rels, err := NewPgRelationshipStore(s.db)
	if err != nil {
		t.Fatalf("relationship store: %v", err)
	}
	if err := rels.UpsertRelationships(context.Background(), []Relationship{
		{Subject: "worklaptop", Predicate: "assigned_to", Object: "u1", UserID: "u1", ScopeTag: "shard:work"},
		{Subject: "u1", Predicate: "works_at", Object: "acme", UserID: "u1"},
	}); err != nil {
		t.Fatalf("seed triple: %v", err)
	}
	related, err := rels.RelatedForContents(context.Background(), []string{"the worklaptop"}, "u1", 10)
	if err != nil || len(related) == 0 {
		t.Errorf("RelatedForContents: u2's isolated shard hid u1's triple (err=%v)", err)
	}
	// Two hops: reaching works_at requires the recursive step to cross
	// the shard:work edge.
	walked, err := rels.TraverseFrom(context.Background(), "worklaptop", "u1", 2, 10)
	if err != nil {
		t.Fatalf("TraverseFrom: %v", err)
	}
	var crossed bool
	for _, r := range walked {
		if r.Predicate == "works_at" {
			crossed = true
		}
	}
	if !crossed {
		t.Errorf("TraverseFrom: u2's isolated shard blocked the walk across u1's edge (got %+v)", walked)
	}
}

func TestSearchInScopeAndInView(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	q := axisVec(3)
	declareShardFor(t, s, "u1", "kiosk", "shard:kiosk", "isolated")
	top := insertFor(t, s, "u1", "top-level fact", "", q)
	own := insertFor(t, s, "u1", "kiosk fact", "shard:kiosk", q)

	res, err := s.SearchInScope(ctx, q, 5, 0.5, "u1", "shard:kiosk")
	if err != nil {
		t.Fatalf("SearchInScope: %v", err)
	}
	if len(res) != 1 || res[0].ID != own {
		t.Fatalf("SearchInScope = %+v, want only the kiosk's own row", res)
	}

	cases := []struct {
		name, id, scope string
		want            bool
	}{
		{"shard on its own row", own, "shard:kiosk", true},
		{"shard on the owner's top-level row", top, "shard:kiosk", false},
		{"trusted on a top-level row", top, "", true},
		{"trusted on an isolated shard's row", own, "", false},
		{"another user", top, "", false},
		{"malformed id", "not-a-uuid", "", false},
	}
	for _, c := range cases {
		user := "u1"
		if c.name == "another user" {
			user = "u2"
		}
		got, err := s.InView(ctx, c.id, user, c.scope)
		if err != nil {
			t.Errorf("%s: InView error %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: InView = %v, want %v", c.name, got, c.want)
		}
	}
}

// The owner's list_my_memories view leaves isolated shards' rows out.
func TestListMemories_TopLevelOnlyExcludesIsolatedRows(t *testing.T) {
	s := setupMemoryStore(t)
	q := axisVec(4)
	declareShardFor(t, s, "u1", "clinic", "shard:clinic", "isolated")
	insertFor(t, s, "u1", "patient intake transcript", "shard:clinic", q)
	insertFor(t, s, "u1", "owner's own fact", "", q)

	rows, err := s.ListMemories(context.Background(), MemoryFilter{
		UserIDFilterMode: UserIDFilterExact, UserID: "u1", TopLevelOnly: true,
	}, 50, 0)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	var sawOwn bool
	for _, r := range rows {
		if r.Content == "patient intake transcript" {
			t.Fatal("top-level listing includes an isolated shard's row")
		}
		if r.Content == "owner's own fact" {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Fatal("top-level listing dropped the owner's own fact")
	}
}

// Rows an isolated shard already superseded across scopes are restored
// by the repair migration.
func TestMigrate_RestoresFactsAnIsolatedShardSuperseded(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	q := axisVec(5)
	declareShardFor(t, s, "u1", "vet", "shard:vet", "isolated")
	rex := insertFor(t, s, "u1", "the dog is Rex", "", q)
	max := insertFor(t, s, "u1", "the dog is Max", "shard:vet", q)
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET supersedes = $1::uuid WHERE id = $2::uuid`, rex, max); err != nil {
		t.Fatalf("seed supersede: %v", err)
	}
	if err := db.Migrate(ctx, s.db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	res, err := s.Search(ctx, q, 5, 0.5, "u1")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.ID == rex {
			return
		}
	}
	t.Fatal("the owner's fact is still hidden by the isolated shard's supersede")
}
