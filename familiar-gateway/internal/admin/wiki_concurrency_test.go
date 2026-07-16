package admin

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testutil"
)

// wikiStoreForTest returns a WikiStore over a dedicated schema (like the
// other DB-gated suites) plus a seeded approved user to own pages.
func wikiStoreForTest(t *testing.T) (*WikiStore, string) {
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
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS wiki_conc_test`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := db.Open(dsn + sep + "options=" + url.QueryEscape("-csearch_path=wiki_conc_test,public"))
	if err != nil {
		t.Fatalf("db.Open (scoped): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role)
		VALUES ('wu', 'Wiki User', 'approved', 'user') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return NewWikiStore(pool), "wu"
}

func ptr(s string) *string { return &s }

// The grocery-list case, resolved the pleasant way. Two writers load the
// SAME base and each add a DIFFERENT item. The first wins outright; the
// second carries a now-stale precondition but its edit touches a different
// line, so the server three-way merges it against the base revision instead
// of rejecting — BOTH items survive and the second save is flagged Merged.
func TestUpdatePage_AutoMergesDisjointStaleWrite(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()

	book, err := s.CreateBook(ctx, user, "Groceries", "", "")
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	page, err := s.CreatePage(ctx, book.ID, user, "List", "- milk\n", "")
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}

	base := page.UpdatedAt // both "clients" loaded this version

	// Writer A adds eggs.
	a, err := s.UpdatePage(ctx, book.ID, page.Slug, user, PagePatch{
		Content: ptr("- milk\n- eggs\n"),
		IfMatch: &base,
	})
	if err != nil {
		t.Fatalf("writer A: %v", err)
	}
	if a.Merged {
		t.Error("writer A (fresh base) should not be flagged Merged")
	}
	if a.UpdatedAt.Equal(base) {
		t.Fatal("writer A did not bump updated_at")
	}

	// Writer B, still holding the ORIGINAL base, adds bread on a different
	// line. Auto-merge keeps A's eggs AND B's bread.
	b, err := s.UpdatePage(ctx, book.ID, page.Slug, user, PagePatch{
		Content: ptr("- milk\n- bread\n"),
		IfMatch: &base,
	})
	if err != nil {
		t.Fatalf("writer B: err = %v, want a clean auto-merge", err)
	}
	if !b.Merged {
		t.Error("writer B (stale, disjoint) should be flagged Merged")
	}

	// Both survived — in the merged response and re-read from the store.
	for _, want := range []string{"milk", "eggs", "bread"} {
		if !strings.Contains(b.Content, want) {
			t.Errorf("merged response lost %q: %q", want, b.Content)
		}
	}
	final, err := s.GetPage(ctx, book.ID, page.Slug)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	for _, want := range []string{"milk", "eggs", "bread"} {
		if !strings.Contains(final.Content, want) {
			t.Errorf("stored content lost %q: %q", want, final.Content)
		}
	}
}

// When two stale writers change the SAME line differently, there is no
// safe merge — the server must reject the second as stale rather than pick
// a winner, so the client can surface a keep-mine/take-theirs choice.
func TestUpdatePage_ConflictOnSameLineStaleWrite(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()

	book, err := s.CreateBook(ctx, user, "Groceries", "", "")
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	page, err := s.CreatePage(ctx, book.ID, user, "List", "- milk\n", "")
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}
	base := page.UpdatedAt

	if _, err := s.UpdatePage(ctx, book.ID, page.Slug, user, PagePatch{
		Content: ptr("- oat milk\n"),
		IfMatch: &base,
	}); err != nil {
		t.Fatalf("writer A: %v", err)
	}

	// Writer B changes the SAME line to something else from the stale base.
	_, err = s.UpdatePage(ctx, book.ID, page.Slug, user, PagePatch{
		Content: ptr("- soy milk\n"),
		IfMatch: &base,
	})
	if !errors.Is(err, ErrPageStale) {
		t.Fatalf("writer B: err = %v, want ErrPageStale (same-line divergence must not auto-merge)", err)
	}

	// A's edit stands untouched.
	final, err := s.GetPage(ctx, book.ID, page.Slug)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if !strings.Contains(final.Content, "oat milk") || strings.Contains(final.Content, "soy milk") {
		t.Errorf("conflict resolution clobbered A: %q", final.Content)
	}
}

// A stale precondition on a TITLE edit is never auto-merged — titles aren't
// line-mergeable — so it must be rejected as stale.
func TestUpdatePage_TitleEditStaleRejected(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()

	book, err := s.CreateBook(ctx, user, "Groceries", "", "")
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	page, err := s.CreatePage(ctx, book.ID, user, "List", "- milk\n", "")
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}
	base := page.UpdatedAt

	// Someone else edits the body, moving the base.
	if _, err := s.UpdatePage(ctx, book.ID, page.Slug, user, PagePatch{
		Content: ptr("- milk\n- eggs\n"),
		IfMatch: &base,
	}); err != nil {
		t.Fatalf("first writer: %v", err)
	}

	// A title rename holding the stale base must not auto-merge.
	_, err = s.UpdatePage(ctx, book.ID, page.Slug, user, PagePatch{
		Title:   ptr("Shopping List"),
		IfMatch: &base,
	})
	if !errors.Is(err, ErrPageStale) {
		t.Fatalf("stale title edit: err = %v, want ErrPageStale", err)
	}
}

// Two appends racing on the same page must BOTH survive — the exact
// case where the old GetPage+compose+UpdatePage lost one. AppendPage's
// single atomic in-DB concatenation (serialized by row locks) keeps
// both. Runs them concurrently to exercise the interleaving.
func TestAppendPage_ConcurrentAppendsBothSurvive(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()

	book, err := s.CreateBook(ctx, user, "Groceries", "", "")
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	page, err := s.CreatePage(ctx, book.ID, user, "List", "- milk", "")
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, text := range []string{"- eggs", "- bread"} {
		wg.Add(1)
		go func(i int, text string) {
			defer wg.Done()
			_, errs[i] = s.AppendPage(ctx, book.ID, page.ID, user, text)
		}(i, text)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatalf("concurrent append: %v", e)
		}
	}

	final, err := s.GetPage(ctx, book.ID, page.Slug)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	for _, want := range []string{"milk", "eggs", "bread"} {
		if !strings.Contains(final.Content, want) {
			t.Errorf("append lost %q — final content: %q", want, final.Content)
		}
	}
}

// EnsureResearchBook is per-user and idempotent: two different users
// each get their OWN research book (research:{userID}), so concurrent
// users never collide on a shared "research" slug — the multi-user
// break that shipped in the first cut. The book is also hidden from
// every listing and system-managed (no rename/archive).
func TestEnsureResearchBook_PerUserAndHidden(t *testing.T) {
	store, user := wikiStoreForTest(t)
	ctx := context.Background()

	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role)
		VALUES ('wu2', 'Wiki User 2', 'approved', 'user') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed second user: %v", err)
	}

	a1, err := store.EnsureResearchBook(ctx, user)
	if err != nil {
		t.Fatalf("EnsureResearchBook(user): %v", err)
	}
	if a1.Slug != "research:"+user {
		t.Errorf("slug = %q, want research:%s", a1.Slug, user)
	}
	// Idempotent — same book back, no fork.
	a2, err := store.EnsureResearchBook(ctx, user)
	if err != nil || a2.ID != a1.ID {
		t.Fatalf("second ensure returned a different book: %v (%s vs %s)", err, a2.ID, a1.ID)
	}
	// A different user gets a DISTINCT book (the whole point).
	b1, err := store.EnsureResearchBook(ctx, "wu2")
	if err != nil {
		t.Fatalf("EnsureResearchBook(wu2): %v", err)
	}
	if b1.ID == a1.ID {
		t.Fatal("two users share one research book — the multi-user break is back")
	}

	// Hidden from BOTH listings (plain and include-personal).
	for _, withPersonal := range []bool{false, true} {
		books, err := store.listBooksFiltered(ctx, user, true, withPersonal)
		if err != nil {
			t.Fatalf("list (personal=%v): %v", withPersonal, err)
		}
		for _, bk := range books {
			if strings.HasPrefix(bk.Slug, "research:") {
				t.Errorf("research book leaked into listing (personal=%v): %s", withPersonal, bk.Slug)
			}
		}
	}

	// System-managed: no rename, no archive.
	if _, err := store.UpdateBook(ctx, a1.Slug, user, false, BookPatch{Name: ptr("Renamed")}); err == nil {
		t.Error("research book was renamable")
	}
	arch := true
	if _, err := store.UpdateBook(ctx, a1.Slug, user, false, BookPatch{Archive: &arch}); err == nil {
		t.Error("research book was archivable")
	}

	// CreateBook can't reach the system slug: slugify maps ':' → '-',
	// so a "research:foo" request becomes a DISTINCT, visible book that
	// neither collides with nor is hidden as the system research book.
	vis, err := store.CreateBook(ctx, user, "Research Notes", "", "research:foo")
	if err != nil {
		t.Fatalf("CreateBook(research:foo): %v", err)
	}
	if strings.Contains(vis.Slug, ":") {
		t.Errorf("user book slug kept a colon: %q", vis.Slug)
	}
	books, err := store.listBooksFiltered(ctx, user, true, false)
	if err != nil {
		t.Fatalf("list after create: %v", err)
	}
	var sawVisible bool
	for _, bk := range books {
		if bk.ID == vis.ID {
			sawVisible = true
		}
	}
	if !sawVisible {
		t.Error("a user's research-named book was wrongly hidden by the research: filter")
	}
}

// SweepResearchEvidence reaps hidden research-book pages older than the
// retention window while leaving fresh ones and pages in ordinary books
// untouched — the only bound on the hidden books' growth (§6.6).
func TestSweepResearchEvidence(t *testing.T) {
	store, user := wikiStoreForTest(t)
	ctx := context.Background()

	rb, err := store.EnsureResearchBook(ctx, user)
	if err != nil {
		t.Fatalf("EnsureResearchBook: %v", err)
	}
	old, err := store.CreatePage(ctx, rb.ID, user, "Old Evidence", "stale", "")
	if err != nil {
		t.Fatalf("create old page: %v", err)
	}
	fresh, err := store.CreatePage(ctx, rb.ID, user, "Fresh Evidence", "recent", "")
	if err != nil {
		t.Fatalf("create fresh page: %v", err)
	}
	// Age the "old" page past the window.
	if _, err := store.db.ExecContext(ctx,
		`UPDATE wiki_pages SET updated_at = NOW() - INTERVAL '10 days' WHERE id = $1::uuid`, old.ID); err != nil {
		t.Fatalf("age old page: %v", err)
	}
	// A page in a NORMAL book must never be swept.
	nb, err := store.CreateBook(ctx, user, "Normal", "", "")
	if err != nil {
		t.Fatalf("create normal book: %v", err)
	}
	normal, err := store.CreatePage(ctx, nb.ID, user, "Keep Me", "important", "")
	if err != nil {
		t.Fatalf("create normal page: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE wiki_pages SET updated_at = NOW() - INTERVAL '10 days' WHERE id = $1::uuid`, normal.ID); err != nil {
		t.Fatalf("age normal page: %v", err)
	}

	n, err := store.SweepResearchEvidence(ctx, 72*time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("sweep reaped %d pages, want exactly 1 (the aged research page)", n)
	}
	// The old research page is gone; the fresh one and the normal-book
	// page survive.
	if _, err := store.GetPage(ctx, rb.ID, old.Slug); !errors.Is(err, ErrPageNotFound) {
		t.Errorf("aged research page survived the sweep: %v", err)
	}
	if _, err := store.GetPage(ctx, rb.ID, fresh.Slug); err != nil {
		t.Errorf("fresh research page was wrongly swept: %v", err)
	}
	if _, err := store.GetPage(ctx, nb.ID, normal.Slug); err != nil {
		t.Errorf("normal-book page was swept: %v", err)
	}

	// Disabled (retention ≤ 0) is a no-op.
	if got, err := store.SweepResearchEvidence(ctx, 0); err != nil || got != 0 {
		t.Errorf("disabled sweep = (%d, %v), want (0, nil)", got, err)
	}
}

// ListMemberBookIDs returns hidden books (personal + research) too —
// the page-events SSE gates on membership, and the live evidence view
// depends on the owner receiving events for their hidden research book.
func TestListMemberBookIDs_IncludesHidden(t *testing.T) {
	store, user := wikiStoreForTest(t)
	ctx := context.Background()

	personal, err := store.EnsurePersonalBook(ctx, user)
	if err != nil {
		t.Fatalf("EnsurePersonalBook: %v", err)
	}
	research, err := store.EnsureResearchBook(ctx, user)
	if err != nil {
		t.Fatalf("EnsureResearchBook: %v", err)
	}
	normal, err := store.CreateBook(ctx, user, "Normal", "", "")
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	ids, err := store.ListMemberBookIDs(ctx, user)
	if err != nil {
		t.Fatalf("ListMemberBookIDs: %v", err)
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for _, want := range []struct{ name, id string }{
		{"personal", personal.ID}, {"research", research.ID}, {"normal", normal.ID},
	} {
		if !set[want.id] {
			t.Errorf("member ids missing the %s book (%s) — SSE would drop its events", want.name, want.id)
		}
	}
}
