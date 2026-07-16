// Package notesmigration backfills the legacy `notes` table into
// per-user personal books (BOOKS-WIKI-ARCHITECTURE Phase 1 step 5).
//
// Each note becomes a wiki_pages row in the user's personal book.
// The migration is idempotent: it preserves the note's UUID as the
// page's UUID, so re-runs short-circuit on rows that already exist.
// External references to note IDs (chat tool calls, bookmarks,
// frontend caches) keep resolving post-migration because the IDs
// don't change.
//
// Pinned notes get a row in user_page_prefs (the per-user pin
// table). Folders are dropped during migration — the architecture
// doc maps folders to parent pages in Phase 2; for now they're
// preserved nowhere except in the original notes table (which the
// operator can keep around as a backup until Phase 2 ships).
//
// Soft-deleted notes are skipped — they're already invisible from
// the UX and shouldn't take up space in the new table. Operators
// who want to preserve them can pass --include-deleted.
//
// The package is intentionally SQL-direct rather than going
// through admin.WikiStore. Migration is a one-shot operational
// pass; bypassing the public API lets us preserve original IDs
// and timestamps that the public CRUD path doesn't expose.
package notesmigration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/familiar/gateway/internal/db"
)

// Options control which notes get picked up.
type Options struct {
	// IncludeDeleted folds soft-deleted notes (deleted_at IS NOT
	// NULL) into the migration. Off by default — operators
	// shouldn't accidentally resurrect notes their user already
	// trashed.
	IncludeDeleted bool
}

// UserPlan summarizes what would happen for one user. Returned by
// Plan; used by the ctl tool's plan / dry-run mode to print a
// per-user breakdown before any writes.
type UserPlan struct {
	UserID          string
	NoteCount       int // total notes for this user (post-Options filter)
	AlreadyMigrated int // notes whose id already lives in wiki_pages
	ToMigrate       int // = NoteCount - AlreadyMigrated
}

// Result is the outcome of MigrateUser. Errors is per-note so a
// single bad row doesn't kill the batch — the note id + reason are
// captured and the loop continues.
type Result struct {
	UserID   string
	Migrated int
	Skipped  int // already migrated (idempotency)
	Failed   int
	Errors   []string // formatted "note=<id>: <reason>"
}

// VerifyResult compares notes-table counts against the migrated
// pages. Match is true when every (non-filtered) note has a
// matching wiki_pages row in the user's personal book. Orphan-
// Notes lists any note IDs that didn't make it across.
type VerifyResult struct {
	UserID        string
	NotesCount    int
	MigratedCount int
	Match         bool
	OrphanNoteIDs []string
}

