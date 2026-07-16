package notesmigration

// Integration tests for the notes → personal-book migration.
// These tests opt in via FAMILIAR_TEST_DSN; the default
// `go test ./...` run skips them so unit-only CI stays hermetic.
//
// What these tests cover that the pure-function tests can't:
//   * Real Postgres + the gateway's full migration set, so the
//     SQL-direct paths in this package are exercised against the
//     same schema production runs against.
//   * Idempotency by ID preservation: re-running MigrateUser
//     short-circuits notes whose UUID is already in wiki_pages.
//   * Slug collisions: two notes with the same title get the
//     -2 / -3 suffix path inside the same book without violating
//     the unique index.
//   * Pinned mapping: notes.pinned → user_page_prefs.pinned.
//   * Soft-delete behavior: skipped by default, included with
//     Options.IncludeDeleted.
//   * VerifyUser: detects orphans when migration is partial.
//
// The devops agent supplies a Postgres DSN via FAMILIAR_TEST_DSN.
// See docs/notes-migration-runbook.md for one-shot setup steps.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testutil"
)

// ── Setup ─────────────────────────────────────────────────────────

// setupMigrationTest brings the migration-relevant tables back to
// a known state and seeds the FK target user. Returns a pool
// caller already holds the t.Cleanup hook for closing.
//
// We use unique per-test user IDs (e.g. notesmig-{TestName}) so
// two tests touching the same DB don't share migration state. The
// truncates are scoped through CASCADE on books — wiki_pages,
// wiki_revisions, wiki_page_links, wiki_page_entities, and
// user_page_prefs all chain off books or wiki_pages.
func setupMigrationTest(t *testing.T) (*db.Pool, string) {
	t.Helper()
	pool := testutil.PgTestPool(t)
	ctx := context.Background()

	// Wipe the migration-relevant slate. CASCADE handles the FK
	// chain. notes is a separate top-level table so it gets its
	// own truncate.
	if _, err := pool.ExecContext(ctx, `TRUNCATE TABLE notes`); err != nil {
		t.Fatalf("truncate notes: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `TRUNCATE TABLE books CASCADE`); err != nil {
		t.Fatalf("truncate books: %v", err)
	}

	// Per-test unique user id keeps cross-test runs from sharing
	// state via the FK-referenced users row.
	userID := "notesmig-" + sanitizeForUserID(t.Name())
	_, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status)
		VALUES ($1, $1, 'approved')
		ON CONFLICT (id) DO NOTHING`, userID)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return pool, userID
}

// sanitizeForUserID strips chars users.id wouldn't tolerate (we're
// using TEXT but slashes from subtest names look ugly in any
// downstream report).
func sanitizeForUserID(name string) string {
	r := strings.NewReplacer("/", "-", " ", "-", ":", "-")
	return strings.ToLower(r.Replace(name))
}

// seedNote inserts a single note row. UUIDs are auto-generated
// server-side so we read the id back. Returns the note's id.
func seedNote(t *testing.T, pool *db.Pool, userID, title, content string, pinned bool, deleted bool) string {
	t.Helper()
	q := `
		INSERT INTO notes (user_id, title, content, pinned, deleted_at)
		VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN NOW() ELSE NULL END)
		RETURNING id::text`
	var id string
	if err := pool.QueryRowContext(context.Background(), q,
		userID, title, content, pinned, deleted,
	).Scan(&id); err != nil {
		t.Fatalf("seed note: %v", err)
	}
	return id
}

// pageRow snapshots the columns we want to assert against.
type pageRow struct {
	ID, BookID, Slug, Title, Content string
	CreatedAt, UpdatedAt             time.Time
}

func loadPage(t *testing.T, pool *db.Pool, pageID string) (pageRow, bool) {
	t.Helper()
	var p pageRow
	err := pool.QueryRowContext(context.Background(), `
		SELECT id::text, book_id::text, slug, title, content,
		       created_at, updated_at
		  FROM wiki_pages
		 WHERE id = $1::uuid`, pageID,
	).Scan(&p.ID, &p.BookID, &p.Slug, &p.Title, &p.Content, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return pageRow{}, false
	}
	return p, true
}

func countRevisions(t *testing.T, pool *db.Pool, pageID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM wiki_revisions WHERE page_id = $1::uuid`,
		pageID,
	).Scan(&n); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	return n
}

