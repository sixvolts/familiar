package wiki

// Wiki-skill tests. Mirror the notes_test.go style: in-memory fake
// backend, no Postgres. Coverage targets the role-based write gate,
// the slug→book resolution, and the patch / append composition that
// can't be inherited from the underlying store.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/skills"
)

// fakeWiki is a minimal in-memory WikiBackend. Books are keyed by
// slug; pages by (bookID, pageSlug). roles[bookID][userID] gates
// membership + write capability.
type fakeWiki struct {
	mu    sync.Mutex
	books map[string]*admin.Book                // slug → book
	roles map[string]map[string]string          // bookID → userID → role
	pages map[string]map[string]*admin.WikiPage // bookID → pageSlug → page

	created  int
	updated  int
	lastUser string
	pins     map[string]bool // userID+"|"+pageID → pinned

	// Test knobs for the concurrency retry path: failStaleTimes makes
	// UpdatePage return ErrPageStale that many times before succeeding
	// (simulating a human editing between the tool's read and write);
	// lastIfMatch records the precondition the last UpdatePage carried.
	failStaleTimes int
	lastIfMatch    *time.Time
}

func newFakeWiki() *fakeWiki {
	return &fakeWiki{
		books: map[string]*admin.Book{},
		roles: map[string]map[string]string{},
		pages: map[string]map[string]*admin.WikiPage{},
	}
}

