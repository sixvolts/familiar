package memory

import (
	"context"
	"testing"
)

// Phase C curation: supersede-chain walk + collapse, store health.
// DB-gated via setupMemoryStore's dedicated memory_test schema.

func setSupersedes(t *testing.T, s *PgVectorStore, id, target string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE memories SET supersedes = $2::uuid WHERE id = $1::uuid`, id, target); err != nil {
		t.Fatalf("set supersedes: %v", err)
	}
}

func TestChainForMemory_WalksBothDirections(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()

	a := insertMemory(t, s, "v1: drew drives a truck", "user", axisVec(1))
	b := insertMemory(t, s, "v2: drew drives a rivian", "user", axisVec(2))
	c := insertMemory(t, s, "v3: drew drives a rivian r1t", "user", axisVec(3))
	loner := insertMemory(t, s, "unrelated", "user", axisVec(4))
	setSupersedes(t, s, b, a)
	setSupersedes(t, s, c, b)

	// Walk from the middle: the chain must contain all three, with
	// the live tip last.
	chain, err := s.ChainForMemory(ctx, b)
	if err != nil {
		t.Fatalf("ChainForMemory: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("chain length = %d, want 3", len(chain))
	}
	if chain[0].ID != a || chain[2].ID != c {
		t.Errorf("chain order wrong: got [%s %s %s], want [a b c]", chain[0].ID, chain[1].ID, chain[2].ID)
	}
	if chain[2].Superseded {
		t.Error("tip reads as superseded")
	}
	for _, row := range chain {
		if row.ID == loner {
			t.Error("unrelated row leaked into the chain")
		}
	}
}

func TestCollapseChain_KeepsTipDeletesRest(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()

	a := insertMemory(t, s, "v1", "user", axisVec(1))
	b := insertMemory(t, s, "v2", "user", axisVec(2))
	c := insertMemory(t, s, "v3", "user", axisVec(3))
	setSupersedes(t, s, b, a)
	setSupersedes(t, s, c, b)

	deleted, tip, err := s.CollapseChain(ctx, a)
	if err != nil {
		t.Fatalf("CollapseChain: %v", err)
	}
	if deleted != 2 || tip != c {
		t.Errorf("collapse = (%d, %s), want (2, %s)", deleted, tip, c)
	}
	for _, gone := range []string{a, b} {
		if row, _ := s.GetMemory(ctx, gone); row != nil {
			t.Errorf("superseded row %s survived the collapse", gone)
		}
	}
	row, err := s.GetMemory(ctx, c)
	if err != nil || row == nil {
		t.Fatalf("tip is gone: %v", err)
	}
	if row.Supersedes != "" {
		t.Errorf("tip still carries a dangling supersedes pointer: %q", row.Supersedes)
	}

	// Collapsing a single live row is a no-op, not an error.
	deleted, tip, err = s.CollapseChain(ctx, c)
	if err != nil || deleted != 0 || tip != c {
		t.Errorf("no-op collapse = (%d, %s, %v), want (0, %s, nil)", deleted, tip, err, c)
	}
}

// Deleting a SUPERSEDED row (one another row points at via the
// self-referential supersedes FK) must succeed by detaching the child
// first, not raise 23503. This is the exact cleanup an operator
// attempts on a stale/hidden fact.
func TestDeleteMemory_SupersededRowDetachesChild(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()

	old := insertMemory(t, s, "v1 old", "user", axisVec(1))
	newer := insertMemory(t, s, "v2 new", "user", axisVec(2))
	setSupersedes(t, s, newer, old) // newer points at old → old is superseded

	// Unscoped admin delete of the pointed-at row.
	if err := s.DeleteMemory(ctx, old); err != nil {
		t.Fatalf("DeleteMemory on a superseded row: %v", err)
	}
	if row, _ := s.GetMemory(ctx, old); row != nil {
		t.Error("superseded row survived delete")
	}
	// The child survives with its pointer cleared (no dangling FK).
	child, err := s.GetMemory(ctx, newer)
	if err != nil || child == nil {
		t.Fatalf("child row gone: %v", err)
	}
	if child.Supersedes != "" {
		t.Errorf("child still points at the deleted row: supersedes=%q", child.Supersedes)
	}

	// Owner-scoped path handles the same case (and refuses cross-user).
	o2 := insertMemory(t, s, "o2 old", "user", axisVec(3))
	n2 := insertMemory(t, s, "o2 new", "user", axisVec(4))
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET user_id = 'owner-x' WHERE id IN ($1::uuid, $2::uuid)`, o2, n2); err != nil {
		t.Fatal(err)
	}
	setSupersedes(t, s, n2, o2)
	// Wrong owner → no-op, no error, row survives.
	if ok, err := s.DeleteMemoryOwned(ctx, o2, "someone-else"); err != nil || ok {
		t.Errorf("cross-user owned delete: ok=%v err=%v, want false/nil", ok, err)
	}
	if row, _ := s.GetMemory(ctx, o2); row == nil {
		t.Error("cross-user delete removed the row")
	}
	// Right owner → detaches child and deletes.
	if ok, err := s.DeleteMemoryOwned(ctx, o2, "owner-x"); err != nil || !ok {
		t.Fatalf("owned delete of superseded row: ok=%v err=%v", ok, err)
	}
	if child, _ := s.GetMemory(ctx, n2); child == nil || child.Supersedes != "" {
		t.Error("owned delete didn't detach the child")
	}
}

func TestMemoryHealth_CountsPerUser(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Two chunks (one 10 days old), one fact without an embedding,
	// one superseded pair — all for user hu; a foreign user's
	// no-embedding fact must not count.
	mustExec(`INSERT INTO memories (agent_id, scope, content, source_type, user_id, created_at)
	          VALUES ('test', 'session', 'chunk old', 'conversation', 'hu', NOW() - interval '10 days'),
	                 ('test', 'session', 'chunk new', 'conversation', 'hu', NOW()),
	                 ('test', 'user', 'no vector', 'explicit', 'hu', NOW()),
	                 ('test', 'user', 'foreign no vector', 'explicit', 'someone', NOW())`)
	old := insertMemory(t, s, "old fact", "user", axisVec(5))
	tip := insertMemory(t, s, "new fact", "user", axisVec(6))
	mustExec(`UPDATE memories SET user_id = 'hu' WHERE id IN ($1::uuid, $2::uuid)`, old, tip)
	setSupersedes(t, s, tip, old)

	h, err := s.MemoryHealth(ctx, "hu")
	if err != nil {
		t.Fatalf("MemoryHealth: %v", err)
	}
	if h.Chunks != 2 {
		t.Errorf("Chunks = %d, want 2", h.Chunks)
	}
	if h.OldestChunkDays < 9 || h.OldestChunkDays > 11 {
		t.Errorf("OldestChunkDays = %d, want ~10", h.OldestChunkDays)
	}
	if h.MissingEmbeddings != 1 {
		t.Errorf("MissingEmbeddings = %d, want 1 (foreign row must not count)", h.MissingEmbeddings)
	}
	if h.SupersededRows != 1 {
		t.Errorf("SupersededRows = %d, want 1", h.SupersededRows)
	}
}
