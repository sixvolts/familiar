package memengine

// Periodic store maintenance (née the previous engine's
// sleep/consolidation cycle, the engine migration). The
// post-turn sidecar pass owns write-path quality — extraction,
// ADD/UPDATE/DUPLICATE classification, supersede chains, versions —
// so this job keeps only the time-based hygiene no per-turn pass can
// provide:
//
//   1. resolveConflicts — store-wide drift dedup: pairs of live
//      KNOWLEDGE facts (conversation chunks and wiki rows excluded)
//      within one (user_id, scope_tag) partition whose cosine
//      similarity clears the threshold get collapsed — the newer row
//      survives and points `supersedes` at the older, which hides
//      the older (retrieval treats pointed-at rows as dead). Catches
//      near-duplicates written independently before either existed,
//      which the write-time hash dedup + classifier can't see.
//   2. pruneOldSession — retention: hard-delete session-scope rows
//      (raw conversation chunks) idle past the archive window.
//
// Two phases that used to live here are gone, on purpose:
// promote_cross_session (its ≥2 conv:* tag trigger could never fire —
// ON CONFLICT re-commits don't merge tags, and extraction facts are
// born user-scope) and decay_stale_session (it wrote decay_score,
// which nothing anywhere read).
//
// One ticker, one goroutine, phases run sequentially with a short
// context per query. Operator-tunable interval + threshold via
// [sleep] config. Safe to call Stop multiple times.

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/db"
	"github.com/lib/pq"
)

// SleepCycle owns the goroutine that runs consolidation on a tick.
// Construct with NewSleepCycle and call Start to begin; Stop drains
// the goroutine cleanly. CycleStats lets the admin status panel
// surface the most recent pass.
type SleepCycle struct {
	pool    *db.Pool
	agentID string
	cfg     config.SleepConfig

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once

	mu     sync.RWMutex
	last   CycleStats
	cycles uint64

	// runMu keeps passes from overlapping: the ticker's, and on-demand
	// ones (StartSleep, the admin button), which ran concurrently.
	runMu sync.Mutex
	// dedupMark is how far the drift dedup has got: rows changed after
	// it (updated_at, then id) haven't been compared yet. Guarded by
	// runMu. Zero after a restart, so the first passes cover every row,
	// a batch at a time, across as many cycles as that takes.
	dedupMark dedupMark
}

type dedupMark struct {
	at time.Time
	id string
}

// CycleStats is one maintenance pass's results. Surfaced via
// LastStats() for the admin status panel + log lines on each tick.
type CycleStats struct {
	StartedAt         time.Time `json:"started_at"`
	DurationMs        int64     `json:"duration_ms"`
	ConflictsResolved uint32    `json:"conflicts_resolved"`
	FactsArchived     uint32    `json:"facts_archived"`
	Errors            []string  `json:"errors,omitempty"`
}