func (f *fakeWiki) seedBook(slug, name string, members map[string]string) *admin.Book {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := &admin.Book{
		ID:        "book-" + slug,
		Slug:      slug,
		Name:      name,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	f.books[slug] = b
	f.roles[b.ID] = map[string]string{}
	for u, r := range members {
		f.roles[b.ID][u] = r
	}
	if f.pages[b.ID] == nil {
		f.pages[b.ID] = map[string]*admin.WikiPage{}
	}
	return b
}

func (f *fakeWiki) seedPage(bookID, slug, title, content string) *admin.WikiPage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pages[bookID] == nil {
		f.pages[bookID] = map[string]*admin.WikiPage{}
	}
	p := &admin.WikiPage{
		ID:        "page-" + slug,
		BookID:    bookID,
		Slug:      slug,
		Title:     title,
		Content:   content,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	f.pages[bookID][slug] = p
	return p
}

func (f *fakeWiki) ListBooks(_ context.Context, userID string, includeArchived bool) ([]admin.BookSummary, error) {
	return f.listBooks(userID, false)
}

func (f *fakeWiki) ListBooksWithPersonal(_ context.Context, userID string, includeArchived bool) ([]admin.BookSummary, error) {
	return f.listBooks(userID, true)
}

func (f *fakeWiki) listBooks(userID string, includePersonal bool) ([]admin.BookSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	out := []admin.BookSummary{}
	for _, b := range f.books {
		role := f.roles[b.ID][userID]
		if role == "" {
			continue
		}
		// fakeWiki doesn't model is_personal yet; tests that need
		// the include_personal split should set up books with the
		// "personal:" slug convention and rely on this skip.
		isPersonal := strings.HasPrefix(b.Slug, "personal:")
		if isPersonal && !includePersonal {
			continue
		}
		out = append(out, admin.BookSummary{
			ID: b.ID, Slug: b.Slug, Name: b.Name,
			Description: b.Description, Role: role,
			UpdatedAt: b.UpdatedAt, ArchivedAt: b.ArchivedAt,
		})
	}
	return out, nil
}

func (f *fakeWiki) GetBookBySlug(_ context.Context, slug, userID string, isAdmin bool) (*admin.Book, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	b, ok := f.books[slug]
	if !ok {
		return nil, admin.ErrBookNotFound
	}
	if isAdmin {
		cp := *b
		return &cp, nil
	}
	if f.roles[b.ID][userID] == "" {
		return nil, admin.ErrBookNotFound
	}
	cp := *b
	return &cp, nil
}

func (f *fakeWiki) EnsurePersonalBook(_ context.Context, userID string) (*admin.Book, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	slug := "personal:" + userID
	if b, ok := f.books[slug]; ok {
		cp := *b
		return &cp, nil
	}
	b := &admin.Book{ID: "book-" + slug, Slug: slug, Name: "Personal",
		IsPersonal: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.books[slug] = b
	f.roles[b.ID] = map[string]string{userID: "owner"}
	f.pages[b.ID] = map[string]*admin.WikiPage{}
	cp := *b
	return &cp, nil
}

func (f *fakeWiki) MemberRole(_ context.Context, bookID, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roles[bookID][userID], nil
}

func (f *fakeWiki) ListPages(_ context.Context, bookID string) ([]admin.WikiPageSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []admin.WikiPageSummary{}
	for _, p := range f.pages[bookID] {
		out = append(out, admin.WikiPageSummary{
			ID: p.ID, Slug: p.Slug, Title: p.Title,
			Snippet: p.Content, UpdatedAt: p.UpdatedAt,
		})
	}
	return out, nil
}

func (f *fakeWiki) GetPage(_ context.Context, bookID, pageSlug string) (*admin.WikiPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.pages[bookID][pageSlug]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, admin.ErrPageNotFound
}

func (f *fakeWiki) CreatePage(_ context.Context, bookID, userID, title, content, requestedSlug string) (*admin.WikiPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	f.lastUser = userID
	slug := requestedSlug
	if slug == "" {
		slug = strings.ToLower(strings.ReplaceAll(title, " ", "-"))
	}
	p := &admin.WikiPage{
		ID: "page-" + slug, BookID: bookID, Slug: slug,
		Title: title, Content: content,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if f.pages[bookID] == nil {
		f.pages[bookID] = map[string]*admin.WikiPage{}
	}
	f.pages[bookID][slug] = p
	cp := *p
	return &cp, nil
}

func (f *fakeWiki) UpdatePage(_ context.Context, bookID, pageSlug, userID string, p admin.PagePatch) (*admin.WikiPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated++
	f.lastUser = userID
	f.lastIfMatch = p.IfMatch
	if f.failStaleTimes > 0 {
		f.failStaleTimes--
		return nil, admin.ErrPageStale
	}
	cur, ok := f.pages[bookID][pageSlug]
	if !ok {
		return nil, admin.ErrPageNotFound
	}
	if p.Title != nil {
		cur.Title = *p.Title
	}
	if p.Content != nil {
		cur.Content = *p.Content
	}
	cur.UpdatedAt = time.Now()
	cp := *cur
	return &cp, nil
}

func (f *fakeWiki) AppendPage(_ context.Context, bookID, pageID, userID, text string) (*admin.WikiPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated++
	f.lastUser = userID
	for _, cur := range f.pages[bookID] {
		if cur.ID == pageID {
			if cur.Content == "" {
				cur.Content = text
			} else {
				cur.Content = strings.TrimRight(cur.Content, "\n") + "\n\n" + text
			}
			cur.UpdatedAt = time.Now()
			cp := *cur
			return &cp, nil
		}
	}
	return nil, admin.ErrPageNotFound
}

func (f *fakeWiki) SearchPages(_ context.Context, bookID, query string, limit int) ([]admin.WikiPageSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []admin.WikiPageSummary{}
	q := strings.ToLower(query)
	for _, p := range f.pages[bookID] {
		if strings.Contains(strings.ToLower(p.Title), q) ||
			strings.Contains(strings.ToLower(p.Content), q) {
			out = append(out, admin.WikiPageSummary{
				ID: p.ID, Slug: p.Slug, Title: p.Title,
				Snippet: p.Content, UpdatedAt: p.UpdatedAt,
			})
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeWiki) SetPagePinned(_ context.Context, userID, pageID string, pinned bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	if f.pins == nil {
		f.pins = map[string]bool{}
	}
	f.pins[userID+"|"+pageID] = pinned
	return nil
}

func ctxAsUser(userID string) context.Context {
	return skills.WithContext(context.Background(), skills.SessionContext{UserID: userID})
}

// ctxScoped is ctxAsUser plus a shard book allowlist — the envelope a
// scheduled action / shard run carries. bookIDs are the resolved book
// IDs (fakeWiki uses "book-"+slug).
func ctxScoped(userID string, bookIDs ...string) context.Context {
	return skills.WithContext(context.Background(), skills.SessionContext{
		UserID:    userID,
		BookScope: bookIDs,
	})
}

func newSkillWith(b WikiBackend) *Skill {
	return New(func() WikiBackend { return b })
}

// ──────────────────────────────────────────────────────────────────
// Boundary
// ──────────────────────────────────────────────────────────────────

func TestExecute_NoBackend(t *testing.T) {
	s := New(nil)
	res, _ := s.Execute(ctxAsUser("operator"), "list_books", nil)
	if !strings.Contains(res.Error, "not configured") {
		t.Errorf("nil backend should error helpfully; got %q", res.Error)
	}
}

func TestExecute_NoUser(t *testing.T) {
	s := newSkillWith(newFakeWiki())
	res, _ := s.Execute(context.Background(), "list_books", nil)
	if !strings.Contains(res.Error, "no authenticated user") {
		t.Errorf("missing user should error; got %q", res.Error)
	}
}

func TestExecute_UnknownToolReturnsGoError(t *testing.T) {
	s := newSkillWith(newFakeWiki())
	_, err := s.Execute(ctxAsUser("operator"), "nope", nil)
	if err == nil {
		t.Error("unknown tool should return Go error (integration bug, not user input)")
	}
}

// ──────────────────────────────────────────────────────────────────
// Read tools
// ──────────────────────────────────────────────────────────────────

func TestListBooks_OnlyMembers(t *testing.T) {
	b := newFakeWiki()
	b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "owner"})
	b.seedBook("familywiki", "Family Wiki", map[string]string{"alison": "owner"})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "list_books", nil)
	if !strings.Contains(res.Content, "Firehouse Wiki") {
		t.Errorf("operator should see Firehouse Wiki; got %s", res.Content)
	}
	if strings.Contains(res.Content, "Family Wiki") {
		t.Errorf("operator leaked alison's book: %s", res.Content)
	}
}

func TestListPages_RequiresBookSlug(t *testing.T) {
	s := newSkillWith(newFakeWiki())
	res, _ := s.Execute(ctxAsUser("operator"), "list_pages", json.RawMessage(`{}`))
	if !strings.Contains(res.Error, "book_slug is required") {
		t.Errorf("missing slug should error; got %q", res.Error)
	}
}

func TestListPages_BookNotFoundForNonMember(t *testing.T) {
	b := newFakeWiki()
	b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"alison": "owner"})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "list_pages",
		json.RawMessage(`{"book_slug":"firehouse"}`))
	if !strings.Contains(strings.ToLower(res.Error), "not found") {
		t.Errorf("non-member should see not-found; got %q", res.Error)
	}
}

