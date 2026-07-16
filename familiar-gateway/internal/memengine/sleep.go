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
	return s.runOnce(ctx)
}

func (s *SleepCycle) runOnce(ctx context.Context) CycleStats {
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
// KNOWLEDGE fact, find its nearest live neighbor with a different
// content_hash inside the same (user_id, scope_tag) partition; if
// the cosine similarity clears the threshold, the NEWER row survives
// and points `supersedes` at the older one — which hides the older
// row, because everywhere in the system "superseded" means "another
// row points at me" (retrieval's NOT EXISTS filter, the extraction
// pipeline's UPDATE path, the console's superseded flag).
//
// Deliberately excluded from dedup:
//   - source_type='conversation' — raw turn chunks aren't knowledge;
//     asking a similar question twice must not hide either transcript.
//   - source_type='wiki_page' — wiki rows are owned by their source
//     page (clean-replaced on save), not by memory lifecycle.
//   - cross-partition pairs — a shard-isolated or wiki-scoped fact
//     must never supersede a top-level one, nor one user's another's.
//
// Threshold defaults to 0.92 if cfg.ConflictThreshold is 0 — matches
// the noopDedupThreshold the gateway already uses at write time.
func (s *SleepCycle) resolveConflicts(ctx context.Context, threshold float64) (uint32, error) {
	if threshold <= 0 {
		threshold = 0.92
	}
	res, err := s.pool.ExecContext(ctx, `
		WITH live AS (
			SELECT id, content_hash, embedding, created_at, user_id, scope_tag
			  FROM memories
			 WHERE agent_id = $1
			   AND embedding IS NOT NULL
			   AND content_hash IS NOT NULL
			   AND supersedes IS NULL
			   AND source_type NOT IN ('conversation', 'wiki_page')
			   AND NOT EXISTS (
			       SELECT 1 FROM memories s WHERE s.supersedes = memories.id
			   )
		),
		pairs AS (
			SELECT a.id AS a_id, a.created_at AS a_created,
			       b.id AS b_id, b.created_at AS b_created,
			       (1.0 - (a.embedding <=> b.embedding))::float4 AS similarity
			  FROM live a
			  JOIN LATERAL (
			       SELECT id, content_hash, embedding, created_at
			         FROM live l
			        WHERE l.id <> a.id
			          AND l.content_hash <> a.content_hash
			          AND l.user_id = a.user_id
			          AND l.scope_tag IS NOT DISTINCT FROM a.scope_tag
			        ORDER BY l.embedding <=> a.embedding
			        LIMIT 1
			  ) b ON true
			 WHERE (1.0 - (a.embedding <=> b.embedding)) >= $2
		),
		winners AS (
			SELECT CASE WHEN a_created > b_created THEN a_id ELSE b_id END AS winner,
			       CASE WHEN a_created > b_created THEN b_id ELSE a_id END AS loser
			  FROM pairs
			 WHERE a_created <> b_created
		),
		dedup AS (
			SELECT DISTINCT winner, loser FROM winners
		)
		UPDATE memories m
		   SET supersedes = d.loser,
		       updated_at = NOW()
		  FROM dedup d
		 WHERE m.id = d.winner
		   AND m.agent_id = $1
		   AND m.supersedes IS NULL`, s.agentID, threshold)
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
		   AND NOT EXISTS (
		       SELECT 1 FROM memories s WHERE s.supersedes = memories.id
		   )`, s.agentID, archiveDays)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return uint32(n), nil
}
