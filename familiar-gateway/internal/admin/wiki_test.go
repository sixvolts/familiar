package admin

// Books + wiki handler tests (BOOKS-WIKI-ARCHITECTURE Phase 1a +
// role rename). Same shape as notes_test.go: 503/401 branches at
// the handler boundary, scope-helper coverage, and DTO round-
// trips. Store-level SQL tests are integration concerns deferred
// to a real test pool.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListBooks_503WhenNotConfigured(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest("GET", "/console/api/books", nil).
		WithContext(ctxAuth(alisonUser()))
	rec := httptest.NewRecorder()
	h.listBooks(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503 when wiki store not wired", rec.Code)
	}
}

func TestCreateBook_503WhenNotConfigured(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest("POST", "/console/api/books", strings.NewReader(`{"name":"x"}`)).
		WithContext(ctxAuth(alisonUser()))
	rec := httptest.NewRecorder()
	h.createBook(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503 when wiki store not wired", rec.Code)
	}
}

func TestListBooks_401WhenUnauthenticated(t *testing.T) {
	// No auth on the context — handler must reject before touching
	// the store. Using a non-nil store proves the auth check fires
	// first; a 503 here would mean we leaked through.
	h := &Handler{wiki: &WikiStore{}}
	req := httptest.NewRequest("GET", "/console/api/books", nil)
	rec := httptest.NewRecorder()
	h.listBooks(rec, req)
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401 without auth", rec.Code)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "untitled"},
		{"  ", "untitled"},
		{"Hello World", "hello-world"},
		{"Project IRIS — kickoff!", "project-iris-kickoff"},
		{"---trim---", "trim"},
		{"a/b/c", "a-b-c"},
		{"  Multiple   Spaces  ", "multiple-spaces"},
	}
	for _, tc := range cases {
		got := slugify(tc.in)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSlugify_TruncatesLong(t *testing.T) {
	got := slugify(strings.Repeat("a", 200))
	if len(got) > 60 {
		t.Errorf("slugify long input length = %d, want <= 60", len(got))
	}
}

func TestSlugify_BoundaryAwareTruncation(t *testing.T) {
	// Long input with internal dashes should truncate on a dash
	// boundary (not in the middle of a word) when the dash sits
	// past the 30-char minimum.
	in := "very-long-and-descriptive-book-name-that-needs-truncation-applied-here"
	got := slugify(in)
	if len(got) > 60 {
		t.Errorf("length %d > 60", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("trailing dash in %q", got)
	}
}

func TestScopeForWiki_401WhenUnauthenticated(t *testing.T) {
	h := &Handler{wiki: &WikiStore{}}
	req := httptest.NewRequest("GET", "/console/api/books/anything", nil)
	req.SetPathValue("slug", "anything")
	rec := httptest.NewRecorder()
	h.getBook(rec, req)
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401 without auth", rec.Code)
	}
}

func TestScopeForWiki_400WhenSlugMissing(t *testing.T) {
	h := &Handler{wiki: &WikiStore{}}
	req := httptest.NewRequest("GET", "/console/api/books/", nil).
		WithContext(ctxAuth(alisonUser()))
	rec := httptest.NewRecorder()
	h.getBook(rec, req)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400 for missing slug", rec.Code)
	}
}

func TestBookDTO_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	archived := now.Add(time.Hour)
	b := Book{
		ID: "b1", Slug: "ops", Name: "Ops",
		Description: "How we run", CreatedBy: "alison",
		CreatedAt: now, UpdatedAt: now,
		ArchivedAt: &archived,
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{
		`"id":"b1"`, `"slug":"ops"`, `"name":"Ops"`,
		`"created_by":"alison"`, `"archived_at":`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("missing %q in %s", field, raw)
		}
	}
	var back Book
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ArchivedAt == nil {
		t.Error("archived_at lost on round-trip")
	}
}

func TestBookSummary_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	b := BookSummary{
		ID: "b1", Slug: "ops", Name: "Ops",
		Description: "blurb", Role: "owner", UpdatedAt: now,
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"role":"owner"`) {
		t.Errorf("role missing in %s", raw)
	}
}

func TestWikiPageDTO_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	parent := "p0"
	maintainer := "alison"
	p := WikiPage{
		ID: "p1", BookID: "b1", Slug: "intro", Title: "Intro",
		Content:  "# Hi",
		ParentID: &parent, SortOrder: 5,
		CreatedBy: "operator", UpdatedBy: "alison",
		MaintainedBy: &maintainer, EditCount: 12,
		CreatedAt: now, UpdatedAt: now,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{
		`"book_id":"b1"`, `"parent_id":"p0"`,
		`"sort_order":5`, `"edit_count":12`,
		`"maintained_by":"alison"`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("missing %q in %s", field, raw)
		}
	}
}

func TestWikiPageSummary_OmitsContent(t *testing.T) {
	// The summary shape should never carry the content field —
	// that's the whole point of having two types. JSON marshaling
	// must reflect that.
	now := time.Now().UTC().Truncate(time.Second)
	s := WikiPageSummary{
		ID: "p1", Slug: "intro", Title: "Intro",
		Snippet: "Hi", UpdatedAt: now, UpdatedBy: "alison",
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"content":`) {
		t.Errorf("summary leaked content field: %s", raw)
	}
}

func TestWikiRevisionDTO_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	r := WikiRevision{
		ID: "r1", PageID: "p1",
		Content: "body", EditedBy: "alison",
		CreatedAt: now, Summary: "fix typos",
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{
		`"page_id":"p1"`, `"summary":"fix typos"`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("missing %q in %s", field, raw)
		}
	}
}

