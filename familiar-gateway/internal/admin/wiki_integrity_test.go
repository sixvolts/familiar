package admin

// Store tests for the page tree, links, revisions, membership and share
// fixes. The concurrency cases are deterministic: a test transaction
// holds the lock (or an uncommitted row) the code under test must wait
// for, and the test checks it waited, then lets it finish.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func seedWikiUsers(t *testing.T, s *WikiStore, ids ...string) {
	t.Helper()
	for _, id := range ids {
		role := "user"
		if strings.HasPrefix(id, "admin") {
			role = "admin"
		}
		if _, err := s.db.ExecContext(context.Background(), `
			INSERT INTO users (id, display_name, status, role)
			VALUES ($1, $1, 'approved', $2) ON CONFLICT (id) DO NOTHING`, id, role); err != nil {
			t.Fatalf("seed user %s: %v", id, err)
		}
	}
}

// finishesWithin runs fn in the background and reports whether it
// returned within d; the caller gets its error once it does.
func startBlocked(fn func() error) (done <-chan error) {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	return ch
}

func assertWaiting(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s didn't wait for the lock (returned %v)", what, err)
	case <-time.After(300 * time.Millisecond):
	}
}

func awaitDone(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never finished", what)
		return nil
	}
}

func mustPage(t *testing.T, s *WikiStore, bookID, user, title, content string) *WikiPage {
	t.Helper()
	p, err := s.CreatePage(context.Background(), bookID, user, title, content, "")
	if err != nil {
		t.Fatalf("CreatePage %q: %v", title, err)
	}
	return p
}

func mustMove(t *testing.T, s *WikiStore, bookID, pageID, parentID string) {
	t.Helper()
	if _, err := s.MovePage(context.Background(), bookID, pageID, parentID, nil); err != nil {
		t.Fatalf("MovePage: %v", err)
	}
}

func parentOf(t *testing.T, s *WikiStore, bookID, pageID string) string {
	t.Helper()
	p, err := s.GetPageByID(context.Background(), bookID, pageID)
	if err != nil {
		t.Fatalf("GetPageByID: %v", err)
	}
	if p.ParentID == nil {
		return ""
	}
	return *p.ParentID
}

// Deleting a page moves its children up to its parent. They kept
// pointing at the deleted page, and the sidebar trees never reached
// them.
func TestDeletePage_ChildrenMoveUpToItsParent(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Tree", "", "")
	if err != nil {
		t.Fatal(err)
	}
	root := mustPage(t, s, book.ID, user, "Projects", "")
	mid := mustPage(t, s, book.ID, user, "Garden", "")
	kid1 := mustPage(t, s, book.ID, user, "Beds", "")
	kid2 := mustPage(t, s, book.ID, user, "Seeds", "")
	mustMove(t, s, book.ID, mid.ID, root.ID)
	mustMove(t, s, book.ID, kid1.ID, mid.ID)
	mustMove(t, s, book.ID, kid2.ID, mid.ID)

	if err := s.DeletePageByID(ctx, book.ID, mid.ID); err != nil {
		t.Fatal(err)
	}
	for _, k := range []*WikiPage{kid1, kid2} {
		if got := parentOf(t, s, book.ID, k.ID); got != root.ID {
			t.Errorf("%s's parent = %q after its parent's delete, want the grandparent %q", k.Title, got, root.ID)
		}
	}
	// A top-level page's children become top-level.
	if err := s.DeletePage(ctx, book.ID, root.Slug); err != nil {
		t.Fatal(err)
	}
	if got := parentOf(t, s, book.ID, kid1.ID); got != "" {
		t.Errorf("parent = %q after the top-level parent's delete, want top level", got)
	}
}

