package wikiknowledge

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testutil"
)

// PgReindexStore against a private, migrated schema (DB-gated).
func reindexStoreForTest(t *testing.T) *PgReindexStore {
	t.Helper()
	dsn := os.Getenv(testutil.EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s not set", testutil.EnvDSN)
	}
	ctx := context.Background()
	adminPool, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = adminPool.Close() })
	schema := "wiki_reindex_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	if _, err := adminPool.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = adminPool.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := db.Open(dsn + sep + "options=" + url.QueryEscape("-csearch_path="+schema+",public"))
	if err != nil {
		t.Fatalf("db.Open (scoped): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return &PgReindexStore{DB: pool}
}

type wikiSeed struct {
	s    *PgReindexStore
	t    *testing.T
	user string
}

func (w wikiSeed) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.s.DB.ExecContext(context.Background(), q, args...); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
}

func (w wikiSeed) book(slug string) string {
	w.t.Helper()
	var id string
	if err := w.s.DB.QueryRowContext(context.Background(),
		`INSERT INTO books (slug, name, created_by) VALUES ($1, $1, $2) RETURNING id::text`, slug, w.user).Scan(&id); err != nil {
		w.t.Fatalf("book %s: %v", slug, err)
	}
	return id
}

// page adds a page last saved `age` before now. The job's start is
// pinned an hour ago, so an age under an hour means "saved since".
func (w wikiSeed) page(book, slug string, age time.Duration, deleted bool) string {
	w.t.Helper()
	var id string
	if err := w.s.DB.QueryRowContext(context.Background(), `
		INSERT INTO wiki_pages (book_id, slug, title, content, created_by, updated_by, updated_at, deleted_at)
		VALUES ($1::uuid, $2, $2, 'content of '||$2, $3, $3, NOW() - $4::interval, CASE WHEN $5 THEN NOW() END)
		RETURNING id::text`,
		book, slug, w.user, fmtInterval(age), deleted).Scan(&id); err != nil {
		w.t.Fatalf("page %s: %v", slug, err)
	}
	return id
}

func fmtInterval(d time.Duration) string { return d.String() }

// Pending is the live, non-research pages saved before the re-key that
// aren't done or given up on, most recently edited first.
func TestPgReindexStore_PendingPages(t *testing.T) {
	s := reindexStoreForTest(t)
	w := wikiSeed{s, t, "reindex-user"}
	w.exec(`INSERT INTO users (id, display_name, status) VALUES ($1, 'R', 'approved')`, w.user)
	if err := s.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.exec(`UPDATE applied_data_fixes SET applied_at = NOW() - interval '1 hour' WHERE name = $1`, reindexStarted)
	// A resumed job keeps its original start.
	if err := s.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	notes := w.book("notes")
	research := w.book("research:reindex-user")

	older := w.page(notes, "older", 3*time.Hour, false)
	newer := w.page(notes, "newer", 2*time.Hour, false)
	retrying := w.page(notes, "retrying", 4*time.Hour, false)
	w.page(notes, "saved-since", 10*time.Minute, false) // after the job started: its save ingested it
	w.page(notes, "deleted", 3*time.Hour, true)
	w.page(research, "evidence", 3*time.Hour, false)
	done := w.page(notes, "done", 3*time.Hour, false)
	gaveUp := w.page(notes, "gave-up", 3*time.Hour, false)

	ctx := context.Background()
	if err := s.MarkDone(ctx, done); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPageAttempts; i++ {
		if err := s.RecordFailure(ctx, gaveUp, "boom"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordFailure(ctx, retrying, "boom"); err != nil {
		t.Fatal(err)
	}

	ids, err := s.PendingPages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{newer, older, retrying}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("pending = %v, want %v (newer, older, retrying)", ids, want)
	}

	// Finish records the job done and reports the pages given up on.
	if d, _ := s.Done(ctx); d {
		t.Fatal("done before Finish")
	}
	n, err := s.Finish(ctx)
	if err != nil || n != 2 { // gave-up, plus retrying (still not done)
		t.Errorf("Finish = %d, %v; want 2 pages not done", n, err)
	}
	if d, _ := s.Done(ctx); !d {
		t.Error("Finish didn't mark the job done")
	}
}

// LoadPage reads what a save would pass: current content, the last
// editor as the owner of the facts, and the page's links.
func TestPgReindexStore_LoadPage(t *testing.T) {
	s := reindexStoreForTest(t)
	w := wikiSeed{s, t, "reindex-user"}
	w.exec(`INSERT INTO users (id, display_name, status) VALUES ($1, 'R', 'approved'), ('editor', 'E', 'approved')`, w.user)
	book := w.book("notes")
	id := w.page(book, "biopsy", time.Hour, false)
	w.exec(`UPDATE wiki_pages SET updated_by = 'editor', content = 'The biopsy is on Monday.' WHERE id = $1::uuid`, id)
	target := "t-1"
	s.Links = func(_ context.Context, pageID string) ([]admin.PageLink, error) {
		return []admin.PageLink{{SourcePageID: pageID, TargetPageSlug: "clinic", TargetPageID: &target}}, nil
	}
	evt, ok, err := s.LoadPage(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("LoadPage: ok=%v err=%v", ok, err)
	}
	if evt.BookID != book || evt.BookSlug != "notes" || evt.PageSlug != "biopsy" || evt.UserID != "editor" ||
		evt.Content != "The biopsy is on Monday." || len(evt.Links) != 1 {
		t.Errorf("LoadPage = %+v", evt)
	}
	w.exec(`UPDATE wiki_pages SET deleted_at = NOW() WHERE id = $1::uuid`, id)
	if _, ok, err := s.LoadPage(context.Background(), id); ok || err != nil {
		t.Errorf("a deleted page loaded: ok=%v err=%v", ok, err)
	}
}

// Only one gateway runs the job at a time.
func TestPgReindexStore_Lock(t *testing.T) {
	s := reindexStoreForTest(t)
	ctx := context.Background()
	release, ok, err := s.Lock(ctx)
	if err != nil || !ok {
		t.Fatalf("first Lock: ok=%v err=%v", ok, err)
	}
	if _, ok2, err := s.Lock(ctx); err != nil || ok2 {
		t.Errorf("second Lock while held: ok=%v err=%v, want refused", ok2, err)
	}
	release()
	release2, ok3, err := s.Lock(ctx)
	if err != nil || !ok3 {
		t.Fatalf("Lock after release: ok=%v err=%v", ok3, err)
	}
	release2()
}
