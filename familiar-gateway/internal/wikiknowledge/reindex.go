package wikiknowledge

// One-time wiki knowledge re-index.
//
// Wiki facts used to be keyed by "{book_slug}/{page_slug}", and slugs
// follow the title. The wiki_page_fact_identity migration re-keyed the
// facts whose slug still named a live page and dropped the rest, which
// includes most of the knowledge of any page titled after its first
// save (new pages start "untitled", and the old upsert never moved a
// fact to the new slug). This job gives every live page its knowledge
// back: it re-extracts each page once, through the same pipeline and
// per-page rules as a save.
//
// It runs in the gateway, in the background after boot, because only
// the running gateway can order it against live saves (Reingest) and
// share the sidecar's slot gate. It goes one page at a time with a
// pause between pages, keeps per-page progress in wiki_reindex_progress
// so a restart resumes it, holds an advisory lock during each pass so
// two gateways on one database don't both run it, and records itself done in
// applied_data_fixes. Pages saved after the job first started are
// skipped: their save already ingested them under the new key.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/db"
)

const (
	// reindexFix is the applied_data_fixes row that marks the job done;
	// reindexStarted records when it first started.
	reindexFix     = "wiki_knowledge_reindex"
	reindexStarted = "wiki_knowledge_reindex_started"
	// maxPageAttempts is how many failed extractions a page gets before
	// the job gives up on it (it is re-extracted on its next save).
	maxPageAttempts = 3
	// outageStreak failures in a row, with no success in between, is
	// the extractor being down rather than bad pages: the pass stops,
	// and those failures aren't counted against the pages.
	outageStreak = 3
	// reindexLockKey is the pg_advisory_lock key held while the job
	// runs. Arbitrary but must never change.
	reindexLockKey = 0x57494B4952454958 // "WIKIREIX"
)

// ReindexStore is the database side of the re-index job.
type ReindexStore interface {
	// Lock takes the job's advisory lock; ok is false when another
	// process holds it. release frees it.
	Lock(ctx context.Context) (release func(), ok bool, err error)
	// Done reports whether the job has already completed.
	Done(ctx context.Context) (bool, error)
	// Begin records when the job first started (a resumed job keeps the
	// original time).
	Begin(ctx context.Context) error
	// PendingPages lists the live, non-research pages last saved before
	// the job first started that it hasn't finished or given up on.
	PendingPages(ctx context.Context) ([]string, error)
	// LoadPage reads a page's current content as a save would pass it;
	// false when the page is gone.
	LoadPage(ctx context.Context, pageID string) (SaveEvent, bool, error)
	MarkDone(ctx context.Context, pageID string) error
	RecordFailure(ctx context.Context, pageID, cause string) error
	// Finish marks the job done, returning how many pages it gave up on.
	Finish(ctx context.Context) (gaveUp int, err error)
}

// Reindexer runs the job. Pipeline and Store are required.
type Reindexer struct {
	Pipeline *Pipeline
	Store    ReindexStore
	// Pause is the wait between pages, so the job never keeps the
	// extract model busy back to back. Zero uses 5s.
	Pause time.Duration
	// Backoff is the wait after a pass that looked like an outage, or
	// that had failures and no success. Zero uses 10 minutes.
	Backoff time.Duration

	sleep func(context.Context, time.Duration) error // tests replace it
}

// Run works through every pending page until none is left, then marks
// the job done. It returns early when the job is already done, another
// gateway holds its lock, or ctx ends.
func (r *Reindexer) Run(ctx context.Context) error {
	if r.Pause <= 0 {
		r.Pause = 5 * time.Second
	}
	if r.Backoff <= 0 {
		r.Backoff = 10 * time.Minute
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	for {
		wait, finished, err := r.lockedPass(ctx)
		if err != nil || finished {
			return err
		}
		if wait {
			if err := r.sleep(ctx, r.Backoff); err != nil {
				return err
			}
		}
	}
}

// lockedPass runs one pass under the job's advisory lock. The lock pins
// a pool connection, so it is held for a pass, not across the backoff
// between passes. finished is true when there is nothing left to do
// here: the job is done, or another gateway holds the lock.
func (r *Reindexer) lockedPass(ctx context.Context) (wait, finished bool, err error) {
	if done, err := r.Store.Done(ctx); err != nil || done {
		return false, true, err
	}
	release, ok, err := r.Store.Lock(ctx)
	if err != nil {
		return false, true, err
	}
	if !ok {
		log.Printf("[wikiknowledge] re-index: another gateway is running it")
		return false, true, nil
	}
	defer release()
	// Re-checked under the lock: another gateway may have finished it.
	if done, err := r.Store.Done(ctx); err != nil || done {
		return false, true, err
	}
	if err := r.Store.Begin(ctx); err != nil {
		return false, true, err
	}
	ids, err := r.Store.PendingPages(ctx)
	if err != nil {
		return false, true, err
	}
	if len(ids) == 0 {
		gaveUp, err := r.Store.Finish(ctx)
		if err != nil {
			return false, true, err
		}
		log.Printf("[wikiknowledge] re-index done (%d page(s) given up on after %d failed attempts; they re-extract on their next save)", gaveUp, maxPageAttempts)
		return false, true, nil
	}
	log.Printf("[wikiknowledge] re-index: %d page(s) to re-extract", len(ids))
	wait, err = r.pass(ctx, ids)
	return wait, err != nil, err
}

// pass re-extracts ids once each. It reports whether the caller should
// back off before the next pass.
func (r *Reindexer) pass(ctx context.Context, ids []string) (backoff bool, err error) {
	// failures since the last success: counted against their pages once
	// the extractor is shown to work, dropped if they turn out to be an
	// outage.
	type failure struct{ id, cause string }
	var pending []failure
	succeeded := false
	flush := func() error {
		for _, f := range pending {
			if err := r.Store.RecordFailure(ctx, f.id, f.cause); err != nil {
				return err
			}
		}
		pending = nil
		return nil
	}
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		out, loadErr := r.Pipeline.Reingest(ctx, id, func(ctx context.Context) (SaveEvent, bool, error) {
			return r.Store.LoadPage(ctx, id)
		})
		switch out {
		case Committed, Superseded, Skipped:
			if err := r.Store.MarkDone(ctx, id); err != nil {
				return false, err
			}
			if out == Committed {
				succeeded = true
				if err := flush(); err != nil {
					return false, err
				}
			}
		default:
			cause := "extraction failed"
			if loadErr != nil {
				cause = loadErr.Error()
			}
			pending = append(pending, failure{id, cause})
			if len(pending) >= outageStreak {
				log.Printf("[wikiknowledge] re-index: %d failures in a row, pausing (extractor down?)", len(pending))
				return true, nil
			}
		}
		if (i+1)%10 == 0 {
			log.Printf("[wikiknowledge] re-index: %d/%d", i+1, len(ids))
		}
		if i < len(ids)-1 {
			if err := r.sleep(ctx, r.Pause); err != nil {
				return false, err
			}
		}
	}
	// Trailing failures, short of an outage: count them. With no success
	// in the whole pass, wait before retrying, so a short outage doesn't
	// use up the few pages left.
	failedOnly := !succeeded && len(pending) > 0
	if err := flush(); err != nil {
		return false, err
	}
	return failedOnly, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// PgReindexStore is the Postgres ReindexStore. Links loads a page's
// outbound links (admin.WikiStore.ListPageLinks), so the link triples
// match what a save would write.
type PgReindexStore struct {
	DB    *db.Pool
	Links func(ctx context.Context, pageID string) ([]admin.PageLink, error)
}

func (s *PgReindexStore) Lock(ctx context.Context) (func(), bool, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, int64(reindexLockKey)).Scan(&ok); err != nil || !ok {
		conn.Close()
		return nil, false, err
	}
	return func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(reindexLockKey))
		conn.Close()
	}, true, nil
}