// NewSleepCycle constructs a cycle but does not start it. Caller
// must invoke Start exactly once. Nil pool returns a no-op cycle —
// Start logs and exits immediately, useful for deployments without
// memory.local_dsn.
func NewSleepCycle(pool *db.Pool, agentID string, cfg config.SleepConfig) *SleepCycle {
	return &SleepCycle{
		pool:    pool,
		agentID: agentID,
		cfg:     cfg,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start kicks off the ticker goroutine. Returns immediately. The
// first tick fires after cfg.Interval — not on Start — so a freshly
// booted gateway doesn't run a heavy consolidation pass during
// warmup. Pass nil to disable: a nil cycle does nothing on Start.
func (s *SleepCycle) Start(ctx context.Context) {
	if s == nil {
		return
	}
	if s.pool == nil {
		log.Printf("[sleep] no db pool wired; consolidation disabled")
		close(s.doneCh)
		return
	}
	if !s.cfg.Enabled {
		log.Printf("[sleep] disabled in config; consolidation will not run")
		close(s.doneCh)
		return
	}
	interval := s.cfg.IntervalSecs
	if interval <= 0 {
		interval = 1800 // 30 minutes default
	}
	go s.loop(ctx, time.Duration(interval)*time.Second)
}

// Stop signals the goroutine to exit and waits for it to finish.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *SleepCycle) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() { close(s.stopCh) })
	<-s.doneCh
}

// LastStats returns a copy of the most recent cycle's results.
// Empty until the first cycle completes.
func (s *SleepCycle) LastStats() CycleStats {
	if s == nil {
		return CycleStats{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

// CycleCount returns how many cycles have run since Start. Drives
// the admin panel's "last consolidation" timestamp + counter.
func (s *SleepCycle) CycleCount() uint64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cycles
}

func (s *SleepCycle) loop(ctx context.Context, interval time.Duration) {
	defer close(s.doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// RunOnce triggers a consolidation pass immediately. Exposed for
// the admin-panel "Run now" button + integration tests; the
// background loop calls runOnce directly without going through the
// public surface.
func (s *SleepCycle) RunOnce(ctx context.Context) CycleStats {
	if s == nil || s.pool == nil {
		return CycleStats{}
	}
	// A pass is already running (the ticker's, or another request's):
	// don't start a second one, and don't make the caller wait for it.
	if !s.runMu.TryLock() {
		stats := s.LastStats()
		stats.Errors = append(append([]string(nil), stats.Errors...), "a consolidation pass is already running")
		return stats
	}
	defer s.runMu.Unlock()
	return s.runPass(ctx)
}

func (s *SleepCycle) runOnce(ctx context.Context) CycleStats {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.runPass(ctx)
}

func (s *SleepCycle) runPass(ctx context.Context) CycleStats {
	stats := CycleStats{StartedAt: time.Now().UTC()}

	// Phase 1: resolve semantic conflicts in the persistent tier.
	// Bounded by a per-phase timeout so a slow pgvector query
	// doesn't stall the whole tick.
	phaseCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	n, err := s.resolveConflicts(phaseCtx, s.cfg.ConflictThreshold)
	cancel()
	if err != nil {
		stats.Errors = append(stats.Errors, "conflicts: "+err.Error())
	} else {
		stats.ConflictsResolved = n
	}

	// Phase 2: prune old session facts (chunk retention).
	phaseCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
	n, err = s.pruneOldSession(phaseCtx, s.cfg.SessionArchiveDays)
	cancel()
	if err != nil {
		stats.Errors = append(stats.Errors, "prune: "+err.Error())
	} else {
		stats.FactsArchived = n
	}

	stats.DurationMs = time.Since(stats.StartedAt).Milliseconds()

	s.mu.Lock()
	s.last = stats
	s.cycles++
	s.mu.Unlock()

	// Log once per cycle so a tail of `[sleep]` answers "did the
	// maintenance pass run last night, and what did it find?"
	// without spinning up a dashboard.
	if len(stats.Errors) > 0 {
		log.Printf("[sleep] cycle done in %dms: conflicts=%d archived=%d errors=%v",
			stats.DurationMs, stats.ConflictsResolved, stats.FactsArchived, stats.Errors)
	} else {
		log.Printf("[sleep] cycle done in %dms: conflicts=%d archived=%d",
			stats.DurationMs, stats.ConflictsResolved, stats.FactsArchived)
	}
	return stats
}

// resolveConflicts is the store-wide drift dedup: for each live
// KNOWLEDGE fact changed since the last pass, find its nearest live
// neighbor with a different content_hash inside the same (user_id,
// scope_tag) partition; if the cosine similarity clears the threshold,
// the NEWER row survives and points `supersedes` at the older one —
// which hides the older row, because everywhere in the system
// "superseded" means "another row points at me" (retrieval's NOT EXISTS
// filter, the extraction pipeline's UPDATE path, the console's
// superseded flag).
//
// Bounded work: rows are taken dedupBatch at a time in (updated_at, id)
// order from s.dedupMark, each batch compared with its whole partition,
// until none are left or ctx (the phase timeout) runs out; the mark
// carries over to the next cycle. Comparing every row with its
// partition in one statement is quadratic, and past about ten thousand
// facts it hit the timeout on every cycle, so the dedup never ran.
//
// A chain head (a fact that replaced an older one) can be the older
// side of a pair, and is then hidden in turn; only a row that replaces
// nothing can take a pointer (one pointer per row). Each older row gets
// one newer winner and each winner one loser: two winners pointing at
// one row branched its chain.
//
// Deliberately excluded from dedup:
//   - source_type='conversation' — raw turn chunks aren't knowledge;
//     asking a similar question twice must not hide either transcript.
//   - source_type='wiki_page' — wiki rows are owned by their source
//     page (clean-replaced on save), not by memory lifecycle.
//   - cross-partition pairs — a shard-isolated or wiki-scoped fact
//     must never supersede a top-level one, nor one user's another's.
//   - pairs of different embedding dimensions (a changed embedding
//     model): not comparable, and pgvector refuses the distance.
//
// Threshold defaults to 0.92 if cfg.ConflictThreshold is 0 — matches
// the noopDedupThreshold the gateway already uses at write time.
// Caller holds runMu.
func (s *SleepCycle) resolveConflicts(ctx context.Context, threshold float64) (uint32, error) {
	if threshold <= 0 {
		threshold = 0.92
	}
	var total uint32
	for ctx.Err() == nil {
		ids, next, err := s.dedupBatchAfter(ctx, s.dedupMark)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		n, err := s.dedupPass(ctx, threshold, ids)
		if err != nil {
			return total, err
		}
		total += n
		s.dedupMark = next
	}
	return total, nil
}

// dedupBatch is how many changed rows one statement compares with their
// partitions (a variable so tests can force several batches).
var dedupBatch = 200

// dedupCandidates is the set of rows the drift dedup considers: live
// (nothing points at them) knowledge rows with a vector and a hash.
const dedupCandidates = `
	agent_id = $1
	AND embedding IS NOT NULL
	AND content_hash IS NOT NULL
	AND source_type NOT IN ('conversation', 'wiki_page')
	AND NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = memories.id)`

// dedupBatchAfter returns the next batch of candidate ids changed after
// mark, in (updated_at, id) order, and the mark after them.
func (s *SleepCycle) dedupBatchAfter(ctx context.Context, mark dedupMark) ([]string, dedupMark, error) {
	rows, err := s.pool.QueryContext(ctx, `
		SELECT id::text, updated_at FROM memories
		 WHERE `+dedupCandidates+`
		   AND (updated_at, id::text) > ($2, $3)
		 ORDER BY updated_at, id::text
		 LIMIT $4`, s.agentID, mark.at, mark.id, dedupBatch)
	if err != nil {
		return nil, mark, err
	}
	defer rows.Close()
	var ids []string
	next := mark
	for rows.Next() {
		if err := rows.Scan(&next.id, &next.at); err != nil {
			return nil, mark, err
		}
		ids = append(ids, next.id)
	}
	return ids, next, rows.Err()
}

// dedupPass pairs each of ids with its nearest candidate neighbour and
// applies the winners.
func (s *SleepCycle) dedupPass(ctx context.Context, threshold float64, ids []string) (uint32, error) {
	res, err := s.pool.ExecContext(ctx, `
		WITH live AS (
			SELECT id, content_hash, embedding, created_at, user_id, scope_tag,
			       supersedes IS NULL AS free
			  FROM memories
			 WHERE `+dedupCandidates+`
		),
		pairs AS (
			SELECT a.id AS a_id, a.created_at AS a_created, a.free AS a_free,
			       b.id AS b_id, b.created_at AS b_created, b.free AS b_free
			  FROM live a
			  JOIN LATERAL (
			       SELECT id, created_at, free, embedding
			         FROM live l
			        WHERE l.id <> a.id
			          AND l.content_hash <> a.content_hash
			          AND l.user_id = a.user_id
			          AND l.scope_tag IS NOT DISTINCT FROM a.scope_tag
			          AND vector_dims(l.embedding) = vector_dims(a.embedding)
			        ORDER BY CASE WHEN vector_dims(l.embedding) = vector_dims(a.embedding)
			                      THEN l.embedding <=> a.embedding END
			        LIMIT 1
			  ) b ON true
			 WHERE a.id = ANY($3::uuid[])
			   AND (1.0 - (a.embedding <=> b.embedding)) >= $2
		),
		winners AS (
			SELECT CASE WHEN a_created > b_created THEN a_id ELSE b_id END AS winner,
			       CASE WHEN a_created > b_created THEN b_id ELSE a_id END AS loser,
			       GREATEST(a_created, b_created) AS winner_created
			  FROM pairs
			 WHERE a_created <> b_created
			   AND CASE WHEN a_created > b_created THEN a_free ELSE b_free END
		),
		one_per_loser AS (
			SELECT DISTINCT ON (loser) winner, loser
			  FROM winners
			 ORDER BY loser, winner_created DESC, winner
		),
		one_per_winner AS (
			SELECT DISTINCT ON (winner) winner, loser
			  FROM one_per_loser
			 ORDER BY winner, loser
		)
		UPDATE memories m
		   SET supersedes = d.loser,
		       updated_at = NOW()
		  FROM one_per_winner d
		 WHERE m.id = d.winner
		   AND m.agent_id = $1
		   AND m.supersedes IS NULL`, s.agentID, threshold, pq.Array(ids))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return uint32(n), nil
}

// pruneOldSession hard-deletes session facts past the archive
// window that aren't pointed at by a supersedes link. Mirrors the
// previous implementation exactly — session facts are ephemeral and hard delete
// is the correct retention answer.
//
// A row that itself supersedes another is kept: pruning it would bring
// back the older fact it hid (see memory.DeleteVersionsSQL). Session
// rows are conversation chunks, which nothing supersedes with today,
// so this only guards against that changing.
//
// archiveDays defaults to 90 if cfg.SessionArchiveDays is 0.
func (s *SleepCycle) pruneOldSession(ctx context.Context, archiveDays int) (uint32, error) {
	if archiveDays <= 0 {
		archiveDays = 90
	}
	res, err := s.pool.ExecContext(ctx, `
		DELETE FROM memories
		 WHERE agent_id = $1
		   AND scope = 'session'
		   AND last_accessed < NOW() - make_interval(days => $2)
		   AND supersedes IS NULL
		   AND NOT EXISTS (
		       SELECT 1 FROM memories s WHERE s.supersedes = memories.id
		   )`, s.agentID, archiveDays)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return uint32(n), nil
}
