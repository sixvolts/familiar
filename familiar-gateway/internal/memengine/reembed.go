package memengine

// Re-embed sweep: drains the pending_embeds queue once an embedder is
// reachable again.
//
// A fact committed while every embedder in the [roles.embedder] chain
// was offline lands in `memories` with a NULL embedding. It is still
// durable and still matchable by full-text search, but it is invisible
// to semantic retrieval — so without this sweep an embedder outage would
// permanently degrade every fact written during it. CommitFacts and
// UpdateFact enqueue those rows; this drains the queue.
//
// Deliberately independent of the [sleep] consolidation cycle: it must
// run even with sleep disabled, and on a much tighter cadence than a
// 30-minute heavy pass — a fact should become searchable shortly after
// the embedder returns, not up to half an hour later.

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/db"
)

// errEmptyVector marks an embedder that answered without erroring but
// returned no numbers — a misconfigured server, not a transport failure,
// so the row records the attempt and the pass continues.
var errEmptyVector = errors.New("embedder returned an empty vector")

// EmbedFunc computes a dense vector for a text string. Matches the
// pipeline's EmbedFunc signature; the gateway passes the same
// role-resolving closure it hands the pipeline, so the sweep fails over
// primary → backup exactly like every other embed call.
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

// Defaults for the sweep: check every 2 minutes, embed at most 64 rows
// per pass. The batch cap keeps a large backlog from monopolizing the
// embedder (and the sweep's own transaction time) — the next tick picks
// up where this one stopped.
const (
	defaultReembedInterval = 2 * time.Minute
	defaultReembedBatch    = 64
)

// ReembedSweeper owns the goroutine that drains pending_embeds.
type ReembedSweeper struct {
	pool     *db.Pool
	embed    EmbedFunc
	interval time.Duration
	batch    int

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once

	mu       sync.RWMutex
	lastRun  time.Time
	lastFix  int
	lastErrs int
}

// NewReembedSweeper constructs a sweeper without starting it. A nil pool
// or nil embed func yields a no-op sweeper (Start logs why and exits),
// which is the correct degrade for a deployment with no embedder
// configured — there is nothing that could fill the queue usefully.
func NewReembedSweeper(pool *db.Pool, embed EmbedFunc, interval time.Duration, batch int) *ReembedSweeper {
	if interval <= 0 {
		interval = defaultReembedInterval
	}
	if batch <= 0 {
		batch = defaultReembedBatch
	}
	return &ReembedSweeper{
		pool:     pool,
		embed:    embed,
		interval: interval,
		batch:    batch,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// Start begins the sweep loop. The first pass fires after one interval,
// giving the heartbeat time to establish real health first (a sweep on a
// cold registry would treat "unknown" as usable and burn a batch of
// attempts against an endpoint that may be down).
func (s *ReembedSweeper) Start(ctx context.Context) {
	if s == nil {
		return
	}
	if s.pool == nil || s.embed == nil {
		log.Printf("[reembed] no pool or embedder wired; re-embed sweep disabled")
		close(s.doneCh)
		return
	}
	go s.loop(ctx)
}

// Stop signals the loop to exit and waits for it. Safe to call twice.
func (s *ReembedSweeper) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() { close(s.stopCh) })
	<-s.doneCh
}

