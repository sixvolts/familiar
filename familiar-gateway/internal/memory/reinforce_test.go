package memory

import (
	"context"
	"testing"
	"time"
)

// ReinforceFacts must bump access_count and advance last_accessed for exactly
// the ids given, leave every other row untouched, and be a safe no-op on an
// empty list. This row is the DUPLICATE survivor's recency/frequency signal,
// so a wrong or missed update silently distorts anything that later ages or
// ranks facts.
func TestReinforceFacts(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()

	insert := func(content string) string {
		t.Helper()
		var id string
		if err := s.db.QueryRowContext(ctx,
			`INSERT INTO memories (agent_id, scope, content, source_type, user_id, access_count, last_accessed)
			 VALUES ('test','user',$1,'explicit','u1', 0, NOW() - INTERVAL '1 day')
			 RETURNING id::text`, content).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		return id
	}
	read := func(id string) (int64, time.Time) {
		t.Helper()
		var n int64
		var la time.Time
		if err := s.db.QueryRowContext(ctx,
			`SELECT access_count, last_accessed FROM memories WHERE id = $1::uuid`, id).Scan(&n, &la); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return n, la
	}

	a := insert("fact a")
	b := insert("fact b")
	c := insert("fact c") // control: never reinforced

	// Empty list is a no-op, not an error, and touches nothing.
	if err := s.ReinforceFacts(ctx, nil); err != nil {
		t.Fatalf("empty ReinforceFacts: %v", err)
	}

	// Reinforce a and b together (a multi-id ANY($1::uuid[]) update).
	if err := s.ReinforceFacts(ctx, []string{a, b}); err != nil {
		t.Fatalf("ReinforceFacts: %v", err)
	}
	for _, id := range []string{a, b} {
		n, la := read(id)
		if n != 1 {
			t.Errorf("%s access_count = %d, want 1", id, n)
		}
		if time.Since(la) > time.Minute {
			t.Errorf("%s last_accessed not advanced: %v ago", id, time.Since(la))
		}
	}

	// The un-named row is untouched — still 0 and still ~1 day stale.
	if n, la := read(c); n != 0 || time.Since(la) < 23*time.Hour {
		t.Errorf("control row mutated: access_count=%d, last_accessed=%v ago (want 0 and ~24h)", n, time.Since(la))
	}

	// Not idempotent: every restatement counts, so a second reinforce
	// increments again.
	if err := s.ReinforceFacts(ctx, []string{a}); err != nil {
		t.Fatalf("second ReinforceFacts: %v", err)
	}
	if n, _ := read(a); n != 2 {
		t.Errorf("after second reinforce, access_count = %d, want 2", n)
	}
}
