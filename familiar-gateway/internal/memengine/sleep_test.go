package memengine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testdsn"
)

// SQL-touching paths (the maintenance queries) are integration
// concerns covered by the pool-backed tests below the scaffolding
// tests. The scaffolding tests cover: nil-pool no-op, disabled
// no-op, stats accumulator, double-Stop safety.

func TestSleepCycle_NoPoolNoOp(t *testing.T) {
	// nil pool → Start returns immediately, doneCh closes.
	s := NewSleepCycle(nil, "test", config.DefaultSleepConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	select {
	case <-s.doneCh:
		// expected — no-op path closes doneCh on Start
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Start with nil pool should close doneCh immediately")
	}
	// Stop must be idempotent + not panic.
	s.Stop()
	s.Stop()
}

// The disabled-config branch in Start is structurally identical to
// the nil-pool branch (both log + close doneCh + return), so the
// nil-pool test above exercises the same close-and-exit path.
// Testing the disabled branch independently would require a
// non-nil *db.Pool stub which isn't worth the indirection; an
// integration test with a real pool can cover both branches.

func TestSleepCycle_LastStatsEmpty(t *testing.T) {
	// Before any cycle runs, LastStats returns its zero value.
	s := NewSleepCycle(nil, "test", config.DefaultSleepConfig())
	st := s.LastStats()
	if !st.StartedAt.IsZero() {
		t.Errorf("expected zero StartedAt, got %v", st.StartedAt)
	}
	if s.CycleCount() != 0 {
		t.Errorf("CycleCount = %d, want 0", s.CycleCount())
	}
}

func TestSleepCycle_NilSafe(t *testing.T) {
	// All exported methods on a nil receiver should no-op rather
	// than panic — main.go guards on inProcMem != nil but the
	// per-method check is a belt-and-suspenders gate against an
	// accidentally unwired path.
	var s *SleepCycle
	s.Start(context.Background())
	s.Stop()
	if s.CycleCount() != 0 {
		t.Errorf("nil CycleCount = %d", s.CycleCount())
	}
	if !s.LastStats().StartedAt.IsZero() {
		t.Errorf("nil LastStats should be zero value")
	}
}

// ── DB-gated integration tests (FAMILIAR_TEST_DSN) ───────────────
//
// Same dedicated-schema pattern as internal/actions: `go test ./...`
// runs packages in parallel against one database, so sharing public
// would let another package's truncates eat these rows mid-test.

func sleepPoolForTest(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("FAMILIAR_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping: FAMILIAR_TEST_DSN not set")
	}
	ctx := context.Background()
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open (admin): %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS sleep_test`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	pool, err := db.Open(testdsn.Scoped(t, dsn, "sleep_test"))
	if err != nil {
		t.Fatalf("db.Open (scoped): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

func seedMemory(t *testing.T, pool *db.Pool, agent, id, user, sourceType, scopeTag, emb string, age time.Duration) {
	t.Helper()
	var st any
	if scopeTag != "" {
		st = scopeTag
	}
	_, err := pool.ExecContext(context.Background(), `
		INSERT INTO memories (id, agent_id, scope, content, content_hash, embedding,
		                      source_type, user_id, scope_tag, created_at, last_accessed)
		VALUES ($1::uuid, $2, 'user', $3, $4, $5::vector, $6, $7, $8,
		        NOW() - $9::interval, NOW() - $9::interval)`,
		id, agent, "content "+id, "hash-"+id, emb, sourceType, user, st,
		fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func memoryState(t *testing.T, pool *db.Pool, id string) (supersedes string, hidden bool, exists bool) {
	t.Helper()
	var sup *string
	err := pool.QueryRowContext(context.Background(), `
		SELECT m.supersedes::text,
		       EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		  FROM memories m WHERE m.id = $1::uuid`, id).Scan(&sup, &hidden)
	if err == sql.ErrNoRows {
		return "", false, false
	}
	if err != nil {
		t.Fatalf("state %s: %v", id, err)
	}
	if sup != nil {
		supersedes = *sup
	}
	return supersedes, hidden, true
}

// testUUID builds a per-run unique UUID: the test DB outlives runs,
// so fixed ids would collide with rows seeded by earlier invocations.
func testUUID(idx int) string {
	n := time.Now().UnixNano()
	return fmt.Sprintf("%08x-%04x-4000-8000-%012x", uint32(n>>32), uint16(n>>16), (uint64(n)+uint64(idx))&0xffffffffffff)
}

// The drift dedup collapses a near-duplicate pair in the CORRECT
// direction (newer survives and points at older; older reads as
// superseded/hidden), and never crosses kind, user, or scope_tag
// boundaries.
func TestSleep_ResolveConflictsDirectionAndPartitions(t *testing.T) {
	pool := sleepPoolForTest(t)
	agent := fmt.Sprintf("sleep-agent-%d", time.Now().UnixNano())
	emb := "[1,0,0]"                                             // identical embeddings → cosine similarity 1.0
	uuidA, uuidB, uuidC := testUUID(1), testUUID(2), testUUID(3) // older / newer dup / chunk
	uuidD, uuidE := testUUID(4), testUUID(5)                     // other user / shard scope

	seedMemory(t, pool, agent, uuidA, "u1", "conversation_extraction", "", emb, 2*time.Hour)
	seedMemory(t, pool, agent, uuidB, "u1", "conversation_extraction", "", emb, 1*time.Hour)
	seedMemory(t, pool, agent, uuidC, "u1", "conversation", "", emb, 1*time.Hour)
	seedMemory(t, pool, agent, uuidD, "u2", "conversation_extraction", "", emb, 1*time.Hour)
	seedMemory(t, pool, agent, uuidE, "u1", "conversation_extraction", "shard:x", emb, 1*time.Hour)

	s := NewSleepCycle(pool, agent, config.DefaultSleepConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	n, err := s.resolveConflicts(ctx, 0.92)
	if err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	if n != 1 {
		t.Errorf("resolved %d pairs, want exactly 1 (A↔B)", n)
	}

	// Direction: B (newer) points at A (older); A hidden, B live.
	supB, hiddenB, _ := memoryState(t, pool, uuidB)
	_, hiddenA, _ := memoryState(t, pool, uuidA)
	if supB != uuidA {
		t.Errorf("B.supersedes = %q, want A (%s)", supB, uuidA)
	}
	if !hiddenA {
		t.Error("A (older) should read as superseded/hidden")
	}
	if hiddenB {
		t.Error("B (newer) must stay live — the inverted-direction bug")
	}

	// Partitions: chunk, other-user, and shard rows untouched.
	for _, id := range []string{uuidC, uuidD, uuidE} {
		sup, hidden, _ := memoryState(t, pool, id)
		if sup != "" || hidden {
			t.Errorf("row %s crossed a dedup boundary (supersedes=%q hidden=%v)", id, sup, hidden)
		}
	}
}

// Retention: session rows idle past the archive window are deleted;
// recent ones survive.
func TestSleep_PruneOldSession(t *testing.T) {
	pool := sleepPoolForTest(t)
	agent := fmt.Sprintf("sleep-prune-%d", time.Now().UnixNano())
	oldID, newID := testUUID(11), testUUID(12)
	seedMemory(t, pool, agent, oldID, "u1", "conversation", "", "[0,1,0]", 100*24*time.Hour)
	seedMemory(t, pool, agent, newID, "u1", "conversation", "", "[0,0,1]", 1*time.Hour)
	if _, err := pool.ExecContext(context.Background(),
		`UPDATE memories SET scope = 'session' WHERE agent_id = $1`, agent); err != nil {
		t.Fatal(err)
	}

	s := NewSleepCycle(pool, agent, config.DefaultSleepConfig())
	n, err := s.pruneOldSession(context.Background(), 90)
	if err != nil {
		t.Fatalf("pruneOldSession: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, _, exists := memoryState(t, pool, oldID); exists {
		t.Error("stale session row survived the prune")
	}
	if _, _, exists := memoryState(t, pool, newID); !exists {
		t.Error("recent session row was pruned")
	}
}

// A session row that supersedes another is kept: pruning it would bring
// back the older fact it hid.
func TestSleep_PruneKeepsChainHeads(t *testing.T) {
	pool := sleepPoolForTest(t)
	agent := fmt.Sprintf("sleep-prune-head-%d", time.Now().UnixNano())
	older, head := testUUID(13), testUUID(14)
	seedMemory(t, pool, agent, older, "u1", "conversation_extraction", "", "[1,0,0]", 200*24*time.Hour)
	seedMemory(t, pool, agent, head, "u1", "conversation", "", "[0,1,0]", 100*24*time.Hour)
	if _, err := pool.ExecContext(context.Background(),
		`UPDATE memories SET scope = 'session', supersedes = $2::uuid WHERE id = $1::uuid`, head, older); err != nil {
		t.Fatal(err)
	}
	s := NewSleepCycle(pool, agent, config.DefaultSleepConfig())
	if _, err := s.pruneOldSession(context.Background(), 90); err != nil {
		t.Fatalf("pruneOldSession: %v", err)
	}
	if _, _, exists := memoryState(t, pool, head); !exists {
		t.Error("pruned a session row that was hiding an older fact")
	}
	if _, hidden, _ := memoryState(t, pool, older); !hidden {
		t.Error("the older fact came back")
	}
}

// The repair migration clears INVERTED pointers (pointer older than
// its target — only the buggy dedup ever wrote those) and leaves
// legitimate newer→older pointers alone.
func TestSleep_RepairInvertedSupersedes(t *testing.T) {
	pool := sleepPoolForTest(t)
	agent := fmt.Sprintf("sleep-repair-%d", time.Now().UnixNano())
	oldP, newX := testUUID(21), testUUID(22)         // inverted: older points at newer
	legitOld, legitNew := testUUID(23), testUUID(24) // legit: newer points at older
	seedMemory(t, pool, agent, oldP, "u1", "conversation_extraction", "", "[1,0,0]", 3*time.Hour)
	seedMemory(t, pool, agent, newX, "u1", "conversation_extraction", "", "[0,1,0]", 1*time.Hour)
	seedMemory(t, pool, agent, legitOld, "u1", "conversation_extraction", "", "[0,0,1]", 3*time.Hour)
	seedMemory(t, pool, agent, legitNew, "u1", "conversation_extraction", "", "[1,1,0]", 1*time.Hour)
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`UPDATE memories SET supersedes = $2::uuid WHERE id = $1::uuid`, oldP, newX)
	mustExec(`UPDATE memories SET supersedes = $2::uuid WHERE id = $1::uuid`, legitNew, legitOld)

	// Re-run migrations as a database upgrading into the repair: it is a
	// gated one-shot, so clear the marker the setup's Migrate left.
	mustExec(`DELETE FROM applied_data_fixes WHERE name = 'repair_inverted_supersedes'`)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	supP, _, _ := memoryState(t, pool, oldP)
	if supP != "" {
		t.Errorf("inverted pointer survived repair: %q", supP)
	}
	_, hiddenX, _ := memoryState(t, pool, newX)
	if hiddenX {
		t.Error("newer row still hidden after repair")
	}
	supLegit, _, _ := memoryState(t, pool, legitNew)
	if supLegit != legitOld {
		t.Errorf("legitimate pointer was clobbered: %q", supLegit)
	}
}

// seedVec is seedMemory with a caller-chosen content hash and vector.
func seedVec(t *testing.T, pool *db.Pool, agent, id, hash, emb string, age time.Duration) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO memories (id, agent_id, scope, content, content_hash, embedding,
		                      source_type, user_id, created_at, last_accessed)
		VALUES ($1::uuid, $2, 'user', $3, $4, $5::vector, 'conversation_extraction', 'u1',
		        NOW() - $6::interval, NOW() - $6::interval)`,
		id, agent, "content "+id, hash, emb, fmt.Sprintf("%d seconds", int(age.Seconds()))); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// A fact that replaced an older one can itself be deduplicated by a