func TestReadPage_HappyPath(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "reader"})
	b.seedPage(bk.ID, "intro", "Intro", "# Welcome\n\nFirst page.")
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "read_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"intro"}`))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.HasPrefix(res.Content, "# Intro") {
		t.Errorf("content should lead with title; got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "First page.") {
		t.Errorf("body missing: %s", res.Content)
	}
	if !strings.Contains(res.Content, "page_slug: intro") {
		t.Errorf("footer missing slug: %s", res.Content)
	}
}

func TestSearchPages_Scoped(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "writer"})
	b.seedPage(bk.ID, "engine-1", "Engine 1", "pump capacity 1500 gpm")
	b.seedPage(bk.ID, "engine-2", "Engine 2", "tanker overhead")
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "search_pages",
		json.RawMessage(`{"book_slug":"firehouse","query":"pump"}`))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "Engine 1") {
		t.Errorf("expected Engine 1 hit; got %s", res.Content)
	}
	if strings.Contains(res.Content, "Engine 2") {
		t.Errorf("Engine 2 shouldn't match 'pump': %s", res.Content)
	}
}

// ──────────────────────────────────────────────────────────────────
// Write gate (owner/writer can; reader cannot)
// ──────────────────────────────────────────────────────────────────

func TestCreatePage_WriterAllowed(t *testing.T) {
	b := newFakeWiki()
	b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "writer"})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "create_page",
		json.RawMessage(`{"book_slug":"firehouse","title":"New Page","content":"x"}`))
	if res.Error != "" {
		t.Fatalf("writer should be allowed; got error: %s", res.Error)
	}
	if b.created != 1 {
		t.Errorf("backend Create called %d times, want 1", b.created)
	}
}

func TestCreatePage_ReaderDenied(t *testing.T) {
	b := newFakeWiki()
	b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "reader"})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "create_page",
		json.RawMessage(`{"book_slug":"firehouse","title":"New Page"}`))
	if !strings.Contains(res.Error, "Read-only") {
		t.Errorf("reader should be denied; got: %s", res.Error)
	}
	if b.created != 0 {
		t.Errorf("backend should not be called; created=%d", b.created)
	}
}