func (s *ReembedSweeper) loop(ctx context.Context) {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce drains up to one batch. Returns how many rows got a vector.
// Exported so an admin action (or a test) can force a pass.
func (s *ReembedSweeper) RunOnce(ctx context.Context) int {
	if s == nil || s.pool == nil || s.embed == nil {
		return 0
	}

	type pending struct {
		id      string
		content string
	}
	rows, err := s.pool.QueryContext(ctx, `
		SELECT p.memory_id::text, m.content
		  FROM pending_embeds p
		  JOIN memories m ON m.id = p.memory_id
		 WHERE m.embedding IS NULL
		 ORDER BY p.enqueued_at
		 LIMIT $1`, s.batch)
	if err != nil {
		log.Printf("[reembed] warning: could not read pending queue: %v", err)
		return 0
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.content); err != nil {
			continue
		}
		batch = append(batch, p)
	}
	rows.Close()

	// Rows whose memory already has a vector (re-embedded through some
	// other path, e.g. an admin edit) are stale queue entries — clear
	// them so the queue depth stays honest.
	s.dropSatisfied(ctx)

	if len(batch) == 0 {
		return 0
	}

	fixed, failed := 0, 0
	for _, p := range batch {
		embedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		vec, err := s.embed(embedCtx, p.content)
		cancel()
		if err != nil {
			// The embedder chain is down (or this row is poison). Record
			// the attempt and stop the pass: hammering a dead endpoint
			// with the rest of the batch buys nothing, and the next tick
			// retries.
			s.recordFailure(ctx, p.id, err)
			failed++
			log.Printf("[reembed] embed failed for memory %s (stopping pass): %v", p.id, err)
			break
		}
		if len(vec) == 0 {
			s.recordFailure(ctx, p.id, errEmptyVector)
			failed++
			continue
		}
		if err := s.applyVector(ctx, p.id, vec); err != nil {
			s.recordFailure(ctx, p.id, err)
			failed++
			continue
		}
		fixed++
	}

	s.mu.Lock()
	s.lastRun, s.lastFix, s.lastErrs = time.Now(), fixed, failed
	s.mu.Unlock()

	if fixed > 0 {
		log.Printf("[reembed] back-filled %d embedding(s); %d failure(s)", fixed, failed)
	}
	return fixed
}

// applyVector writes the vector and clears the queue row in one tx, so a
// crash between them can't drop a memory off the queue un-embedded.
func (s *ReembedSweeper) applyVector(ctx context.Context, memoryID string, vec []float32) error {
	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE memories SET embedding = $2, updated_at = NOW()
		 WHERE id = $1::uuid`, memoryID, vectorParam(vec)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pending_embeds WHERE memory_id = $1::uuid`, memoryID); err != nil {
		return err
	}
	return tx.Commit()
}

// dropSatisfied clears queue entries whose memory already has a vector
// or no longer exists (the FK CASCADE covers deletes, but a vector
// written by another path leaves a stale row).
func (s *ReembedSweeper) dropSatisfied(ctx context.Context) {
	if _, err := s.pool.ExecContext(ctx, `
		DELETE FROM pending_embeds p
		 WHERE NOT EXISTS (
		       SELECT 1 FROM memories m
		        WHERE m.id = p.memory_id AND m.embedding IS NULL)`); err != nil {
		log.Printf("[reembed] warning: could not prune satisfied queue rows: %v", err)
	}
}

func (s *ReembedSweeper) recordFailure(ctx context.Context, memoryID string, cause error) {
	if _, err := s.pool.ExecContext(ctx, `
		UPDATE pending_embeds
		   SET attempts = attempts + 1, last_error = $2
		 WHERE memory_id = $1::uuid`, memoryID, cause.Error()); err != nil {
		log.Printf("[reembed] warning: could not record failure for %s: %v", memoryID, err)
	}
}

// PendingCount returns the current queue depth — surfaced on the admin
// memory-health card next to missing_embeddings.
func (s *ReembedSweeper) PendingCount(ctx context.Context) (int, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	var n int
	err := s.pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_embeds`).Scan(&n)
	return n, err
}

// SweepStats is the last pass's outcome for the admin surface.
type SweepStats struct {
	LastRun   time.Time `json:"last_run"`
	LastFixed int       `json:"last_fixed"`
	LastErred int       `json:"last_erred"`
}

// Stats returns the most recent pass's results.
func (s *ReembedSweeper) Stats() SweepStats {
	if s == nil {
		return SweepStats{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SweepStats{LastRun: s.lastRun, LastFixed: s.lastFix, LastErred: s.lastErrs}
}
