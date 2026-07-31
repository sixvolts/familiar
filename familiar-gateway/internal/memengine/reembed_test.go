package memengine

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/familiar/gateway/proto/engine"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testutil"
)

// A nil / unwired sweeper must be inert rather than panicking — the
// no-embedder deployment path.
func TestReembedSweeperNoOpWhenUnwired(t *testing.T) {
	var nilSweeper *ReembedSweeper
	if got := nilSweeper.RunOnce(context.Background()); got != 0 {
		t.Errorf("nil sweeper RunOnce = %d, want 0", got)
	}
	if n, err := nilSweeper.PendingCount(context.Background()); n != 0 || err != nil {
		t.Errorf("nil sweeper PendingCount = (%d, %v), want (0, nil)", n, err)
	}
	nilSweeper.Stop() // must not panic
	if got := nilSweeper.Stats(); !got.LastRun.IsZero() {
		t.Error("nil sweeper should report zero stats")
	}

	// Constructed but with no pool/embedder: Start closes doneCh so Stop
	// returns instead of hanging.
	s := NewReembedSweeper(nil, nil, 0, 0)
	s.Start(context.Background())
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop on an unwired sweeper should return immediately")
	}
}

func TestNewReembedSweeperDefaults(t *testing.T) {
	s := NewReembedSweeper(nil, nil, 0, 0)
	if s.interval != defaultReembedInterval {
		t.Errorf("interval = %v, want the %v default", s.interval, defaultReembedInterval)
	}
	if s.batch != defaultReembedBatch {
		t.Errorf("batch = %d, want the %d default", s.batch, defaultReembedBatch)
	}
	// Explicit values win.
	s2 := NewReembedSweeper(nil, nil, 30*time.Second, 5)
	if s2.interval != 30*time.Second || s2.batch != 5 {
		t.Errorf("explicit tuning not honored: interval=%v batch=%d", s2.interval, s2.batch)
	}
}

// --- DB-gated: the whole enqueue → sweep → searchable lifecycle. ---

// commitFact stores one fact through the real engine path.
func commitFact(t *testing.T, e *MemEngine, content string, vec []float32) {
	t.Helper()
	now := time.Now()
	resp, err := e.CommitFacts(context.Background(), "sess-reembed", []*pb.FactProto{{
		Content:      content,
		Embedding:    vec,
		UserId:       "user-reembed",
		Scope:        "user",
		SourceType:   "test",
		CreatedAt:    timestamppb.New(now),
		LastAccessed: timestamppb.New(now),
	}})
	if err != nil {
		t.Fatalf("CommitFacts: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("CommitFacts reported: %s", resp.Error)
	}
	if resp.Committed != 1 {
		t.Fatalf("committed %d facts, want 1", resp.Committed)
	}
}

// setupReembedTest builds an engine over a private schema so these tests
// never touch tables another package's suite is using. Same isolation
// idiom as internal/db.TestMigrateFreshDatabase: create a scratch schema,
// pin search_path at it (public stays on the path so the `vector` type
// resolves), migrate into it, drop it on cleanup.
func setupReembedTest(t *testing.T) *MemEngine {
	t.Helper()
	dsn := os.Getenv(testutil.EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s not set", testutil.EnvDSN)
	}
	ctx := context.Background()

	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open (admin): %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	// Unique per test so parallel packages can't collide on the schema.
	schema := "reembed_test_" + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToLower(t.Name()))

	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Fatalf("drop stale schema: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scoped := dsn + sep + "options=" + url.QueryEscape("-csearch_path="+schema+",public")
	pool, err := db.Open(scoped)
	if err != nil {
		t.Fatalf("db.Open (scoped): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	migCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.Migrate(migCtx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	e := New(nil, nil, nil, "test-agent")
	e.SetDeps(pool, nil, nil)
	return e
}

// A fact committed with no vector (embedder down) must still land AND be
// queued, so the outage degrades retrieval temporarily rather than
// permanently.
func TestCommitWithoutEmbeddingEnqueues(t *testing.T) {
	e := setupReembedTest(t)
	commitFact(t, e, "the embedder was down for this one", nil)

	var n int
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pending_embeds`).Scan(&n); err != nil {
		t.Fatalf("counting queue: %v", err)
	}
	if n != 1 {
		t.Fatalf("pending_embeds has %d rows, want 1", n)
	}
}

// A fact committed WITH a vector must not be queued.
func TestCommitWithEmbeddingDoesNotEnqueue(t *testing.T) {
	e := setupReembedTest(t)
	commitFact(t, e, "this one embedded fine", []float32{0.1, 0.2, 0.3})

	var n int
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pending_embeds`).Scan(&n); err != nil {
		t.Fatalf("counting queue: %v", err)
	}
	if n != 0 {
		t.Fatalf("pending_embeds has %d rows, want 0", n)
	}
}

// A conversation-source fact is never retrieved by vector, so its NULL
// embedding is expected, not a gap — it must NOT be queued for reembed
// (which would make the sweeper embed rows nothing reads). The row still
// commits so the admin chunk browser can surface it.
func TestCommitConversationFactDoesNotEnqueue(t *testing.T) {
	e := setupReembedTest(t)
	now := time.Now()
	resp, err := e.CommitFacts(context.Background(), "sess-reembed", []*pb.FactProto{{
		Content:      "user: hi\nassistant: hello",
		Embedding:    nil, // conversation facts are intentionally unembedded
		UserId:       "user-reembed",
		Scope:        "session",
		SourceType:   "conversation",
		CreatedAt:    timestamppb.New(now),
		LastAccessed: timestamppb.New(now),
	}})
	if err != nil {
		t.Fatalf("CommitFacts: %v", err)
	}
	if resp.Committed != 1 {
		t.Fatalf("committed %d facts, want 1", resp.Committed)
	}

	var queued int
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pending_embeds`).Scan(&queued); err != nil {
		t.Fatalf("counting queue: %v", err)
	}
	if queued != 0 {
		t.Fatalf("conversation fact was enqueued for reembed: pending_embeds has %d rows, want 0", queued)
	}

	// The row itself must still be stored (nothing about skipping the
	// embed should drop the write the admin browser depends on).
	var stored int
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM memories WHERE source_type = 'conversation'`).Scan(&stored); err != nil {
		t.Fatalf("counting memories: %v", err)
	}
	if stored != 1 {
		t.Fatalf("conversation fact not stored: %d rows, want 1", stored)
	}
}

// The sweep back-fills the vector and clears the queue row once an
// embedder is reachable.
func TestSweepBackfillsAndClearsQueue(t *testing.T) {
	e := setupReembedTest(t)
	commitFact(t, e, "back-fill me", nil)

	embed := func(ctx context.Context, text string) ([]float32, error) {
		return []float32{0.5, 0.5, 0.5}, nil
	}
	s := NewReembedSweeper(e.pool, embed, time.Minute, 10)

	if got := s.RunOnce(context.Background()); got != 1 {
		t.Fatalf("sweep fixed %d rows, want 1", got)
	}
	var missing, queued int
	ctx := context.Background()
	if err := e.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE embedding IS NULL`).Scan(&missing); err != nil {
		t.Fatalf("counting null embeddings: %v", err)
	}
	if missing != 0 {
		t.Errorf("%d memories still lack a vector after the sweep, want 0", missing)
	}
	if err := e.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeds`).Scan(&queued); err != nil {
		t.Fatalf("counting queue: %v", err)
	}
	if queued != 0 {
		t.Errorf("queue still has %d rows after a successful sweep, want 0", queued)
	}
	if st := s.Stats(); st.LastFixed != 1 || st.LastRun.IsZero() {
		t.Errorf("stats not recorded: %+v", st)
	}
}