// patch_page must carry a precondition (If-Match) and recover from a
// concurrent human edit landing between its read and write by
// re-reading and re-applying — closing the agent-vs-human lost-update
// window without clobbering the human's other lines.
func TestPatchPage_RetriesOnStaleAndThreadsIfMatch(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "owner"})
	b.seedPage(bk.ID, "list", "List", "- milk\n- eggz\n")
	b.failStaleTimes = 1 // one concurrent write, then success
	s := newSkillWith(b)

	res, err := s.Execute(ctxAsUser("operator"), "patch_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"list","find":"eggz","replace":"eggs"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("patch should have retried past the stale write, got error: %s", res.Error)
	}
	if b.lastIfMatch == nil {
		t.Error("patch_page did not send an If-Match precondition")
	}
	got, _ := b.GetPage(context.Background(), bk.ID, "list")
	if !strings.Contains(got.Content, "eggs") || strings.Contains(got.Content, "eggz") {
		t.Errorf("patch not applied after retry: %q", got.Content)
	}
}

// A patch that keeps losing the CAS gives up with a re-read hint rather
// than looping forever or clobbering.
func TestPatchPage_GivesUpAfterPersistentConflict(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "owner"})
	b.seedPage(bk.ID, "list", "List", "- milk\n")
	b.failStaleTimes = 99
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "patch_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"list","find":"milk","replace":"oat milk"}`))
	if !strings.Contains(res.Error, "another writer") {
		t.Errorf("expected a give-up hint, got: %q / %q", res.Content, res.Error)
	}
}

func TestUpdatePage_TitlePreservedWhenOmitted(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "owner"})
	b.seedPage(bk.ID, "intro", "Original Title", "old body")
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "update_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"intro","content":"new body"}`))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got, _ := b.GetPage(context.Background(), bk.ID, "intro")
	if got.Title != "Original Title" {
		t.Errorf("title was lost: %q", got.Title)
	}
	if got.Content != "new body" {
		t.Errorf("content not updated: %q", got.Content)
	}
}

func TestUpdatePage_ReaderDenied(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "reader"})
	b.seedPage(bk.ID, "intro", "T", "body")
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "update_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"intro","content":"hacked"}`))
	if !strings.Contains(res.Error, "Read-only") {
		t.Errorf("reader should be denied; got: %s", res.Error)
	}
}

func TestAppendToPage_ComposesParagraph(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "writer"})
	b.seedPage(bk.ID, "log", "Run Log", "Line 1\nLine 2")
	s := newSkillWith(b)
	_, err := s.Execute(ctxAsUser("operator"), "append_to_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"log","text":"Line 3 added"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, _ := b.GetPage(context.Background(), bk.ID, "log")
	want := "Line 1\nLine 2\n\nLine 3 added"
	if got.Content != want {
		t.Errorf("appended body = %q\nwant %q", got.Content, want)
	}
}

func TestAppendToPage_OnEmptyJustWrites(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "writer"})
	b.seedPage(bk.ID, "log", "Empty", "")
	s := newSkillWith(b)
	_, _ = s.Execute(ctxAsUser("operator"), "append_to_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"log","text":"first"}`))
	got, _ := b.GetPage(context.Background(), bk.ID, "log")
	if got.Content != "first" {
		t.Errorf("empty-page append should write bare; got %q", got.Content)
	}
}

func TestPatchPage_ExactMatch(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "writer"})
	b.seedPage(bk.ID, "engine-1", "Engine 1", "pump capacity 1500 gpm\nhose: 200ft")
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "patch_page", json.RawMessage(
		`{"book_slug":"firehouse","page_slug":"engine-1","find":"1500 gpm","replace":"1750 gpm"}`))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got, _ := b.GetPage(context.Background(), bk.ID, "engine-1")
	if !strings.Contains(got.Content, "1750 gpm") {
		t.Errorf("replacement not applied: %q", got.Content)
	}
	if strings.Contains(got.Content, "1500 gpm") {
		t.Errorf("original still present: %q", got.Content)
	}
}