// Title-style links resolve: [[French Toast]] is the page slugged
// "french-toast", or titled so; a link written before its page existed
// resolves when the page is created or renamed to match, and breaks
// when it's deleted.
func TestPageLinks_TitleLinksResolve(t *testing.T) {
	got := ParseLinks("[[French Toast]] [[Recipes/Pancake Day|pd]] [[https://x.test/a]] [[Kitchen\\Big Pot]] [[personal:u1/Some Page]]")
	want := []ParsedLink{
		{TargetPageSlug: "french-toast"},
		{TargetBookSlug: "recipes", TargetPageSlug: "pancake-day", DisplayText: "pd"},
		{TargetBookSlug: "kitchen", TargetPageSlug: "big-pot"},
		{TargetBookSlug: "personal:u1", TargetPageSlug: "some-page"},
	}
	if len(got) != len(want) {
		t.Fatalf("ParseLinks = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Recipes", "", "")
	if err != nil {
		t.Fatal(err)
	}
	src := mustPage(t, s, book.ID, user, "Breakfast", "Try [[French Toast]] and [[Waffles]].")
	target := func(slug string) *string {
		var id *string
		if err := s.db.QueryRowContext(ctx, `
			SELECT target_page_id::text FROM wiki_page_links
			 WHERE source_page_id = $1::uuid AND target_page_slug = $2`, src.ID, slug).Scan(&id); err != nil {
			t.Fatalf("link %s: %v", slug, err)
		}
		return id
	}
	if target("french-toast") != nil {
		t.Fatal("a link to a missing page resolved")
	}
	toast := mustPage(t, s, book.ID, user, "French Toast", "")
	if id := target("french-toast"); id == nil || *id != toast.ID {
		t.Errorf("link didn't resolve when its page was created: %v", id)
	}
	// A page renamed to match: its slug isn't "waffles" (explicit), so
	// only the title fallback finds it.
	w, err := s.CreatePage(ctx, book.ID, user, "Untitled", "", "untitled-4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdatePageByID(ctx, book.ID, w.ID, user, PagePatch{Title: ptr("Waffles"), Slug: ptr("untitled-4")}); err != nil {
		t.Fatal(err)
	}
	if id := target("waffles"); id == nil || *id != w.ID {
		t.Errorf("link didn't resolve to the page renamed to match: %v", id)
	}
	// And a fresh index of the source resolves by title too.
	if _, err := s.AppendPage(ctx, book.ID, src.ID, user, "More."); err != nil {
		t.Fatal(err)
	}
	if id := target("waffles"); id == nil || *id != w.ID {
		t.Errorf("re-index didn't resolve by title: %v", id)
	}
	if err := s.DeletePageByID(ctx, book.ID, toast.ID); err != nil {
		t.Fatal(err)
	}
	if target("french-toast") != nil {
		t.Error("a link to a deleted page stayed resolved")
	}
}

// A save based on a version made by a rename (or a move) merges. Those
// bump updated_at without a revision, and the merge needed a revision
// at exactly the If-Match time: such a save always conflicted.
func TestUpdatePage_MergesFromARenamedVersion(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Lists", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := mustPage(t, s, book.ID, user, "Groceries", "- milk\n")
	renamed, err := s.UpdatePageByID(ctx, book.ID, p.ID, user, PagePatch{Title: ptr("Shopping")})
	if err != nil {
		t.Fatal(err)
	}
	// Loaded after the rename; meanwhile someone adds bread.
	loaded := renamed.UpdatedAt
	if _, err := s.UpdatePageByID(ctx, book.ID, p.ID, user, PagePatch{Content: ptr("- milk\n- bread\n"), IfMatch: &renamed.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	out, err := s.UpdatePageByID(ctx, book.ID, p.ID, user, PagePatch{Content: ptr("- eggs\n- milk\n"), IfMatch: &loaded})
	if err != nil {
		t.Fatalf("save from the renamed version: %v", err)
	}
	if out.Content != "- eggs\n- milk\n- bread\n" || !out.Merged {
		t.Errorf("merged = %q (merged=%v)", out.Content, out.Merged)
	}

	// Same after a move.
	parent := mustPage(t, s, book.ID, user, "Home", "")
	moved, err := s.MovePage(ctx, book.ID, p.ID, parent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdatePageByID(ctx, book.ID, p.ID, user, PagePatch{Content: ptr("- eggs\n- milk\n- bread\n- jam\n"), IfMatch: &moved.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	out, err = s.UpdatePageByID(ctx, book.ID, p.ID, user, PagePatch{Content: ptr("- tea\n- eggs\n- milk\n- bread\n"), IfMatch: &moved.UpdatedAt})
	if err != nil {
		t.Fatalf("save from the moved version: %v", err)
	}
	if out.Content != "- tea\n- eggs\n- milk\n- bread\n- jam\n" {
		t.Errorf("merged = %q", out.Content)
	}
}

// Moves are serialized per book: a move waits while another holds the
// book's tree lock. Without it two moves (A under B, B under A) each
// passed the cycle check and together made a loop.
func TestMovePage_WaitsForTheTreeLock(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Moves", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := mustPage(t, s, book.ID, user, "A", "")
	b := mustPage(t, s, book.ID, user, "B", "")

	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := lockBookTree(ctx, tx, book.ID); err != nil {
		t.Fatal(err)
	}
	// While holding it, make B a child of A, as a racing move would.
	if _, err := tx.ExecContext(ctx, `UPDATE wiki_pages SET parent_id = $1::uuid WHERE id = $2::uuid`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	done := startBlocked(func() error {
		_, err := s.MovePage(ctx, book.ID, a.ID, b.ID, nil) // A under B: a cycle once B is under A
		return err
	})
	assertWaiting(t, done, "MovePage")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := awaitDone(t, done, "MovePage"); !errors.Is(err, ErrInvalidParent) {
		t.Errorf("A under B after B under A committed: %v, want ErrInvalidParent", err)
	}
}

// Deletes take the same lock (they re-parent children).
func TestDeletePage_WaitsForTheTreeLock(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Deletes", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := mustPage(t, s, book.ID, user, "Doomed", "")
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := lockBookTree(ctx, tx, book.ID); err != nil {
		t.Fatal(err)
	}
	done := startBlocked(func() error { return s.DeletePageByID(ctx, book.ID, p.ID) })
	assertWaiting(t, done, "DeletePageByID")
	_ = tx.Commit()
	if err := awaitDone(t, done, "DeletePageByID"); err != nil {
		t.Fatal(err)
	}
}

// Role changes are serialized per book, so the last-owner guard holds:
// two owners demoting each other at once both saw two owners and left
// none.
func TestMembers_RoleChangesWaitForTheRosterLock(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	seedWikiUsers(t, s, "co-owner")
	book, err := s.CreateBook(ctx, user, "Household", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, user, book.ID, "co-owner", "owner"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := lockBookRoster(ctx, tx, book.ID); err != nil {
		t.Fatal(err)
	}
	// The other demotion, committed while the lock is held.
	if _, err := tx.ExecContext(ctx, `UPDATE book_members SET role = 'writer' WHERE book_id = $1::uuid AND user_id = 'co-owner'`, book.ID); err != nil {
		t.Fatal(err)
	}
	done := startBlocked(func() error {
		_, err := s.ChangeMemberRole(ctx, "co-owner", book.ID, user, "writer")
		return err
	})
	assertWaiting(t, done, "ChangeMemberRole")
	_ = tx.Commit()
	if err := awaitDone(t, done, "ChangeMemberRole"); err == nil || !strings.Contains(err.Error(), "last owner") {
		t.Errorf("demoting the last owner: %v, want refused", err)
	}
	if role, _ := s.MemberRole(ctx, book.ID, user); role != "owner" {
		t.Errorf("the last owner is now %q", role)
	}

	// Removals take the lock too.
	tx2, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	if err := lockBookRoster(ctx, tx2, book.ID); err != nil {
		t.Fatal(err)
	}
	done = startBlocked(func() error { return s.RemoveMember(ctx, user, book.ID, "co-owner") })
	assertWaiting(t, done, "RemoveMember")
	_ = tx2.Commit()
	if err := awaitDone(t, done, "RemoveMember"); err != nil {
		t.Fatal(err)
	}
}

// A role change needs a role and a member: routed through AddMember,
// an empty role reset the member to writer, and a non-member was
// added.
func TestChangeMemberRole_NeedsARoleAndAMember(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	seedWikiUsers(t, s, "reader-1", "stranger")
	book, err := s.CreateBook(ctx, user, "Club", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, user, book.ID, "reader-1", "reader"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeMemberRole(ctx, user, book.ID, "reader-1", ""); err == nil {
		t.Error("an empty role was accepted")
	}
	if role, _ := s.MemberRole(ctx, book.ID, "reader-1"); role != "reader" {
		t.Errorf("reader is now %q", role)
	}
	if _, err := s.ChangeMemberRole(ctx, user, book.ID, "stranger", "writer"); !errors.Is(err, ErrMemberNotFound) {
		t.Errorf("non-member: %v, want ErrMemberNotFound", err)
	}
	if role, _ := s.MemberRole(ctx, book.ID, "stranger"); role != "" {
		t.Errorf("a role change added a non-member as %q", role)
	}
}

// Books named "Personal …" can be created; only the bare slug is the
// personal-book alias.
func TestCreateBook_PersonalNames(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	b, err := s.CreateBook(ctx, user, "Personal Finance", "", "")
	if err != nil {
		t.Fatalf("Personal Finance: %v", err)
	}
	if b.Slug != "personal-finance" {
		t.Errorf("slug = %q", b.Slug)
	}
	b, err = s.CreateBook(ctx, user, "Personal", "", "")
	if err != nil {
		t.Fatalf("Personal: %v", err)
	}
	if b.Slug == "personal" || !strings.HasPrefix(b.Slug, "personal-book") {
		t.Errorf("slug = %q, want a personal-book slug, never the alias", b.Slug)
	}
}

// Writes by id stay on that page. Each retry re-resolved the slug, with
// GetPage's title fallback: once the page was renamed, a write meant
// for it went to another page whose title matched the old slug.
func TestUpdatePageByID_StaysOnThePage(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Home", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := mustPage(t, s, book.ID, user, "Groceries", "a")
	b, err := s.CreatePage(ctx, book.ID, user, "Groceries", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.Slug == "groceries" {
		t.Fatal("second page took the first's slug")
	}
	if _, err := s.UpdatePageByID(ctx, book.ID, a.ID, user, PagePatch{Title: ptr("Shopping")}); err != nil {
		t.Fatal(err)
	}
	// By the old slug, the title fallback finds B now.
	if got, _ := s.GetPage(ctx, book.ID, "groceries"); got == nil || got.ID != b.ID {
		t.Fatalf("setup: GetPage(groceries) = %+v", got)
	}
	out, err := s.UpdatePageByID(ctx, book.ID, a.ID, user, PagePatch{Title: ptr("Weekly shop")})
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != a.ID {
		t.Errorf("the write went to page %s, want %s", out.ID, a.ID)
	}
	if got, _ := s.GetPageByID(ctx, book.ID, b.ID); got.Title != "Groceries" {
		t.Errorf("the other page was renamed to %q", got.Title)
	}
}

// A slug taken between uniquePageSlug's check and the write is retried
// with another, for creates and renames. It failed with a raw
// duplicate-key error (a double-clicked "New page", a save lost to a
// racing rename).
func TestPageSlugs_RetryWhenTakenMeanwhile(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Racy", "", "")
	if err != nil {
		t.Fatal(err)
	}
	hold := func(slug string) func() {
		tx, err := s.db.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO wiki_pages (book_id, slug, title, content, created_by, updated_by)
			VALUES ($1::uuid, $2, 'held', '', $3, $3)`, book.ID, slug, user); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}

	commit := hold("minutes")
	var created *WikiPage
	done := startBlocked(func() error {
		var err error
		created, err = s.CreatePage(ctx, book.ID, user, "Minutes", "", "")
		return err
	})
	assertWaiting(t, done, "CreatePage")
	commit()
	if err := awaitDone(t, done, "CreatePage"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Slug == "minutes" || !strings.HasPrefix(created.Slug, "minutes-") {
		t.Errorf("created slug = %q", created.Slug)
	}

	p := mustPage(t, s, book.ID, user, "Draft", "")
	commit = hold("agenda")
	var renamed *WikiPage
	done = startBlocked(func() error {
		var err error
		renamed, err = s.UpdatePageByID(ctx, book.ID, p.ID, user, PagePatch{Title: ptr("Agenda")})
		return err
	})
	assertWaiting(t, done, "rename")
	commit()
	if err := awaitDone(t, done, "rename"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Slug == "agenda" || !strings.HasPrefix(renamed.Slug, "agenda-") {
		t.Errorf("renamed slug = %q", renamed.Slug)
	}
}

// Revisions list a page at a time, newest first, without content.
func TestListRevisions_PagesWithoutContent(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "History", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := mustPage(t, s, book.ID, user, "Log", "v0")
	cur := p
	for _, c := range []string{"v1", "v2", "v3", "v4"} {
		cur, err = s.UpdatePageByID(ctx, book.ID, p.ID, user, PagePatch{Content: ptr(c), IfMatch: &cur.UpdatedAt})
		if err != nil {
			t.Fatal(err)
		}
	}
	var seen []int
	var before *time.Time
	for page := 0; page < 5; page++ {
		revs, err := s.ListRevisions(ctx, p.ID, before, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range revs {
			seen = append(seen, r.Size)
		}
		if len(revs) < 2 {
			break
		}
		t0 := revs[len(revs)-1].CreatedAt
		before = &t0
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d revisions, want 5", len(seen))
	}
	for _, n := range seen {
		if n != 2 {
			t.Errorf("sizes = %v, want 2 each", seen)
			break
		}
	}
}

// A share serves only while its creator can still write the book: a
// removed (or demoted) writer's links kept serving the live page.
func TestLookupSharedPage_CreatorMustStillWrite(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	seedWikiUsers(t, s, "writer-x", "admin-z")
	book, err := s.CreateBook(ctx, user, "Family", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, user, book.ID, "writer-x", "writer"); err != nil {
		t.Fatal(err)
	}
	mine := mustPage(t, s, book.ID, user, "Owner's", "")
	theirs := mustPage(t, s, book.ID, user, "Writer's", "")
	admins := mustPage(t, s, book.ID, user, "Admin's", "")
	shOwner, _ := s.EnablePageShare(ctx, mine.ID, user)
	shWriter, _ := s.EnablePageShare(ctx, theirs.ID, "writer-x")
	shAdmin, _ := s.EnablePageShare(ctx, admins.ID, "admin-z")
	for _, sh := range []*PageShare{shOwner, shWriter, shAdmin} {
		if _, err := s.LookupSharedPage(ctx, sh.ShareKey); err != nil {
			t.Fatalf("share %s before: %v", sh.ShareKey, err)
		}
	}
	if _, err := s.ChangeMemberRole(ctx, user, book.ID, "writer-x", "reader"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSharedPage(ctx, shWriter.ShareKey); !errors.Is(err, ErrPageNotFound) {
		t.Errorf("demoted writer's share: %v, want not found", err)
	}
	if err := s.RemoveMember(ctx, user, book.ID, "writer-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSharedPage(ctx, shWriter.ShareKey); !errors.Is(err, ErrPageNotFound) {
		t.Errorf("removed writer's share: %v, want not found", err)
	}
	for _, sh := range []*PageShare{shOwner, shAdmin} {
		if _, err := s.LookupSharedPage(ctx, sh.ShareKey); err != nil {
			t.Errorf("share %s after: %v", sh.ShareKey, err)
		}
	}
}

// Two enables at once return the one share: the second insert failed
// on the one-public-share index (a 500 for a double click).
func TestEnablePageShare_ConcurrentEnable(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Shared", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := mustPage(t, s, book.ID, user, "Public", "")
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO wiki_page_shares (share_key, page_id, visibility, created_by)
		VALUES ('AAAAAAAAAAAAAAAA', $1::uuid, 'public', $2)`, p.ID, user); err != nil {
		t.Fatal(err)
	}
	var got *PageShare
	done := startBlocked(func() error {
		var err error
		got, err = s.EnablePageShare(ctx, p.ID, user)
		return err
	})
	assertWaiting(t, done, "EnablePageShare")
	_ = tx.Commit()
	if err := awaitDone(t, done, "EnablePageShare"); err != nil {
		t.Fatalf("second enable: %v", err)
	}
	if got.ShareKey != "AAAAAAAAAAAAAAAA" {
		t.Errorf("second enable returned %q, want the first's share", got.ShareKey)
	}
}

// The opt-in purge hard-deletes pages deleted long enough ago, with
// their history and shares, and leaves recent deletes and live pages.
func TestPurgeDeletedPages(t *testing.T) {
	s, user := wikiStoreForTest(t)
	ctx := context.Background()
	book, err := s.CreateBook(ctx, user, "Purge", "", "")
	if err != nil {
		t.Fatal(err)
	}
	old := mustPage(t, s, book.ID, user, "Old", "gone")
	recent := mustPage(t, s, book.ID, user, "Recent", "kept")
	live := mustPage(t, s, book.ID, user, "Live", "")
	// A child left under a deleted parent by the old delete.
	if _, err := s.db.ExecContext(ctx, `UPDATE wiki_pages SET parent_id = $1::uuid WHERE id = $2::uuid`, old.ID, live.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnablePageShare(ctx, old.ID, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE wiki_pages SET deleted_at = NOW() - INTERVAL '40 days' WHERE id = $1::uuid;`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE wiki_pages SET deleted_at = NOW() - INTERVAL '2 days' WHERE id = $1::uuid;`, recent.ID); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeDeletedPages(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	count := func(q string, args ...any) int {
		var c int
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if count(`SELECT count(*) FROM wiki_pages WHERE id = $1::uuid`, old.ID) != 0 ||
		count(`SELECT count(*) FROM wiki_revisions WHERE page_id = $1::uuid`, old.ID) != 0 ||
		count(`SELECT count(*) FROM wiki_page_shares WHERE page_id = $1::uuid`, old.ID) != 0 {
		t.Error("the old page, its revisions or its share survived")
	}
	if count(`SELECT count(*) FROM wiki_pages WHERE id = $1::uuid`, recent.ID) != 1 {
		t.Error("a page deleted two days ago was purged")
	}
	if got := parentOf(t, s, book.ID, live.ID); got != "" {
		t.Errorf("the live child's parent = %q, want top level", got)
	}
	if n, _ := s.PurgeDeletedPages(ctx, 0); n != 0 {
		t.Error("a zero retention purged")
	}
}
