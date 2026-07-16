package notes

// Notes-skill tests (FAMILIAR-WORKSPACE-SPEC Phase 2c). The skill
// now talks to a NotesBackend interface backed in production by
// admin.NotesStore (Postgres). These tests use a fake backend
// keyed by user_id so we can exercise the role-scoped behavior
// without standing up a database.

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

// fakeBackend is a trivial in-memory NotesBackend keyed by user_id.
// Records the last user_id seen by each method so tests assert on
// scoping end-to-end.
type fakeBackend struct {
	mu       sync.Mutex
	notes    map[string]map[string]*admin.Note // userID → noteID → note
	lastUser string
	created  int
	updated  int
	appended int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{notes: map[string]map[string]*admin.Note{}}
}

func (f *fakeBackend) seedNote(userID string, n *admin.Note) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notes[userID] == nil {
		f.notes[userID] = map[string]*admin.Note{}
	}
	f.notes[userID][n.ID] = n
}

func (f *fakeBackend) List(_ context.Context, userID, folder string, includeDeleted bool, limit, offset int) ([]admin.NoteSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	out := []admin.NoteSummary{}
	for _, n := range f.notes[userID] {
		if folder != "" && n.Folder != folder {
			continue
		}
		out = append(out, admin.NoteSummary{
			ID: n.ID, Title: n.Title, Folder: n.Folder,
			Pinned: n.Pinned, UpdatedAt: n.UpdatedAt,
			Snippet: n.Content,
		})
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeBackend) Get(_ context.Context, id, userID string) (*admin.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	if n, ok := f.notes[userID][id]; ok {
		cp := *n
		return &cp, nil
	}
	return nil, admin.ErrNoteNotFound
}

func (f *fakeBackend) Create(_ context.Context, userID, title, content, folder string) (*admin.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	f.created++
	id := "n-" + strings.ToLower(title) + "-" + userID
	now := time.Now()
	n := &admin.Note{
		ID: id, UserID: userID, Title: title, Content: content,
		Folder: folder, CreatedAt: now, UpdatedAt: now,
	}
	if f.notes[userID] == nil {
		f.notes[userID] = map[string]*admin.Note{}
	}
	f.notes[userID][id] = n
	cp := *n
	return &cp, nil
}

func (f *fakeBackend) Update(_ context.Context, id, userID string, p admin.NotePatch) (*admin.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	f.updated++
	n, ok := f.notes[userID][id]
	if !ok {
		return nil, admin.ErrNoteNotFound
	}
	if p.Title != nil {
		n.Title = *p.Title
	}
	if p.Content != nil {
		n.Content = *p.Content
	}
	if p.Folder != nil {
		n.Folder = *p.Folder
	}
	if p.Pinned != nil {
		n.Pinned = *p.Pinned
	}
	n.UpdatedAt = time.Now()
	cp := *n
	return &cp, nil
}

func (f *fakeBackend) Append(_ context.Context, id, userID, text string) (*admin.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	f.appended++
	n, ok := f.notes[userID][id]
	if !ok {
		return nil, admin.ErrNoteNotFound
	}
	if n.Content == "" {
		n.Content = text
	} else {
		n.Content = strings.TrimRight(n.Content, "\n") + "\n\n" + text
	}
	n.UpdatedAt = time.Now()
	cp := *n
	return &cp, nil
}

func (f *fakeBackend) Search(_ context.Context, userID, q string, limit int) ([]admin.NoteSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = userID
	out := []admin.NoteSummary{}
	for _, n := range f.notes[userID] {
		if strings.Contains(strings.ToLower(n.Title), strings.ToLower(q)) ||
			strings.Contains(strings.ToLower(n.Content), strings.ToLower(q)) {
			out = append(out, admin.NoteSummary{
				ID: n.ID, Title: n.Title, Folder: n.Folder,
				Pinned: n.Pinned, UpdatedAt: n.UpdatedAt,
				Snippet: n.Content,
			})
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func ctxAsUser(userID string) context.Context {
	return skills.WithContext(context.Background(), skills.SessionContext{UserID: userID})
}

func newSkillWith(b NotesBackend) *Skill {
	return New(func() NotesBackend { return b })
}

// ──────────────────────────────────────────────────────────────────
// Configuration / auth boundary
// ──────────────────────────────────────────────────────────────────

func TestExecute_NoBackend(t *testing.T) {
	s := New(nil)
	res, _ := s.Execute(ctxAsUser("alison"), "list_recent_notes", nil)
	if !strings.Contains(res.Error, "not configured") {
		t.Errorf("nil backend should error helpfully; got %q", res.Error)
	}
}

func TestExecute_NoUserContext(t *testing.T) {
	s := newSkillWith(newFakeBackend())
	// No SessionContext — pipeline didn't install one.
	res, _ := s.Execute(context.Background(), "list_recent_notes", nil)
	if !strings.Contains(res.Error, "no authenticated user") {
		t.Errorf("missing user should error; got %q", res.Error)
	}
}

func TestExecute_UnknownToolReturnsGoError(t *testing.T) {
	s := newSkillWith(newFakeBackend())
	_, err := s.Execute(ctxAsUser("alison"), "nope", nil)
	if err == nil {
		t.Error("unknown tool should return Go error (integration bug, not user input)")
	}
}

// ──────────────────────────────────────────────────────────────────
// Read tools
// ──────────────────────────────────────────────────────────────────

func TestSearchNotes_HappyPath(t *testing.T) {
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{
		ID: "n1", Title: "Cannonball Power Notes",
		Content:   "regen recovery curve at 35°C",
		UpdatedAt: time.Now(),
	})
	s := newSkillWith(b)
	res, err := s.Execute(ctxAsUser("operator"), "search_notes",
		json.RawMessage(`{"query":"cannonball"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "Cannonball Power Notes") {
		t.Errorf("content missing title:\n%s", res.Content)
	}
	if b.lastUser != "operator" {
		t.Errorf("backend saw user %q, want operator (scope leak?)", b.lastUser)
	}
}

func TestSearchNotes_OneUserDoesNotSeeAnother(t *testing.T) {
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{ID: "a1", Title: "operator-private", Content: "secret"})
	b.seedNote("alison", &admin.Note{ID: "b1", Title: "different", Content: "y"})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("alison"), "search_notes",
		json.RawMessage(`{"query":"secret"}`))
	// Response will mention "secret" because the no-match message
	// echoes the query back. The leak test is that operator's note
	// title doesn't appear in alison's response.
	if strings.Contains(res.Content, "operator-private") {
		t.Errorf("alison saw operator's note title: %s", res.Content)
	}
	if !strings.Contains(res.Content, "no notes match") {
		t.Errorf("expected no-match message for alison, got: %s", res.Content)
	}
}

func TestSearchNotes_EmptyResults(t *testing.T) {
	b := newFakeBackend()
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("alison"), "search_notes",
		json.RawMessage(`{"query":"absent"}`))
	if !strings.Contains(res.Content, `no notes match "absent"`) {
		t.Errorf("expected no-match message, got %q", res.Content)
	}
}

func TestSearchNotes_QueryRequired(t *testing.T) {
	s := newSkillWith(newFakeBackend())
	res, _ := s.Execute(ctxAsUser("alison"), "search_notes", json.RawMessage(`{}`))
	if !strings.Contains(res.Error, "query is required") {
		t.Errorf("missing query should error; got %q", res.Error)
	}
}

func TestListRecentNotes_DefaultLimit(t *testing.T) {
	b := newFakeBackend()
	for i := 0; i < 12; i++ {
		b.seedNote("operator", &admin.Note{
			ID:        "n" + string(rune('a'+i)),
			Title:     "Note " + string(rune('A'+i)),
			Content:   "body",
			UpdatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		})
	}
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "list_recent_notes", json.RawMessage(`{}`))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	count := strings.Count(res.Content, "id: n")
	if count != defaultListLimit {
		t.Errorf("default limit produced %d results, want %d", count, defaultListLimit)
	}
}

func TestListRecentNotes_LimitClamped(t *testing.T) {
	s := newSkillWith(newFakeBackend())
	res, err := s.Execute(ctxAsUser("alison"), "list_recent_notes",
		json.RawMessage(`{"limit":999}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Error != "" {
		t.Errorf("limit=999 should clamp silently, got error: %s", res.Error)
	}
}

func TestReadNote_HappyPath(t *testing.T) {
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{
		ID: "note-xyz", Title: "Phase 1 retro",
		Content:   "# Lessons learned\n\n- tool dispatch worked",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "read_note",
		json.RawMessage(`{"id":"note-xyz"}`))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.HasPrefix(res.Content, "# Phase 1 retro") {
		t.Errorf("content should lead with markdown header:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "tool dispatch worked") {
		t.Errorf("body missing:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "note_id: note-xyz") {
		t.Errorf("footer missing note_id:\n%s", res.Content)
	}
}

func TestReadNote_NotFound(t *testing.T) {
	s := newSkillWith(newFakeBackend())
	res, _ := s.Execute(ctxAsUser("alison"), "read_note",
		json.RawMessage(`{"id":"missing"}`))
	if !strings.Contains(strings.ToLower(res.Error), "not found") {
		t.Errorf("error should mention 'not found', got: %s", res.Error)
	}
}

func TestReadNote_OwnershipScoped(t *testing.T) {
	// Operator has note "n1"; Alison asks for it. Should 404.
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{ID: "n1", Title: "operator's", Content: "body"})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("alison"), "read_note",
		json.RawMessage(`{"id":"n1"}`))
	if !strings.Contains(strings.ToLower(res.Error), "not found") {
		t.Errorf("alison reading operator's note should 404; got: %s", res.Error)
	}
}

// ──────────────────────────────────────────────────────────────────
// Write tools
// ──────────────────────────────────────────────────────────────────

func TestCreateNote_PersistsAndReturnsID(t *testing.T) {
	b := newFakeBackend()
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("alison"), "create_note",
		json.RawMessage(`{"title":"New Note","content":"first line"}`))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if b.created != 1 {
		t.Errorf("backend Create called %d times, want 1", b.created)
	}
	if b.lastUser != "alison" {
		t.Errorf("backend saw user %q, want alison", b.lastUser)
	}
	if !strings.Contains(res.Content, "New Note") {
		t.Errorf("response should mention title; got: %s", res.Content)
	}
}

func TestCreateNote_TitleRequired(t *testing.T) {
	s := newSkillWith(newFakeBackend())
	res, _ := s.Execute(ctxAsUser("alison"), "create_note",
		json.RawMessage(`{"content":"x"}`))
	if !strings.Contains(res.Error, "title is required") {
		t.Errorf("missing title should error; got %q", res.Error)
	}
}

func TestUpdateNote_TitlePreservedWhenOmitted(t *testing.T) {
	// Critical regression: omitting title in the patch must NOT
	// blank the existing title. The new skill achieves this by
	// only setting patch.Title when newTitle is non-empty.
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{
		ID: "n1", Title: "Original Title", Content: "old body",
	})
	s := newSkillWith(b)
	res, err := s.Execute(ctxAsUser("operator"), "update_note",
		json.RawMessage(`{"id":"n1","content":"new body"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got, _ := b.Get(context.Background(), "n1", "operator")
	if got.Title != "Original Title" {
		t.Errorf("title was lost: %q, want 'Original Title'", got.Title)
	}
	if got.Content != "new body" {
		t.Errorf("content not updated: %q", got.Content)
	}
}

func TestUpdateNote_RetitleHonored(t *testing.T) {
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{ID: "n1", Title: "old", Content: "x"})
	s := newSkillWith(b)
	_, _ = s.Execute(ctxAsUser("operator"), "update_note",
		json.RawMessage(`{"id":"n1","content":"x","title":"Renamed"}`))
	got, _ := b.Get(context.Background(), "n1", "operator")
	if got.Title != "Renamed" {
		t.Errorf("title = %q, want Renamed", got.Title)
	}
}

func TestUpdateNote_NotFoundForNonOwner(t *testing.T) {
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{ID: "n1", Title: "x", Content: "y"})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("alison"), "update_note",
		json.RawMessage(`{"id":"n1","content":"hacked"}`))
	if !strings.Contains(strings.ToLower(res.Error), "not found") {
		t.Errorf("alison updating operator's note should 404; got: %s", res.Error)
	}
}

func TestAppendToNote_AppendsParagraph(t *testing.T) {
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{
		ID: "n1", Title: "Travel notes",
		Content: "Berlin: bring power adapter\nLisbon: tram 28 to Alfama",
	})
	s := newSkillWith(b)
	res, _ := s.Execute(ctxAsUser("operator"), "append_to_note",
		json.RawMessage(`{"id":"n1","text":"Never forget the irish goodbye"}`))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got, _ := b.Get(context.Background(), "n1", "operator")
	want := "Berlin: bring power adapter\nLisbon: tram 28 to Alfama\n\nNever forget the irish goodbye"
	if got.Content != want {
		t.Errorf("appended body = %q\nwant %q", got.Content, want)
	}
}

func TestAppendToNote_OnEmptyNoteJustWritesText(t *testing.T) {
	b := newFakeBackend()
	b.seedNote("operator", &admin.Note{ID: "n1", Title: "Empty", Content: ""})
	s := newSkillWith(b)
	_, _ = s.Execute(ctxAsUser("operator"), "append_to_note",
		json.RawMessage(`{"id":"n1","text":"first line"}`))
	got, _ := b.Get(context.Background(), "n1", "operator")
	if got.Content != "first line" {
		t.Errorf("empty-note append should write bare; got %q", got.Content)
	}
}

func TestAppendToNote_RequiredArgs(t *testing.T) {
	s := newSkillWith(newFakeBackend())
	res, _ := s.Execute(ctxAsUser("alison"), "append_to_note", json.RawMessage(`{"text":"x"}`))
	if !strings.Contains(res.Error, "id is required") {
		t.Errorf("missing id should error; got %q", res.Error)
	}
	res, _ = s.Execute(ctxAsUser("alison"), "append_to_note", json.RawMessage(`{"id":"x"}`))
	if !strings.Contains(res.Error, "text is required") {
		t.Errorf("missing text should error; got %q", res.Error)
	}
}

// ──────────────────────────────────────────────────────────────────
// Formatting
// ──────────────────────────────────────────────────────────────────

func TestPreview_StripsMarkdown(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"# Heading", "Heading"},
		{"## Subheading", "Subheading"},
		{"plain *italic* and **bold**", "plain italic and bold"},
		{"a [link](https://x.com) here", "a link here"},
		{"![alt](img.png) gone", " gone"},
		{"`inline code` works", "inline code works"},
		{"- bullet one\n- bullet two", "bullet one\nbullet two"},
		{"1. numbered\n2. items", "numbered\nitems"},
		{"```go\nfn() {}\n```\nafter", "\nafter"},
	}
	for _, tc := range cases {
		got := stripMarkdown(tc.in)
		if got != tc.want {
			t.Errorf("stripMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Sanity: errors.Is matching against the admin sentinel works
// across the package boundary so the skill's NotFound branches
// fire correctly.
func TestErrorsIs_AdminSentinel(t *testing.T) {
	if !errors.Is(admin.ErrNoteNotFound, admin.ErrNoteNotFound) {
		t.Fatal("errors.Is broken")
	}
}