func (s *PgReindexStore) Done(ctx context.Context) (bool, error) {
	var done bool
	err := s.DB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM applied_data_fixes WHERE name = $1)`, reindexFix).Scan(&done)
	return done, err
}

func (s *PgReindexStore) Begin(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO applied_data_fixes (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`, reindexStarted)
	return err
}

func (s *PgReindexStore) PendingPages(ctx context.Context) ([]string, error) {
	// Most recently edited first: likeliest to matter, and the least
	// likely to have been saved again before the job reaches them.
	rows, err := s.DB.QueryContext(ctx, `
		SELECT p.id::text
		  FROM wiki_pages p
		  JOIN books b ON b.id = p.book_id
		  LEFT JOIN wiki_reindex_progress r ON r.page_id = p.id
		 WHERE p.deleted_at IS NULL
		   AND b.slug NOT LIKE 'research:%'
		   AND p.updated_at <= COALESCE(
		           (SELECT applied_at FROM applied_data_fixes WHERE name = $2),
		           NOW())
		   AND (r.page_id IS NULL OR (r.done_at IS NULL AND r.attempts < $1))
		 ORDER BY p.updated_at DESC, p.id`, maxPageAttempts, reindexStarted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *PgReindexStore) LoadPage(ctx context.Context, pageID string) (SaveEvent, bool, error) {
	var evt SaveEvent
	err := s.DB.QueryRowContext(ctx, `
		SELECT p.book_id::text, b.slug, p.id::text, p.slug, p.updated_by, p.title, p.content
		  FROM wiki_pages p
		  JOIN books b ON b.id = p.book_id
		 WHERE p.id = $1::uuid AND p.deleted_at IS NULL`, pageID,
	).Scan(&evt.BookID, &evt.BookSlug, &evt.PageID, &evt.PageSlug, &evt.UserID, &evt.Title, &evt.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return SaveEvent{}, false, nil
	}
	if err != nil {
		return SaveEvent{}, false, err
	}
	if s.Links != nil {
		links, err := s.Links(ctx, pageID)
		if err != nil {
			return SaveEvent{}, false, fmt.Errorf("links: %w", err)
		}
		evt.Links = links
	}
	return evt, true, nil
}

func (s *PgReindexStore) MarkDone(ctx context.Context, pageID string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO wiki_reindex_progress (page_id, done_at) VALUES ($1::uuid, NOW())
		ON CONFLICT (page_id) DO UPDATE SET done_at = NOW()`, pageID)
	if isForeignKeyViolation(err) {
		return nil // the page was purged meanwhile; nothing to track
	}
	return err
}

func (s *PgReindexStore) RecordFailure(ctx context.Context, pageID, cause string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO wiki_reindex_progress (page_id, attempts, last_error) VALUES ($1::uuid, 1, $2)
		ON CONFLICT (page_id) DO UPDATE
		   SET attempts = wiki_reindex_progress.attempts + 1, last_error = $2`, pageID, cause)
	if isForeignKeyViolation(err) {
		return nil
	}
	return err
}

func (s *PgReindexStore) Finish(ctx context.Context) (int, error) {
	var gaveUp int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wiki_reindex_progress WHERE done_at IS NULL`).Scan(&gaveUp); err != nil {
		return 0, err
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO applied_data_fixes (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`, reindexFix)
	return gaveUp, err
}

// isForeignKeyViolation reports a Postgres foreign_key_violation (23503).
func isForeignKeyViolation(err error) bool {
	var coded interface{ SQLState() string }
	return err != nil && errors.As(err, &coded) && coded.SQLState() == "23503"
}