func loadPin(t *testing.T, pool *db.Pool, userID, pageID string) (bool, bool) {
	t.Helper()
	var pinned bool
	err := pool.QueryRowContext(context.Background(), `
		SELECT pinned FROM user_page_prefs
		 WHERE user_id = $1 AND page_id = $2::uuid`,
		userID, pageID,
	).Scan(&pinned)
	if err != nil {
		return false, false
	}
	return pinned, true
}

// ── Tests ─────────────────────────────────────────────────────────

// TestMigrateUser_HappyPath covers the core promise: a note in
// the legacy table arrives in wiki_pages with the same UUID,
// preserved title/content, and a seed revision row. Personal book
// is auto-created.
func TestMigrateUser_HappyPath(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	noteID := seedNote(t, pool, userID, "Tire notes", "stock 225/65R17", false, false)

	res, err := MigrateUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("MigrateUser: %v", err)
	}
	if res.Migrated != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("counts wrong: %#v", res)
	}

	page, ok := loadPage(t, pool, noteID)
	if !ok {
		t.Fatalf("page with id=%s not found post-migration", noteID)
	}
	if page.ID != noteID {
		t.Errorf("id changed: got %q, want %q (id preservation is the idempotency contract)", page.ID, noteID)
	}
	if page.Title != "Tire notes" {
		t.Errorf("title = %q, want Tire notes", page.Title)
	}
	if page.Content != "stock 225/65R17" {
		t.Errorf("content lost in migration: %q", page.Content)
	}
	if got := countRevisions(t, pool, noteID); got != 1 {
		t.Errorf("expected 1 seed revision, got %d", got)
	}
}

// TestMigrateUser_PreservesTimestamps. The timestamps must come
// from the source notes row, not NOW(). Otherwise the user's
// "Last edited 3 weeks ago" indicator becomes "just now" for
// every note on migration day.
func TestMigrateUser_PreservesTimestamps(t *testing.T) {
	pool, userID := setupMigrationTest(t)

	// Set an old created_at + updated_at directly so we have known
	// values to assert.
	created := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2025, 6, 20, 9, 30, 0, 0, time.UTC)
	var noteID string
	err := pool.QueryRowContext(context.Background(), `
		INSERT INTO notes (user_id, title, content, created_at, updated_at)
		VALUES ($1, 'old note', 'body', $2, $3)
		RETURNING id::text`, userID, created, updated,
	).Scan(&noteID)
	if err != nil {
		t.Fatalf("seed dated note: %v", err)
	}

	if _, err := MigrateUser(context.Background(), pool, userID, Options{}); err != nil {
		t.Fatalf("MigrateUser: %v", err)
	}
	page, ok := loadPage(t, pool, noteID)
	if !ok {
		t.Fatalf("migrated page missing")
	}
	if !page.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v (timestamps must be preserved)", page.CreatedAt, created)
	}
	if !page.UpdatedAt.Equal(updated) {
		t.Errorf("updated_at = %v, want %v", page.UpdatedAt, updated)
	}
}

// TestMigrateUser_PinnedMaps: pinned notes land in user_page_prefs.
// The notes shape had pinned per-row; the pages shape has it
// per-user/per-page. For personal books (one member) the effect
// is identical, but the migration still has to write the pref row.
func TestMigrateUser_PinnedMaps(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	pinnedID := seedNote(t, pool, userID, "pin me", "p", true, false)
	plainID := seedNote(t, pool, userID, "ignore me", "x", false, false)

	if _, err := MigrateUser(context.Background(), pool, userID, Options{}); err != nil {
		t.Fatalf("MigrateUser: %v", err)
	}

	pinned, ok := loadPin(t, pool, userID, pinnedID)
	if !ok || !pinned {
		t.Errorf("pinned note didn't land in user_page_prefs (ok=%v, pinned=%v)", ok, pinned)
	}
	if _, ok := loadPin(t, pool, userID, plainID); ok {
		t.Errorf("non-pinned note should NOT have a user_page_prefs row")
	}
}

