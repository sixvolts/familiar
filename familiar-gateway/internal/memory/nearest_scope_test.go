package memory

import (
	"context"
	"testing"
)

// insertScoped adds a knowledge row with an explicit scope_tag.
func insertScoped(t *testing.T, s *PgVectorStore, content, scopeTag string, vec []float32) {
	t.Helper()
	var tag any
	if scopeTag != "" {
		tag = scopeTag
	}
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO memories (agent_id, scope, content, embedding, source_type, user_id, scope_tag)
		 VALUES ('test', 'user', $1, $2::vector, 'explicit', 'u1', $3)`,
		content, vectorToString(vec), tag); err != nil {
		t.Fatalf("insert %q: %v", content, err)
	}
}

// declareShard registers a shard row so the isolation predicate has
// something to match against.
func declareShard(t *testing.T, s *PgVectorStore, scopeTag, visibility string) {
	t.Helper()
	// shards.owner_id is FK-constrained to users.
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO users (id, display_name, status, role) VALUES ('u1','u1','approved','user')
		 ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if _, err := s.db.ExecContext(context.Background(),
		// Upsert: the shards table outlives an individual test in the
		// shared test schema, so re-declaring must be idempotent AND must
		// win on visibility.
		`INSERT INTO shards (id, owner_id, name, system_prompt, scope_tag, visibility, persistence)
		 VALUES ($1, 'u1', $1, 'p', $2, $3, 'persistent')
		 ON CONFLICT (id) DO UPDATE SET scope_tag = $2, visibility = $3`,
		"shard-"+scopeTag, scopeTag, visibility); err != nil {
		t.Fatalf("insert shard %s: %v", scopeTag, err)
	}
}

// The write-time conflict target must respect shard isolation, or the
// extraction path becomes a channel for reaching into a private shard's
// rows — Search and both HybridSearch arms already exclude them.
func TestNearestLiveFact_TrustedPathSkipsIsolatedShardRows(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()

	q := axisVec(1)
	insertScoped(t, s, "secret from an isolated shard", "iso-tag", q)          // exact match
	insertScoped(t, s, "ordinary top-level fact", "", mixVec(1, 2, 0.9, 0.44)) // slightly off
	declareShard(t, s, "iso-tag", "isolated")

	// Trusted path: no scope tag of its own.
	nf, ok, err := s.NearestLiveFact(ctx, q, "u1", "")
	if err != nil {
		t.Fatalf("NearestLiveFact: %v", err)
	}
	if !ok {
		t.Fatal("expected the top-level row to be found")
	}
	if nf.Content != "ordinary top-level fact" {
		t.Errorf("conflict target = %q — a trusted extraction must not reach an isolated shard's rows", nf.Content)
	}
}

// The shard's OWN write path must still see its own rows, or an isolated
// shard gets no dedup and no supersede at all. This is why the predicate
// is a disjunction rather than plain exclusion.
func TestNearestLiveFact_IsolatedShardSeesItsOwnRows(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()

	q := axisVec(1)
	insertScoped(t, s, "the shard's own earlier fact", "iso-tag", q)
	declareShard(t, s, "iso-tag", "isolated")

	nf, ok, err := s.NearestLiveFact(ctx, q, "u1", "iso-tag")
	if err != nil {
		t.Fatalf("NearestLiveFact: %v", err)
	}
	if !ok {
		t.Fatal("an isolated shard must still find its own rows, or it can never dedup or supersede")
	}
	if nf.Content != "the shard's own earlier fact" {
		t.Errorf("got %q, want the shard's own row", nf.Content)
	}
}

// A PROMOTED shard's rows are surfaced by top-level retrieval, so they
// must stay supersedable — excluding every scope_tag would wrongly
// freeze them.
func TestNearestLiveFact_PromotedShardRowsRemainReachable(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()

	q := axisVec(1)
	insertScoped(t, s, "promoted shard fact", "promo-tag", q)
	declareShard(t, s, "promo-tag", "promoted")

	nf, ok, err := s.NearestLiveFact(ctx, q, "u1", "")
	if err != nil {
		t.Fatalf("NearestLiveFact: %v", err)
	}
	if !ok || nf.Content != "promoted shard fact" {
		t.Errorf("got (%q, ok=%v) — promoted rows are visible to retrieval, so they must be supersedable", nf.Content, ok)
	}
}