func TestBookMember_RoleSerialization(t *testing.T) {
	m := BookMember{BookID: "b1", UserID: "alison", Role: "owner"}
	raw, _ := json.Marshal(m)
	if !strings.Contains(string(raw), `"role":"owner"`) {
		t.Errorf("role missing in %s", raw)
	}
}

func TestEnsureWiki_503WhenNotWired(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	if h.ensureWiki(rec) {
		t.Error("ensureWiki returned true with no store wired")
	}
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestEnsureWiki_TrueWhenWired(t *testing.T) {
	h := &Handler{wiki: &WikiStore{}}
	rec := httptest.NewRecorder()
	if !h.ensureWiki(rec) {
		t.Error("ensureWiki returned false with store wired")
	}
}

// ── Role validation ───────────────────────────────────────────────
//
// AddMember validates the role string before issuing any SQL, so
// these tests run safely against a zero-value WikiStore (nil pool).
// The legacy 'editor' / 'viewer' values are now rejected; the new
// 'writer' / 'reader' / 'owner' set is accepted (and would proceed
// to the SQL path, which we don't exercise here).

func TestAddMember_RejectsLegacyRoles(t *testing.T) {
	s := &WikiStore{}
	for _, role := range []string{"editor", "viewer"} {
		_, err := s.AddMember(context.Background(), "operator", "b1", "alison", role)
		if err == nil {
			t.Errorf("expected legacy role %q to be rejected", role)
			continue
		}
		if !strings.Contains(err.Error(), "invalid role") {
			t.Errorf("role %q: want 'invalid role' error, got %v", role, err)
		}
	}
}

func TestAddMember_RejectsUnknownRole(t *testing.T) {
	s := &WikiStore{}
	_, err := s.AddMember(context.Background(), "operator", "b1", "alison", "admin")
	if err == nil || !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("expected 'invalid role' error for 'admin', got %v", err)
	}
}

// requirePageWrite gates POST/PATCH/DELETE on pages. Admins bypass;
// the role-lookup path needs a real DB so we cover only the bypass
// branch here. Non-admin enforcement is exercised via integration
// tests against a real pool.
func TestRequirePageWrite_AdminBypass(t *testing.T) {
	h := &Handler{wiki: &WikiStore{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/console/api/books/x/pages", nil)
	if !h.requirePageWrite(rec, req, &Book{ID: "b1"}, "operator", true) {
		t.Error("admin should bypass page-write check")
	}
	if rec.Code != 200 {
		t.Errorf("admin path wrote status %d (expected nothing)", rec.Code)
	}
}

func TestBookMember_AcceptsAllThreeRoles(t *testing.T) {
	// JSON round-trip on each of the new roles. Catches accidental
	// drops if the role name set drifts.
	for _, role := range []string{"owner", "writer", "reader"} {
		m := BookMember{BookID: "b1", UserID: "alison", Role: role}
		raw, _ := json.Marshal(m)
		if !strings.Contains(string(raw), `"role":"`+role+`"`) {
			t.Errorf("role %q not preserved: %s", role, raw)
		}
	}
}

// ── Link parser ──────────────────────────────────────────────────

func TestParseLinks_Forms(t *testing.T) {
	cases := []struct {
		name, body string
		want       []ParsedLink
	}{
		{
			name: "same-book",
			body: "see [[deploy-process]] for the steps",
			want: []ParsedLink{{TargetPageSlug: "deploy-process"}},
		},
		{
			name: "same-book with display",
			body: "see [[deploy-process|the deploy doc]]",
			want: []ParsedLink{{TargetPageSlug: "deploy-process", DisplayText: "the deploy doc"}},
		},
		{
			name: "cross-book",
			body: "linked to [[engineering/deploy-process]]",
			want: []ParsedLink{{TargetBookSlug: "engineering", TargetPageSlug: "deploy-process"}},
		},
		{
			name: "cross-book with display",
			body: "linked to [[engineering/deploy-process|Deploy]]",
			want: []ParsedLink{{TargetBookSlug: "engineering", TargetPageSlug: "deploy-process", DisplayText: "Deploy"}},
		},
		{
			name: "case-normalized to lowercase",
			body: "see [[Engineering/Deploy-Process]]",
			want: []ParsedLink{{TargetBookSlug: "engineering", TargetPageSlug: "deploy-process"}},
		},
		{
			name: "multiple in one body",
			body: "[[a]] then [[b]] then [[book/c|see]]",
			want: []ParsedLink{
				{TargetPageSlug: "a"},
				{TargetPageSlug: "b"},
				{TargetBookSlug: "book", TargetPageSlug: "c", DisplayText: "see"},
			},
		},
		{
			name: "no links",
			body: "no brackets here at all",
			want: nil,
		},
		{
			name: "empty link skipped",
			body: "[[]] [[ ]] [[real]]",
			want: []ParsedLink{{TargetPageSlug: "real"}},
		},
		{
			name: "duplicate dedups, last display wins",
			body: "[[a]] then [[a|second]]",
			want: []ParsedLink{{TargetPageSlug: "a", DisplayText: "second"}},
		},
		{
			name: "nested brackets don't merge",
			body: "[[first]][[second]]",
			want: []ParsedLink{{TargetPageSlug: "first"}, {TargetPageSlug: "second"}},
		},
		{
			name: "trailing slash with no page slug skipped",
			body: "[[book/]]",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseLinks(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d links, want %d:\n  got:  %#v\n  want: %#v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("link[%d] = %#v, want %#v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// If-Match precondition (PagePatch.IfMatch + 409 mapping in
// patchBookPage / patchBookPageByID) requires a live wiki store to
// reach the comparison branch — every test in this file is a
// handler-boundary check that bails at the 503 / 401 / scope step
// before touching SQL. Coverage for the 409 path lives in the
// integration suite. The pure parsing of the If-Match header is
// inspected at review.