// TestMigrateUser_Idempotent: a second MigrateUser call must
// short-circuit on rows whose UUID is already present in
// wiki_pages. Re-runs are how operators recover from partial
// failures, and they must not re-insert.
func TestMigrateUser_Idempotent(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	for i := 0; i < 3; i++ {
		seedNote(t, pool, userID, fmt.Sprintf("note %d", i), "body", false, false)
	}

	first, err := MigrateUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("first MigrateUser: %v", err)
	}
	if first.Migrated != 3 || first.Skipped != 0 {
		t.Fatalf("first run wrong: %#v", first)
	}

	second, err := MigrateUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("second MigrateUser: %v", err)
	}
	if second.Migrated != 0 || second.Skipped != 3 || second.Failed != 0 {
		t.Errorf("idempotent re-run should skip all 3 (got %#v)", second)
	}

	// Confirm the page count didn't double.
	var pageCount int
	if err := pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM wiki_pages WHERE created_by = $1`, userID,
	).Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if pageCount != 3 {
		t.Errorf("re-run created duplicates: %d pages, want 3", pageCount)
	}
}

// TestMigrateUser_SlugCollision: two notes with identical titles
// should both migrate, getting unique slugs via the -2 / -3
// suffix path. Otherwise the unique index on (book_id, slug)
// rejects the second insert.
func TestMigrateUser_SlugCollision(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	a := seedNote(t, pool, userID, "Meeting notes", "first", false, false)
	b := seedNote(t, pool, userID, "Meeting notes", "second", false, false)
	c := seedNote(t, pool, userID, "Meeting notes", "third", false, false)

	res, err := MigrateUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("MigrateUser: %v", err)
	}
	if res.Migrated != 3 || res.Failed != 0 {
		t.Fatalf("collision handling failed: %#v", res)
	}
	pa, _ := loadPage(t, pool, a)
	pb, _ := loadPage(t, pool, b)
	pc, _ := loadPage(t, pool, c)
	slugs := map[string]bool{pa.Slug: true, pb.Slug: true, pc.Slug: true}
	if len(slugs) != 3 {
		t.Errorf("slugs collided: %v / %v / %v", pa.Slug, pb.Slug, pc.Slug)
	}
}

// TestMigrateUser_SkipsSoftDeletedByDefault: deleted_at IS NOT
// NULL rows shouldn't be resurrected as live pages unless the
// operator opts in.
func TestMigrateUser_SkipsSoftDeletedByDefault(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	live := seedNote(t, pool, userID, "live", "ok", false, false)
	dead := seedNote(t, pool, userID, "trashed", "x", false, true)

	res, err := MigrateUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("MigrateUser: %v", err)
	}
	if res.Migrated != 1 {
		t.Fatalf("expected 1 migrated (live only); got %#v", res)
	}
	if _, ok := loadPage(t, pool, live); !ok {
		t.Errorf("live note should have migrated")
	}
	if _, ok := loadPage(t, pool, dead); ok {
		t.Errorf("soft-deleted note should NOT have migrated by default")
	}
}

// TestMigrateUser_IncludeDeletedFolds: Options.IncludeDeleted=true
// brings soft-deleted rows along for the ride.
func TestMigrateUser_IncludeDeletedFolds(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	dead := seedNote(t, pool, userID, "trashed", "x", false, true)

	res, err := MigrateUser(context.Background(), pool, userID, Options{IncludeDeleted: true})
	if err != nil {
		t.Fatalf("MigrateUser: %v", err)
	}
	if res.Migrated != 1 {
		t.Fatalf("expected the deleted note to be migrated; got %#v", res)
	}
	if _, ok := loadPage(t, pool, dead); !ok {
		t.Errorf("--include-deleted should have migrated the soft-deleted note")
	}
}

// TestPlanUser_CountsCorrectly verifies the dry-run counts
// reflect what MigrateUser would actually do.
func TestPlanUser_CountsCorrectly(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	for i := 0; i < 5; i++ {
		seedNote(t, pool, userID, fmt.Sprintf("note %d", i), "body", false, false)
	}
	plan, err := PlanUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("PlanUser: %v", err)
	}
	if plan.NoteCount != 5 || plan.AlreadyMigrated != 0 || plan.ToMigrate != 5 {
		t.Errorf("plan wrong: %#v", plan)
	}

	// Migrate, then re-plan: AlreadyMigrated should now be 5,
	// ToMigrate should be 0.
	if _, err := MigrateUser(context.Background(), pool, userID, Options{}); err != nil {
		t.Fatalf("MigrateUser: %v", err)
	}
	plan2, err := PlanUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("PlanUser (post): %v", err)
	}
	if plan2.AlreadyMigrated != 5 || plan2.ToMigrate != 0 {
		t.Errorf("post-migration plan wrong: %#v", plan2)
	}
}

// TestVerifyUser_DetectsOrphans: seed notes, migrate only one,
// then verify; the un-migrated ones should appear in OrphanNoteIDs.
func TestVerifyUser_DetectsOrphans(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	a := seedNote(t, pool, userID, "a", "x", false, false)
	b := seedNote(t, pool, userID, "b", "y", false, false)

	// Force a half-migration: insert a page row for `a` directly,
	// leaving `b` orphaned in the notes table.
	bookID, err := ensurePersonalBookSQL(context.Background(), pool, userID)
	if err != nil {
		t.Fatalf("ensure personal: %v", err)
	}
	_, err = pool.ExecContext(context.Background(), `
		INSERT INTO wiki_pages (id, book_id, slug, title, content, created_by, updated_by, edit_count)
		VALUES ($1::uuid, $2::uuid, 'a', 'a', 'x', $3, $3, 1)`,
		a, bookID, userID)
	if err != nil {
		t.Fatalf("force-insert page: %v", err)
	}

	v, err := VerifyUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if v.Match {
		t.Fatalf("verify reported match when one orphan exists")
	}
	if v.NotesCount != 2 || v.MigratedCount != 1 {
		t.Errorf("counts wrong: %#v", v)
	}
	if len(v.OrphanNoteIDs) != 1 || v.OrphanNoteIDs[0] != b {
		t.Errorf("expected orphan list to contain %q, got %v", b, v.OrphanNoteIDs)
	}
}

// TestVerifyUser_HappyPath: after a clean migration, verify
// reports Match=true with no orphans.
func TestVerifyUser_HappyPath(t *testing.T) {
	pool, userID := setupMigrationTest(t)
	for i := 0; i < 4; i++ {
		seedNote(t, pool, userID, fmt.Sprintf("note %d", i), "body", false, false)
	}
	if _, err := MigrateUser(context.Background(), pool, userID, Options{}); err != nil {
		t.Fatalf("MigrateUser: %v", err)
	}
	v, err := VerifyUser(context.Background(), pool, userID, Options{})
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if !v.Match {
		t.Errorf("verify after clean migration should match: %#v", v)
	}
	if len(v.OrphanNoteIDs) != 0 {
		t.Errorf("orphan list should be empty: %v", v.OrphanNoteIDs)
	}
}

// TestMigrateUser_CrossUserIsolation: two users' notes must not
// bleed into each other's personal books. Caught a real bug class
// during initial development of the SQL.
func TestMigrateUser_CrossUserIsolation(t *testing.T) {
	pool, _ := setupMigrationTest(t)
	ctx := context.Background()
	for _, uid := range []string{"isolation-operator", "isolation-alison"} {
		_, err := pool.ExecContext(ctx, `
			INSERT INTO users (id, display_name, status)
			VALUES ($1, $1, 'approved')
			ON CONFLICT (id) DO NOTHING`, uid)
		if err != nil {
			t.Fatalf("seed user %s: %v", uid, err)
		}
	}
	operatorNote := seedNote(t, pool, "isolation-operator", "operator's note", "secret", false, false)
	alisonNote := seedNote(t, pool, "isolation-alison", "alison's note", "secret", false, false)

	if _, err := MigrateUser(ctx, pool, "isolation-operator", Options{}); err != nil {
		t.Fatalf("migrate operator: %v", err)
	}

	// Operator's note should be in his personal book; Alison's
	// should still be only in the notes table.
	if _, ok := loadPage(t, pool, operatorNote); !ok {
		t.Errorf("operator's note didn't migrate into his book")
	}
	if _, ok := loadPage(t, pool, alisonNote); ok {
		t.Errorf("alison's note leaked into wiki_pages despite only operator being migrated")
	}

	// Operator's personal book should NOT have alison's pages.
	var crossCount int
	err := pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM wiki_pages wp
		  JOIN books b ON b.id = wp.book_id
		 WHERE b.slug = 'personal:isolation-operator' AND wp.created_by = 'isolation-alison'`,
	).Scan(&crossCount)
	if err != nil {
		t.Fatalf("cross-count: %v", err)
	}
	if crossCount != 0 {
		t.Errorf("cross-user leak: %d alison pages in operator's book", crossCount)
	}
}