// While the embedder is still down the sweep must leave the queue intact
// so the work isn't lost, and record the attempt.
func TestSweepLeavesQueueWhenEmbedderDown(t *testing.T) {
	e := setupReembedTest(t)
	commitFact(t, e, "still waiting", nil)

	down := func(ctx context.Context, text string) ([]float32, error) {
		return nil, errors.New("connection refused")
	}
	s := NewReembedSweeper(e.pool, down, time.Minute, 10)
	if got := s.RunOnce(context.Background()); got != 0 {
		t.Fatalf("sweep fixed %d rows with a dead embedder, want 0", got)
	}

	ctx := context.Background()
	var attempts int
	var lastErr *string
	if err := e.pool.QueryRowContext(ctx,
		`SELECT attempts, last_error FROM pending_embeds`).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("reading queue row: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastErr == nil || *lastErr == "" {
		t.Error("expected last_error to be recorded")
	}

	// Recovery: the same row succeeds on the next pass.
	up := func(ctx context.Context, text string) ([]float32, error) {
		return []float32{1, 0, 0}, nil
	}
	s2 := NewReembedSweeper(e.pool, up, time.Minute, 10)
	if got := s2.RunOnce(ctx); got != 1 {
		t.Fatalf("sweep after recovery fixed %d rows, want 1", got)
	}
}

// A queue entry whose memory got a vector by some other path (an admin
// edit) is pruned, so the reported depth stays honest.
func TestSweepPrunesSatisfiedRows(t *testing.T) {
	e := setupReembedTest(t)
	commitFact(t, e, "fixed elsewhere", nil)

	ctx := context.Background()
	if _, err := e.pool.ExecContext(ctx,
		`UPDATE memories SET embedding = $1`, vectorParam([]float32{0.9, 0.1})); err != nil {
		t.Fatalf("simulating an external re-embed: %v", err)
	}

	s := NewReembedSweeper(e.pool, func(ctx context.Context, string2 string) ([]float32, error) {
		t.Error("embedder should not be called for an already-embedded row")
		return nil, nil
	}, time.Minute, 10)
	s.RunOnce(ctx)

	n, err := s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if n != 0 {
		t.Errorf("queue depth = %d after pruning satisfied rows, want 0", n)
	}
}

// Deleting a memory must drop its queue row (FK CASCADE), so the queue
// can't accumulate references to gone rows.
func TestQueueCascadesOnMemoryDelete(t *testing.T) {
	e := setupReembedTest(t)
	commitFact(t, e, "delete me", nil)

	ctx := context.Background()
	if _, err := e.pool.ExecContext(ctx, `DELETE FROM memories`); err != nil {
		t.Fatalf("deleting memories: %v", err)
	}
	var n int
	if err := e.pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_embeds`).Scan(&n); err != nil {
		t.Fatalf("counting queue: %v", err)
	}
	if n != 0 {
		t.Errorf("queue has %d orphaned rows after the memory was deleted, want 0", n)
	}
}

// The batch cap bounds one pass; the next tick picks up the remainder.
func TestSweepRespectsBatchCap(t *testing.T) {
	e := setupReembedTest(t)
	for _, c := range []string{"one", "two", "three"} {
		commitFact(t, e, c, nil)
	}
	embed := func(ctx context.Context, text string) ([]float32, error) {
		return []float32{0.2, 0.2}, nil
	}
	s := NewReembedSweeper(e.pool, embed, time.Minute, 2) // batch of 2
	if got := s.RunOnce(context.Background()); got != 2 {
		t.Fatalf("first pass fixed %d, want the 2-row batch cap", got)
	}
	if got := s.RunOnce(context.Background()); got != 1 {
		t.Fatalf("second pass fixed %d, want the remaining 1", got)
	}
}