// Plan walks every user with notes and returns per-user counts
// without writing anything. Cheap — two aggregate queries per
// user. Used by the dry-run mode to show operators what they'd
// migrate before pulling the trigger.
func Plan(ctx context.Context, pool *db.Pool, opts Options) ([]UserPlan, error) {
	users, err := listUsersWithNotes(ctx, pool, opts.IncludeDeleted)
	if err != nil {
		return nil, err
	}
	out := make([]UserPlan, 0, len(users))
	for _, uid := range users {
		p, err := PlanUser(ctx, pool, uid, opts)
		if err != nil {
			return nil, fmt.Errorf("plan %s: %w", uid, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// PlanUser is the per-user variant of Plan.
func PlanUser(ctx context.Context, pool *db.Pool, userID string, opts Options) (UserPlan, error) {
	var noteCount, migratedCount int
	q := `SELECT COUNT(*) FROM notes WHERE user_id = $1`
	if !opts.IncludeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	if err := pool.QueryRowContext(ctx, q, userID).Scan(&noteCount); err != nil {
		return UserPlan{}, fmt.Errorf("count notes: %w", err)
	}
	// Count notes whose UUID already lives in wiki_pages — those
	// would be skipped on a real migration run (idempotency).
	mq := `SELECT COUNT(*) FROM notes n
	          JOIN wiki_pages p ON p.id = n.id
	         WHERE n.user_id = $1`
	if !opts.IncludeDeleted {
		mq += ` AND n.deleted_at IS NULL`
	}
	if err := pool.QueryRowContext(ctx, mq, userID).Scan(&migratedCount); err != nil {
		return UserPlan{}, fmt.Errorf("count migrated: %w", err)
	}
	return UserPlan{
		UserID:          userID,
		NoteCount:       noteCount,
		AlreadyMigrated: migratedCount,
		ToMigrate:       noteCount - migratedCount,
	}, nil
}

// MigrateAll runs MigrateUser against every user with un-migrated
// notes. Returns the per-user results. A failure for one user does
// not stop the batch — partial migrations are surfaced via
// Result.Failed / Result.Errors so operators can re-run after
// fixing the underlying issue.
func MigrateAll(ctx context.Context, pool *db.Pool, opts Options) ([]Result, error) {
	users, err := listUsersWithNotes(ctx, pool, opts.IncludeDeleted)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(users))
	for _, uid := range users {
		r, err := MigrateUser(ctx, pool, uid, opts)
		if err != nil {
			// Hard error (e.g. couldn't ensure personal book) is
			// captured in the Result struct so the caller still
			// gets a per-user accounting and continues with other
			// users.
			r.Errors = append(r.Errors, "fatal: "+err.Error())
		}
		out = append(out, r)
	}
	return out, nil
}

// MigrateUser converts every note for one user into a wiki_pages
// row in their personal book. Idempotent: notes whose UUID is
// already present in wiki_pages are skipped.
//
// Per-note flow inside the loop:
//  1. Skip if a wiki_pages row with this note's id already exists.
//  2. Generate a unique slug from the note's title (collisions
//     get -2, -3, ... suffixes within the user's personal book).
//  3. Insert wiki_pages with the note's id, title, content,
//     created_at, updated_at preserved.
//  4. Insert wiki_revisions seed row.
//  5. If the note was pinned, upsert a user_page_prefs row.
//
// All writes for one note happen in a single transaction so a mid-
// note failure leaves no half-migrated state.
func MigrateUser(ctx context.Context, pool *db.Pool, userID string, opts Options) (Result, error) {
	res := Result{UserID: userID}

	bookID, err := ensurePersonalBookSQL(ctx, pool, userID)
	if err != nil {
		return res, fmt.Errorf("ensure personal book: %w", err)
	}

	rows, err := selectNotesForMigration(ctx, pool, userID, opts.IncludeDeleted)
	if err != nil {
		return res, fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()

	type noteRow struct {
		ID, Title, Content   string
		Pinned               bool
		CreatedAt, UpdatedAt sql.NullTime
		DeletedAt            sql.NullTime
	}
	var notes []noteRow
	for rows.Next() {
		var n noteRow
		if err := rows.Scan(&n.ID, &n.Title, &n.Content, &n.Pinned,
			&n.CreatedAt, &n.UpdatedAt, &n.DeletedAt); err != nil {
			return res, fmt.Errorf("scan note: %w", err)
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("iter notes: %w", err)
	}

	// Slug bookkeeping per book — the unique index covers the
	// (book_id, slug) pair and migration may insert multiple
	// pages in one batch, so we track in-memory to avoid SQL
	// round-trips for collision detection.
	usedSlugs, err := loadExistingSlugs(ctx, pool, bookID)
	if err != nil {
		return res, fmt.Errorf("load existing slugs: %w", err)
	}

	for _, n := range notes {
		exists, err := pageIDExists(ctx, pool, n.ID)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("note=%s: check existing: %v", n.ID, err))
			continue
		}
		if exists {
			res.Skipped++
			continue
		}
		slug := uniqueSlugInSet(slugify(n.Title), usedSlugs)
		usedSlugs[slug] = true

		if err := insertMigratedPage(ctx, pool, bookID, userID, n.ID, slug,
			n.Title, n.Content, n.CreatedAt, n.UpdatedAt, n.DeletedAt, n.Pinned,
		); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("note=%s: insert: %v", n.ID, err))
			continue
		}
		res.Migrated++
	}
	return res, nil
}

// VerifyAll runs VerifyUser per user and returns the per-user
// results. Cheap aggregate-only queries; safe to run repeatedly.
func VerifyAll(ctx context.Context, pool *db.Pool, opts Options) ([]VerifyResult, error) {
	users, err := listUsersWithNotes(ctx, pool, opts.IncludeDeleted)
	if err != nil {
		return nil, err
	}
	out := make([]VerifyResult, 0, len(users))
	for _, uid := range users {
		v, err := VerifyUser(ctx, pool, uid, opts)
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", uid, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// VerifyUser confirms that every note for a user has a matching
// wiki_pages row (by id). Returns the lists of orphans so the
// operator can re-run MigrateUser to pick them up.
func VerifyUser(ctx context.Context, pool *db.Pool, userID string, opts Options) (VerifyResult, error) {
	var noteCount, migratedCount int
	notesQ := `SELECT COUNT(*) FROM notes WHERE user_id = $1`
	if !opts.IncludeDeleted {
		notesQ += ` AND deleted_at IS NULL`
	}
	if err := pool.QueryRowContext(ctx, notesQ, userID).Scan(&noteCount); err != nil {
		return VerifyResult{}, fmt.Errorf("count notes: %w", err)
	}
	migrQ := `SELECT COUNT(*) FROM notes n
	             JOIN wiki_pages p ON p.id = n.id
	            WHERE n.user_id = $1`
	if !opts.IncludeDeleted {
		migrQ += ` AND n.deleted_at IS NULL`
	}
	if err := pool.QueryRowContext(ctx, migrQ, userID).Scan(&migratedCount); err != nil {
		return VerifyResult{}, fmt.Errorf("count migrated: %w", err)
	}

	orphans := []string{}
	if noteCount != migratedCount {
		orphanQ := `SELECT n.id::text FROM notes n
		            LEFT JOIN wiki_pages p ON p.id = n.id
		           WHERE n.user_id = $1 AND p.id IS NULL`
		if !opts.IncludeDeleted {
			orphanQ += ` AND n.deleted_at IS NULL`
		}
		rows, err := pool.QueryContext(ctx, orphanQ, userID)
		if err != nil {
			return VerifyResult{}, fmt.Errorf("list orphans: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return VerifyResult{}, fmt.Errorf("scan orphan: %w", err)
			}
			orphans = append(orphans, id)
		}
		rows.Close()
	}
	return VerifyResult{
		UserID:        userID,
		NotesCount:    noteCount,
		MigratedCount: migratedCount,
		Match:         noteCount == migratedCount,
		OrphanNoteIDs: orphans,
	}, nil
}

// ── SQL helpers ──────────────────────────────────────────────────

func listUsersWithNotes(ctx context.Context, pool *db.Pool, includeDeleted bool) ([]string, error) {
	q := `SELECT DISTINCT user_id FROM notes`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY user_id`
	rows, err := pool.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func selectNotesForMigration(ctx context.Context, pool *db.Pool, userID string, includeDeleted bool) (*sql.Rows, error) {
	q := `SELECT id::text, title, content, pinned, created_at, updated_at, deleted_at
	        FROM notes
	       WHERE user_id = $1`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	q += ` ORDER BY created_at ASC`
	return pool.QueryContext(ctx, q, userID)
}

// ensurePersonalBookSQL is a SQL-direct equivalent of WikiStore.
// EnsurePersonalBook. Duplicating it here keeps the migration
// package independent of admin (which would drag in HTTP handlers
// and the rest of the world). Same idempotency contract: race
// loser falls back to a SELECT.
func ensurePersonalBookSQL(ctx context.Context, pool *db.Pool, userID string) (string, error) {
	slug := "personal:" + userID
	// Fast path.
	var id string
	err := pool.QueryRowContext(ctx,
		`SELECT id::text FROM books WHERE slug = $1`, slug).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("check personal book: %w", err)
	}
	// Slow path — insert + add owner row in a tx.
	tx, err := pool.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO books (slug, name, description, is_personal, created_by)
		VALUES ($1, 'Personal', 'Your personal notes', true, $2)
		RETURNING id::text`, slug, userID).Scan(&id); err != nil {
		// Race — re-read.
		var raceID string
		if rErr := pool.QueryRowContext(ctx,
			`SELECT id::text FROM books WHERE slug = $1`, slug,
		).Scan(&raceID); rErr == nil {
			return raceID, nil
		}
		return "", fmt.Errorf("insert personal book: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO book_members (book_id, user_id, role)
		VALUES ($1::uuid, $2, 'owner')`, id, userID); err != nil {
		return "", fmt.Errorf("add owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit personal book: %w", err)
	}
	return id, nil
}

func pageIDExists(ctx context.Context, pool *db.Pool, pageID string) (bool, error) {
	var exists bool
	err := pool.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM wiki_pages WHERE id = $1::uuid)`,
		pageID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// loadExistingSlugs pre-populates the in-memory dedup set from the
// book's current slug list so a partial-migration re-run doesn't
// allocate -2 suffixes for already-existing slugs.
func loadExistingSlugs(ctx context.Context, pool *db.Pool, bookID string) (map[string]bool, error) {
	rows, err := pool.QueryContext(ctx,
		`SELECT slug FROM wiki_pages WHERE book_id = $1::uuid`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

func insertMigratedPage(ctx context.Context, pool *db.Pool, bookID, userID, pageID, slug, title, content string, createdAt, updatedAt, deletedAt sql.NullTime, pinned bool) error {
	tx, err := pool.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// COALESCE the timestamps so notes with NULL created_at
	// (shouldn't happen but defensively) land on a sane default.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO wiki_pages
		    (id, book_id, slug, title, content,
		     created_by, updated_by, edit_count,
		     created_at, updated_at, deleted_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5,
		        $6, $6, 1,
		        COALESCE($7, NOW()), COALESCE($8, NOW()), $9)`,
		pageID, bookID, slug, title, content, userID,
		createdAt, updatedAt, deletedAt,
	); err != nil {
		return fmt.Errorf("insert page: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO wiki_revisions (page_id, content, edited_by, summary)
		VALUES ($1::uuid, $2, $3, 'migrated from notes table')`,
		pageID, content, userID,
	); err != nil {
		return fmt.Errorf("insert revision: %w", err)
	}
	if pinned {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_page_prefs (user_id, page_id, pinned)
			VALUES ($1, $2::uuid, true)
			ON CONFLICT (user_id, page_id) DO UPDATE SET pinned = true`,
			userID, pageID,
		); err != nil {
			return fmt.Errorf("set pin pref: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ── Slug helpers ──────────────────────────────────────────────────
//
// Duplicated from admin to keep this package self-contained. The
// admin-side slugify is unexported (and importing admin here would
// drag in HTTP handlers + a much larger dep graph). Migration is a
// one-shot tool — keeping it standalone is worth a few duplicated
// lines.

var slugSafeRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(in string) string {
	s := strings.ToLower(strings.TrimSpace(in))
	s = slugSafeRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "untitled"
	}
	if len(s) > 60 {
		s = s[:60]
		if i := strings.LastIndexByte(s, '-'); i > 30 {
			s = s[:i]
		}
	}
	return s
}

// uniqueSlugInSet appends -2, -3, ... until the slug isn't in
// `used`. Caller is responsible for inserting the returned slug
// into `used` after a successful write.
func uniqueSlugInSet(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !used[candidate] {
			return candidate
		}
	}
}