// newer near-duplicate ("NYC" replaced an older fact; "New York City"
// arrives). Heads were excluded from dedup for good.
func TestSleep_DedupReachesChainHeads(t *testing.T) {
	pool := sleepPoolForTest(t)
	agent := fmt.Sprintf("sleep-heads-%d", time.Now().UnixNano())
	a, b, c := testUUID(11), testUUID(12), testUUID(13)
	seedVec(t, pool, agent, a, "ha", "[0,1,0]", 3*time.Hour)
	seedVec(t, pool, agent, b, "hb", "[1,0,0]", 2*time.Hour)
	seedVec(t, pool, agent, c, "hc", "[1,0,0]", 1*time.Hour)
	if _, err := pool.ExecContext(context.Background(), `UPDATE memories SET supersedes = $1::uuid WHERE id = $2::uuid`, a, b); err != nil {
		t.Fatal(err)
	}
	s := NewSleepCycle(pool, agent, config.DefaultSleepConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.resolveConflicts(ctx, 0.92); err != nil {
		t.Fatal(err)
	}
	if sup, _, _ := memoryState(t, pool, c); sup != b {
		t.Errorf("the newer duplicate supersedes %q, want the chain head %s", sup, b)
	}
}

// Two newer near-duplicates of one fact: one of them takes it, not both
// (which branched the chain).
func TestSleep_DedupDoesNotBranch(t *testing.T) {
	pool := sleepPoolForTest(t)
	agent := fmt.Sprintf("sleep-branch-%d", time.Now().UnixNano())
	a, w1, w2 := testUUID(21), testUUID(22), testUUID(23)
	// w1 and w2 are each close to a (cos 0.97) but not to each other (0.88).
	seedVec(t, pool, agent, a, "ha", "[1,0,0]", 3*time.Hour)
	seedVec(t, pool, agent, w1, "h1", "[0.97,0.2431,0]", 2*time.Hour)
	seedVec(t, pool, agent, w2, "h2", "[0.97,-0.2431,0]", 1*time.Hour)
	s := NewSleepCycle(pool, agent, config.DefaultSleepConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.resolveConflicts(ctx, 0.92); err != nil {
		t.Fatal(err)
	}
	s1, _, _ := memoryState(t, pool, w1)
	s2, _, _ := memoryState(t, pool, w2)
	if (s1 == a) == (s2 == a) {
		t.Errorf("w1.supersedes=%q w2.supersedes=%q: want exactly one pointing at %s", s1, s2, a)
	}
}

// The dedup works through the changed rows a batch at a time, keeps its
// place, and picks up later rows on the next pass; a row with another
// embedding dimension in the partition doesn't fail the pass.
func TestSleep_DedupBatchesAndResumes(t *testing.T) {
	pool := sleepPoolForTest(t)
	agent := fmt.Sprintf("sleep-batches-%d", time.Now().UnixNano())
	old := dedupBatch
	dedupBatch = 2
	t.Cleanup(func() { dedupBatch = old })
	for i := 0; i < 5; i++ {
		v := make([]string, 6)
		for j := range v {
			v[j] = "0"
		}
		v[i] = "1"
		seedVec(t, pool, agent, testUUID(30+i), fmt.Sprint("hx", i), "["+strings.Join(v, ",")+"]", time.Duration(10-i)*time.Hour)
	}
	seedVec(t, pool, agent, testUUID(40), "hdim", "[1,0,0]", 90*time.Minute) // other dimension
	dupOld, dupNew := testUUID(41), testUUID(42)
	seedVec(t, pool, agent, dupOld, "hd1", "[0,0,0,0,0,1]", 2*time.Hour)
	seedVec(t, pool, agent, dupNew, "hd2", "[0,0,0,0,0,1]", 1*time.Hour)
	s := NewSleepCycle(pool, agent, config.DefaultSleepConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	n, err := s.resolveConflicts(ctx, 0.92)
	if err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	if n != 1 {
		t.Errorf("resolved %d, want 1", n)
	}
	if sup, _, _ := memoryState(t, pool, dupNew); sup != dupOld {
		t.Errorf("dup not resolved across batches: %q", sup)
	}
	// A later duplicate is picked up by the next pass.
	late := testUUID(43)
	seedVec(t, pool, agent, late, "hd3", "[0,0,0,0,0,1]", 0)
	if _, err := s.resolveConflicts(ctx, 0.92); err != nil {
		t.Fatal(err)
	}
	if sup, _, _ := memoryState(t, pool, late); sup != dupNew {
		t.Errorf("the later duplicate supersedes %q, want %s", sup, dupNew)
	}
}

// An on-demand pass while one is running reports that and returns.
func TestSleep_RunOnceDoesNotOverlap(t *testing.T) {
	pool := sleepPoolForTest(t)
	s := NewSleepCycle(pool, "sleep-overlap", config.DefaultSleepConfig())
	s.runMu.Lock()
	done := make(chan CycleStats, 1)
	go func() { done <- s.RunOnce(context.Background()) }()
	select {
	case st := <-done:
		if len(st.Errors) == 0 || !strings.Contains(st.Errors[len(st.Errors)-1], "already running") {
			t.Errorf("stats = %+v, want an already-running note", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce waited for the running pass")
	}
	s.runMu.Unlock()
}

// Only a row that replaces nothing can take the older duplicate: when
// the newest candidate is a chain head (its one pointer is taken), the
// next free candidate does. Picking the head left the duplicate live.
func TestSleep_DedupWinnerMustBeFree(t *testing.T) {
	pool := sleepPoolForTest(t)
	agent := fmt.Sprintf("sleep-free-%d", time.Now().UnixNano())
	x, l, f, h := testUUID(51), testUUID(52), testUUID(53), testUUID(54)
	seedVec(t, pool, agent, x, "hx", "[0,1,0]", 4*time.Hour)
	seedVec(t, pool, agent, l, "hl", "[1,0,0]", 3*time.Hour)
	seedVec(t, pool, agent, f, "hf", "[1,0.05,0]", 2*time.Hour)
	seedVec(t, pool, agent, h, "hh", "[1,-0.02,0]", 1*time.Hour)
	if _, err := pool.ExecContext(context.Background(), `UPDATE memories SET supersedes = $1::uuid WHERE id = $2::uuid`, x, h); err != nil {
		t.Fatal(err)
	}
	s := NewSleepCycle(pool, agent, config.DefaultSleepConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.resolveConflicts(ctx, 0.92); err != nil {
		t.Fatal(err)
	}
	if sup, _, _ := memoryState(t, pool, f); sup != l {
		t.Errorf("the free newer duplicate supersedes %q, want %s", sup, l)
	}
	if sup, _, _ := memoryState(t, pool, h); sup != x {
		t.Errorf("the head's pointer moved to %q", sup)
	}
}