func TestPatchPage_AmbiguousRejected(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "writer"})
	b.seedPage(bk.ID, "log", "Log", "ok\nok\nok")
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "patch_page", json.RawMessage(
		`{"book_slug":"firehouse","page_slug":"log","find":"ok","replace":"done"}`))
	if !strings.Contains(res.Error, "matches 3 locations") {
		t.Errorf("ambiguous patch should error; got: %s", res.Error)
	}
}

func TestPatchPage_NotFoundString(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "writer"})
	b.seedPage(bk.ID, "log", "Log", "actual content")
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "patch_page", json.RawMessage(
		`{"book_slug":"firehouse","page_slug":"log","find":"missing string","replace":"x"}`))
	if !strings.Contains(res.Error, "find string not found") {
		t.Errorf("missing string should error; got: %s", res.Error)
	}
}

// ──────────────────────────────────────────────────────────────────
// Shard book confinement (BookScope on SessionContext)
//
// These exercise the scheduled-action / shard run path where authz.go's
// session-level book intersection never runs, so the wiki skill itself
// is the confinement boundary. A member of BOTH books must still be
// confined to the scoped one.
// ──────────────────────────────────────────────────────────────────

func TestBookScope_ListBooksConfinedToAllowlist(t *testing.T) {
	b := newFakeWiki()
	b.seedBook("familywiki", "Family Wiki", map[string]string{"operator": "owner"})
	b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "owner"})
	s := newSkillWith(b)

	res, _ := s.Execute(ctxScoped("operator", "book-familywiki"), "list_books", nil)
	if !strings.Contains(res.Content, "Family Wiki") {
		t.Errorf("scoped book should still list; got %s", res.Content)
	}
	if strings.Contains(res.Content, "Firehouse Wiki") {
		t.Errorf("out-of-scope book leaked into listing: %s", res.Content)
	}
}

func TestBookScope_ReadOutOfScopeIsNotFound(t *testing.T) {
	b := newFakeWiki()
	gh := b.seedBook("familywiki", "Family Wiki", map[string]string{"operator": "owner"})
	fh := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "owner"})
	b.seedPage(gh.ID, "grocery", "Grocery List", "- [x] milk")
	b.seedPage(fh.ID, "secret", "Secret", "classified")
	s := newSkillWith(b)

	// In scope: reads fine.
	in, _ := s.Execute(ctxScoped("operator", "book-familywiki"), "read_page",
		json.RawMessage(`{"book_slug":"familywiki","page_slug":"grocery"}`))
	if in.Error != "" {
		t.Fatalf("in-scope read should succeed; got %q", in.Error)
	}

	// Out of scope: not-found, even though operator owns it. No existence
	// leak, no content.
	out, _ := s.Execute(ctxScoped("operator", "book-familywiki"), "read_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"secret"}`))
	if !strings.Contains(strings.ToLower(out.Error), "not found") {
		t.Errorf("out-of-scope read should be not-found; got %q", out.Error)
	}
	if strings.Contains(out.Content, "classified") {
		t.Errorf("out-of-scope content leaked: %q", out.Content)
	}
}

func TestBookScope_WriteOutOfScopeRefusedAndNoMutation(t *testing.T) {
	b := newFakeWiki()
	fh := b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "owner"})
	b.seedPage(fh.ID, "roster", "Roster", "original")
	s := newSkillWith(b)

	res, _ := s.Execute(ctxScoped("operator", "book-familywiki"), "update_page",
		json.RawMessage(`{"book_slug":"firehouse","page_slug":"roster","content":"hijacked"}`))
	if !strings.Contains(strings.ToLower(res.Error), "not found") {
		t.Errorf("out-of-scope update should be refused as not-found; got %q", res.Error)
	}
	if b.updated != 0 {
		t.Errorf("out-of-scope update must not reach the backend; updated=%d", b.updated)
	}
	got, _ := b.GetPage(context.Background(), fh.ID, "roster")
	if got.Content != "original" {
		t.Errorf("out-of-scope page was mutated: %q", got.Content)
	}
}

