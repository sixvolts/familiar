package admin

// Handler tests for the public share page and its media, the media
// routes, and the page/member/revision handlers changed alongside them.
// DB-gated (wikiStoreForTest).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/media"
)

type shareEnv struct {
	h     *Handler
	s     *WikiStore
	ms    *media.Store
	user  string
	book  *Book
	host  string
	limit int64
}

func shareEnvForTest(t *testing.T) *shareEnv {
	t.Helper()
	s, user := wikiStoreForTest(t)
	const limit = 1 << 20
	ms, err := media.NewStore(s.db, t.TempDir(), limit)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	h.AttachWikiStore(s)
	h.AttachMedia(ms)
	h.AttachSharing(config.SharingConfig{PublicHosts: []string{"share.example"}, PublicBaseURL: "https://share.example"})
	book, err := s.EnsurePersonalBook(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return &shareEnv{h: h, s: s, ms: ms, user: user, book: book, host: "share.example", limit: limit}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (e *shareEnv) upload(t *testing.T, pageID, name string) *media.Media {
	t.Helper()
	m, err := e.ms.SaveImage(context.Background(), pageID, e.user, name, pngBytes(t, 4, 4))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *shareEnv) public(t *testing.T, path string, key, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = e.host
	r.SetPathValue("key", key)
	w := httptest.NewRecorder()
	if id == "" {
		e.h.publicShare(w, r)
	} else {
		r.SetPathValue("id", id)
		e.h.publicShareMedia(w, r)
	}
	return w
}

func (e *shareEnv) authed(method, path string, body io.Reader, user string) *http.Request {
	r := httptest.NewRequest(method, path, body)
	return r.WithContext(ctxWithAuth(r.Context(), AuthUser{UserID: user, Role: "user"}))
}

// The share page shows the images the page shows now, and serves only
// those: an image removed from the page stayed public while the share
// was on, and cacheable by any proxy for a day after.
func TestPublicShareMedia_OnlyWhatThePageShows(t *testing.T) {
	e := shareEnvForTest(t)
	ctx := context.Background()
	a := mustPage(t, e.s, e.book.ID, e.user, "Trip", "")
	b := mustPage(t, e.s, e.book.ID, e.user, "Photos", "")
	kept := e.upload(t, a.ID, "kept.png")
	removed := e.upload(t, a.ID, "id-card.png")
	copied := e.upload(t, b.ID, "view.png")

	seedWikiUsers(t, e.s, "other-user")
	otherBook, err := e.s.EnsurePersonalBook(ctx, "other-user")
	if err != nil {
		t.Fatal(err)
	}
	foreignPage := mustPage(t, e.s, otherBook.ID, "other-user", "Theirs", "")
	foreign, err := e.ms.SaveImage(ctx, foreignPage.ID, "other-user", "theirs.png", pngBytes(t, 4, 4))
	if err != nil {
		t.Fatal(err)
	}

	cur, err := e.s.UpdatePageByID(ctx, e.book.ID, a.ID, e.user, PagePatch{Content: ptr(fmt.Sprintf(
		"![a](/console/api/media/%s)\n\n![b](/console/api/media/%s)\n", kept.ID, removed.ID))})
	if err != nil {
		t.Fatal(err)
	}
	sh, err := e.s.EnablePageShare(ctx, a.ID, e.user)
	if err != nil {
		t.Fatal(err)
	}
	key := sh.ShareKey
	// The ID card was a mistake: remove it. Add an image copied from
	// another page (the editor made its URL absolute), sized, and one
	// from someone else's book.
	if _, err := e.s.UpdatePageByID(ctx, e.book.ID, a.ID, e.user, PagePatch{IfMatch: &cur.UpdatedAt, Content: ptr(fmt.Sprintf(
		"![a](/console/api/media/%s)\n\n![c](https://app.example/console/api/media/%s#w=40)\n\n![f](/console/api/media/%s)\n",
		kept.ID, strings.ToUpper(copied.ID), foreign.ID))}); err != nil {
		t.Fatal(err)
	}

	w := e.public(t, "/p/"+key, key, "")
	if w.Code != 200 {
		t.Fatalf("share page: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"/p/" + key + "/media/" + kept.ID, "/p/" + key + "/media/" + copied.ID, "width:40%"} {
		if !strings.Contains(body, want) {
			t.Errorf("share page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/console/api/media/"+kept.ID) || strings.Contains(body, "app.example") {
		t.Errorf("share page still points at the app:\n%s", body)
	}

	for _, c := range []struct {
		name, id string
		want     int
	}{
		{"shown image", kept.ID, 200},
		{"image removed from the page", removed.ID, 404},
		{"image copied from another page of the book", copied.ID, 200},
		{"image from another book", foreign.ID, 404},
		{"malformed id", "not-a-uuid", 404},
	} {
		w := e.public(t, "/p/"+key+"/media/"+c.id, key, c.id)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
		if w.Code == 200 {
			if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") || strings.Contains(cc, "public") {
				t.Errorf("%s: Cache-Control %q, want private no-store", c.name, cc)
			}
		}
	}
}

// The page used in mermaid-fences.spec.ts: the browser uploads a PNG
// for each fence its scanner finds, and the share page substitutes
// exactly those.
var mermaidFencePage = strings.Join([]string{
	"# How to write a diagram",
	"",
	"````markdown",
	"```mermaid",
	"graph TD; EXAMPLE-->ONLY;",
	"```",
	"````",
	"",
	"The real one:",
	"",
	"```Mermaid",
	"graph TD; REAL-->ONE;",
	"```",
	"",
	"~~~mermaid",
	"graph TD; TILDE-->TWO;",
	"~~~",
	"",
}, "\n")

func TestMermaidFences_MatchTheBrowser(t *testing.T) {
	var bodies []string
	for _, f := range mermaidFences(mermaidFencePage) {
		bodies = append(bodies, strings.TrimSpace(f.body))
	}
	want := []string{"graph TD; REAL-->ONE;", "graph TD; TILDE-->TWO;"}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Errorf("fences = %q, want %q", bodies, want)
	}
	// Unclosed runs to the end; a backtick in a ``` info string isn't a fence.
	got := mermaidFences("```mermaid\ngraph TD; A-->B;\n")
	if len(got) != 1 || strings.TrimSpace(got[0].body) != "graph TD; A-->B;" {
		t.Errorf("unclosed: %+v", got)
	}
	if got := mermaidFences("```mermaid `x`\ngraph\n```\n"); len(got) != 0 {
		t.Errorf("backtick info string: %+v", got)
	}
}

func TestPublicShare_SubstitutesTheFencesTheBrowserRendered(t *testing.T) {
	e := shareEnvForTest(t)
	ctx := context.Background()
	p := mustPage(t, e.s, e.book.ID, e.user, "Diagrams", mermaidFencePage)
	// Renders for both real fences, and a stale one of the example (the
	// old browser code rendered it).
	for _, body := range []string{"graph TD; REAL-->ONE;", "graph TD; TILDE-->TWO;", "graph TD; EXAMPLE-->ONLY;"} {
		e.upload(t, p.ID, "mermaid-"+MermaidRenderHash(body)+".png")
	}
	sh, err := e.s.EnablePageShare(ctx, p.ID, e.user)
	if err != nil {
		t.Fatal(err)
	}
	body := e.public(t, "/p/"+sh.ShareKey, sh.ShareKey, "").Body.String()
	if n := strings.Count(body, "/p/"+sh.ShareKey+"/media/"); n != 2 {
		t.Errorf("%d diagrams substituted, want 2:\n%s", n, body)
	}
	if strings.Contains(body, "REAL--&gt;ONE") || strings.Contains(body, "TILDE--&gt;TWO") {
		t.Errorf("a real diagram was left as code:\n%s", body)
	}
	if !strings.Contains(body, "EXAMPLE--&gt;ONLY") {
		t.Errorf("the example inside the markdown block was replaced:\n%s", body)
	}
}

func TestRewriteShareMedia(t *testing.T) {
	const id = "0b6f1a52-3c4d-4e5f-8a9b-0c1d2e3f4a5b"
	cases := []struct {
		in, wantSrc, wantStyle string
		ref                    bool
	}{
		{`<img src="/console/api/media/` + id + `#w=50">`, "/p/K/media/" + id, "width:50%", true},
		{`<img src="https://app.example/console/api/media/` + strings.ToUpper(id) + `">`, "/p/K/media/" + id, "", true},
		{`<img src="https://cdn.example/pic.png#w=25">`, "https://cdn.example/pic.png#w=25", "width:25%", false},
		{`<img src="/console/api/media/not-a-uuid">`, "/console/api/media/not-a-uuid", "", false},
		{`<img src="/console/api/media/` + id + `#w=100">`, "/p/K/media/" + id, "", true},
	}
	for _, c := range cases {
		out, refs := rewriteShareMedia(template.HTML(c.in), "K")
		s := string(out)
		if !strings.Contains(s, `src="`+c.wantSrc+`"`) {
			t.Errorf("%s → %s, want src %s", c.in, s, c.wantSrc)
		}
		if c.wantStyle != "" && !strings.Contains(s, c.wantStyle) || c.wantStyle == "" && strings.Contains(s, "style=") {
			t.Errorf("%s → %s, want style %q", c.in, s, c.wantStyle)
		}
		if refs[id] != c.ref {
			t.Errorf("%s: refs = %v", c.in, refs)
		}
	}
}

// countingBody is an endless body that records how much was read.
type countingBody struct{ n int64 }

func (c *countingBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	c.n += int64(len(p))
	return len(p), nil
}
func (c *countingBody) Close() error { return nil }

// An oversized upload is refused having read about the limit, not the
// whole body: the multipart parser spooled any size to a temp file.
func TestUploadPageMedia_CapsTheBody(t *testing.T) {
	e := shareEnvForTest(t)
	p := mustPage(t, e.s, e.book.ID, e.user, "Photos", "")
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if _, err := mw.CreateFormFile("file", "huge.png"); err != nil {
		t.Fatal(err)
	}
	body := &countingBody{}
	r := e.authed("POST", "/console/api/books/personal/page-by-id/"+p.ID+"/media",
		io.MultiReader(bytes.NewReader(head.Bytes()), io.LimitReader(body, 64<<20)), e.user)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.SetPathValue("slug", "personal")
	r.SetPathValue("page_id", p.ID)
	w := httptest.NewRecorder()
	e.h.uploadPageMedia(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413: %s", w.Code, w.Body.String())
	}
	if max := e.limit + 2<<20; body.n > max {
		t.Errorf("read %d bytes of the body, want at most ~%d", body.n, max)
	}
}

// Media routes: a malformed id is a plain 404 (it was a 500 with the
// driver's error), non-members and readers get 404s.
func TestMediaRoutes_NotFoundShapes(t *testing.T) {
	e := shareEnvForTest(t)
	ctx := context.Background()
	seedWikiUsers(t, e.s, "outsider", "reader-m")
	book, err := e.s.CreateBook(ctx, e.user, "Album", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddMember(ctx, e.user, book.ID, "reader-m", "reader"); err != nil {
		t.Fatal(err)
	}
	p := mustPage(t, e.s, book.ID, e.user, "Pics", "")
	m := e.upload(t, p.ID, "pic.png")

	do := func(method, id, user string) *httptest.ResponseRecorder {
		r := e.authed(method, "/console/api/media/"+id, nil, user)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		if method == "GET" {
			e.h.serveMedia(w, r)
		} else {
			e.h.deleteMedia(w, r)
		}
		return w
	}
	if w := do("GET", "not-a-uuid", e.user); w.Code != 404 || strings.Contains(w.Body.String(), "uuid") {
		t.Errorf("malformed id: %d %s", w.Code, w.Body.String())
	}
	if w := do("GET", m.ID, "outsider"); w.Code != 404 {
		t.Errorf("non-member read: %d", w.Code)
	}
	if w := do("GET", m.ID, "reader-m"); w.Code != 200 {
		t.Errorf("reader read: %d", w.Code)
	}
	if w := do("DELETE", m.ID, "reader-m"); w.Code != 404 {
		t.Errorf("reader delete: %d", w.Code)
	}
	if w := do("DELETE", m.ID, e.user); w.Code != 204 {
		t.Errorf("owner delete: %d", w.Code)
	}
}

// The by-id PATCH answers with the share's public_url, as the GETs do:
// the notes client adopts that share, and "Copy public link" then did
// nothing.
func TestPatchPageByID_ShareHasItsURL(t *testing.T) {
	e := shareEnvForTest(t)
	ctx := context.Background()
	p := mustPage(t, e.s, e.book.ID, e.user, "Shared note", "a")
	sh, err := e.s.EnablePageShare(ctx, p.ID, e.user)
	if err != nil {
		t.Fatal(err)
	}
	r := e.authed("PATCH", "/console/api/books/personal/page-by-id/"+p.ID, strings.NewReader(`{"content":"ab"}`), e.user)
	r.Header.Set("If-Match", p.UpdatedAt.Format(time.RFC3339Nano))
	r.SetPathValue("slug", "personal")
	r.SetPathValue("page_id", p.ID)
	w := httptest.NewRecorder()
	e.h.patchBookPageByID(w, r)
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Share *PageShare `json:"share"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Share == nil || out.Share.PublicURL != "https://share.example/p/"+sh.ShareKey {
		t.Errorf("share = %+v", out.Share)
	}
}

// PATCH role: {} is refused (it reset a reader to writer), and a
// non-member is a 404 (they were added).
func TestPatchBookMember_RoleRequiredMemberRequired(t *testing.T) {
	e := shareEnvForTest(t)
	ctx := context.Background()
	seedWikiUsers(t, e.s, "reader-p", "nobody-p")
	book, err := e.s.CreateBook(ctx, e.user, "Club", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddMember(ctx, e.user, book.ID, "reader-p", "reader"); err != nil {
		t.Fatal(err)
	}
	patch := func(target, body string) int {
		r := e.authed("PATCH", "/x", strings.NewReader(body), e.user)
		r.SetPathValue("slug", book.Slug)
		r.SetPathValue("user_id", target)
		w := httptest.NewRecorder()
		e.h.patchBookMember(w, r)
		return w.Code
	}
	if c := patch("reader-p", `{}`); c != 400 {
		t.Errorf("empty role: %d, want 400", c)
	}
	if role, _ := e.s.MemberRole(ctx, book.ID, "reader-p"); role != "reader" {
		t.Errorf("reader became %q", role)
	}
	if c := patch("nobody-p", `{"role":"writer"}`); c != 404 {
		t.Errorf("non-member: %d, want 404", c)
	}
	if c := patch("reader-p", `{"role":"writer"}`); c != 200 {
		t.Errorf("real change: %d", c)
	}
}

// The revisions list carries no content and pages.
func TestListRevisionsHandler(t *testing.T) {
	e := shareEnvForTest(t)
	ctx := context.Background()
	p := mustPage(t, e.s, e.book.ID, e.user, "Log", "v0")
	cur := p
	for _, c := range []string{"v1", "v2"} {
		var err error
		if cur, err = e.s.UpdatePageByID(ctx, e.book.ID, p.ID, e.user, PagePatch{Content: ptr(c), IfMatch: &cur.UpdatedAt}); err != nil {
			t.Fatal(err)
		}
	}
	r := e.authed("GET", "/x?limit=2", nil, e.user)
	r.SetPathValue("slug", "personal")
	r.SetPathValue("page_slug", cur.Slug)
	w := httptest.NewRecorder()
	e.h.listBookPageRevisions(w, r)
	var out struct {
		Items      []map[string]any `json:"items"`
		NextBefore string           `json:"next_before"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(out.Items) != 2 || out.NextBefore == "" {
		t.Errorf("items=%d next_before=%q", len(out.Items), out.NextBefore)
	}
	if _, has := out.Items[0]["content"]; has {
		t.Error("list items carry content")
	}
}