func TestBookScope_InScopeWriteSucceeds(t *testing.T) {
	b := newFakeWiki()
	gh := b.seedBook("familywiki", "Family Wiki", map[string]string{"operator": "owner"})
	b.seedPage(gh.ID, "grocery", "Grocery List", "- [x] milk\n- [ ] bread")
	s := newSkillWith(b)

	res, _ := s.Execute(ctxScoped("operator", "book-familywiki"), "update_page",
		json.RawMessage(`{"book_slug":"familywiki","page_slug":"grocery","content":"- [ ] bread"}`))
	if res.Error != "" {
		t.Fatalf("in-scope write should succeed; got %q", res.Error)
	}
	got, _ := b.GetPage(context.Background(), gh.ID, "grocery")
	if got.Content != "- [ ] bread" {
		t.Errorf("in-scope content not updated: %q", got.Content)
	}
}

func TestBookScope_EmptyScopeIsUnrestricted(t *testing.T) {
	// A shard with empty book_access (the common case) must behave like
	// the trusted path: ctxAsUser carries no BookScope, so both books
	// the member owns are reachable.
	b := newFakeWiki()
	b.seedBook("familywiki", "Family Wiki", map[string]string{"operator": "owner"})
	b.seedBook("firehouse", "Firehouse Wiki", map[string]string{"operator": "owner"})
	s := newSkillWith(b)

	res, _ := s.Execute(ctxAsUser("operator"), "list_books", nil)
	if !strings.Contains(res.Content, "Family Wiki") || !strings.Contains(res.Content, "Firehouse Wiki") {
		t.Errorf("empty scope should see all member books; got %s", res.Content)
	}
}

// ──────────────────────────────────────────────────────────────────
// Sanity
// ──────────────────────────────────────────────────────────────────

func TestErrorsIs_BookNotFoundCrossesBoundary(t *testing.T) {
	if !errors.Is(admin.ErrBookNotFound, admin.ErrBookNotFound) {
		t.Fatal("errors.Is broken")
	}
}

// ──────────────────────────────────────────────────────────────────
// pin_page
// ──────────────────────────────────────────────────────────────────

func TestPinPage_PinsForActingUser(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("familywiki", "Family Wiki", map[string]string{"operator": "owner"})
	p := b.seedPage(bk.ID, "recipes", "Recipes", "stuff")
	s := newSkillWith(b)

	res, err := s.Execute(ctxAsUser("operator"), "pin_page",
		json.RawMessage(`{"book_slug":"familywiki","page_slug":"recipes"}`))
	if err != nil || res.Error != "" {
		t.Fatalf("pin failed: err=%v toolErr=%q", err, res.Error)
	}
	if !strings.Contains(res.Content, "Pinned") {
		t.Errorf("result should say Pinned; got %q", res.Content)
	}
	if !b.pins["operator|"+p.ID] {
		t.Error("pin row not written for operator")
	}

	// Explicit pinned=false unpins.
	res, _ = s.Execute(ctxAsUser("operator"), "pin_page",
		json.RawMessage(`{"book_slug":"familywiki","page_slug":"recipes","pinned":false}`))
	if !strings.Contains(res.Content, "Unpinned") {
		t.Errorf("result should say Unpinned; got %q", res.Content)
	}
	if b.pins["operator|"+p.ID] {
		t.Error("unpin did not clear the row")
	}
}

func TestPinPage_ReaderRoleCanPin(t *testing.T) {
	// A pin is a per-user preference, not a content write — readers
	// can pin even though create/update would refuse them.
	b := newFakeWiki()
	bk := b.seedBook("familywiki", "Family Wiki", map[string]string{"alison": "reader"})
	p := b.seedPage(bk.ID, "recipes", "Recipes", "stuff")
	s := newSkillWith(b)

	res, err := s.Execute(ctxAsUser("alison"), "pin_page",
		json.RawMessage(`{"book_slug":"familywiki","page_slug":"recipes"}`))
	if err != nil || res.Error != "" {
		t.Fatalf("reader pin failed: err=%v toolErr=%q", err, res.Error)
	}
	if !b.pins["alison|"+p.ID] {
		t.Error("pin row not written for alison")
	}
}

func TestPinPage_NonMemberAndMissingPage(t *testing.T) {
	b := newFakeWiki()
	bk := b.seedBook("familywiki", "Family Wiki", map[string]string{"operator": "owner"})
	b.seedPage(bk.ID, "recipes", "Recipes", "stuff")
	s := newSkillWith(b)

	// Non-member: book reads as not-found (no existence leak).
	res, _ := s.Execute(ctxAsUser("mallory"), "pin_page",
		json.RawMessage(`{"book_slug":"familywiki","page_slug":"recipes"}`))
	if res.Error == "" || !strings.Contains(strings.ToLower(res.Error), "no book") &&
		!strings.Contains(strings.ToLower(res.Error), "not found") {
		t.Errorf("non-member should get not-found; got %q / %q", res.Error, res.Content)
	}
	if len(b.pins) != 0 {
		t.Error("non-member pin must not write")
	}

	// Missing page slug errors without writing.
	res, _ = s.Execute(ctxAsUser("operator"), "pin_page",
		json.RawMessage(`{"book_slug":"familywiki","page_slug":"nope"}`))
	if res.Error == "" {
		t.Errorf("missing page should error; got content %q", res.Content)
	}
	if len(b.pins) != 0 {
		t.Error("missing-page pin must not write")
	}
}

func TestPinPage_RequiresArgs(t *testing.T) {
	s := newSkillWith(newFakeWiki())
	res, _ := s.Execute(ctxAsUser("operator"), "pin_page", json.RawMessage(`{"book_slug":"x"}`))
	if !strings.Contains(res.Error, "required") {
		t.Errorf("missing page_slug should error; got %q", res.Error)
	}
}

// The "personal" book_slug alias resolves to the caller's personal
// notes without a list_books round-trip — the deterministic path that
// replaced the fragile "list, find is_personal, guess the slug" flow a
// local model botched by writing to a random wiki.
func TestCreatePage_PersonalAlias(t *testing.T) {
	f := newFakeWiki()
	s := newSkillWith(f)
	ctx := ctxAsUser("operator")

	res, err := s.Execute(ctx, "create_page",
		json.RawMessage(`{"book_slug":"personal","title":"Research: Optane","content":"# notes"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("create_page personal alias errored: %s", res.Error)
	}
	// The page must land in the personal book, created on demand.
	f.mu.Lock()
	pb, ok := f.books["personal:operator"]
	var pageCount int
	if ok {
		pageCount = len(f.pages[pb.ID])
	}
	f.mu.Unlock()
	if !ok {
		t.Fatal("personal book was not created/resolved by the alias")
	}
	if pageCount != 1 {
		t.Errorf("personal book has %d pages, want 1", pageCount)
	}
}

// create_page/update_page stash the page location in ToolResult.Data so
// the pipeline can detect a research note and the workspace can auto-open
// it. The book_slug carried must be the REAL slug (personal:{user}), not
// the "personal" alias the caller passed.
func TestCreatePage_DataCarriesLocation(t *testing.T) {
	f := newFakeWiki()
	s := newSkillWith(f)
	ctx := ctxAsUser("operator")

	res, err := s.Execute(ctx, "create_page",
		json.RawMessage(`{"book_slug":"personal","title":"Research: Optane","content":"# notes"}`))
	if err != nil || res.Error != "" {
		t.Fatalf("create_page: err=%v resErr=%s", err, res.Error)
	}
	if len(res.Data) == 0 {
		t.Fatal("create_page result carried no Data")
	}
	var loc struct {
		BookSlug string `json:"book_slug"`
		PageSlug string `json:"page_slug"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal(res.Data, &loc); err != nil {
		t.Fatalf("Data is not valid JSON: %v", err)
	}
	if loc.BookSlug != "personal:operator" {
		t.Errorf("book_slug = %q, want real slug personal:operator (not the alias)", loc.BookSlug)
	}
	if loc.PageSlug == "" {
		t.Error("page_slug is empty")
	}
	if loc.Title != "Research: Optane" {
		t.Errorf("title = %q, want %q", loc.Title, "Research: Optane")
	}
}

// flexBool tolerates the string booleans local models emit, so a
// {"include_personal":"true"} call no longer fails to unmarshal.
func TestListBooks_StringBoolean(t *testing.T) {
	f := newFakeWiki()
	f.seedBook("personal:operator", "Personal", map[string]string{"operator": "owner"})
	f.books["personal:operator"].IsPersonal = true
	s := newSkillWith(f)
	ctx := ctxAsUser("operator")

	// The failing shape from the transcript: include_personal as a string.
	res, err := s.Execute(ctx, "list_books", json.RawMessage(`{"include_personal":"true"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("string-boolean include_personal still errors: %s", res.Error)
	}
}
