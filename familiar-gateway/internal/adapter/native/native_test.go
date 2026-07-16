package native

// Mux registration tests for the native HTTP adapter.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/session"
)

// fakeSessionReader stands in for *admin.Handler's cookie→user
// resolution. Returns a fixed identity so the chat handler reaches
// the conversation-ownership gate via the cookie path (no resolver
// needed).
type fakeSessionReader struct {
	uid string
	ok  bool
}

func (f fakeSessionReader) UserIDFromRequest(*http.Request) (string, bool) {
	return f.uid, f.ok
}

// fakeConvOwner records the ownership question and returns a canned
// verdict.
type fakeConvOwner struct {
	owned     bool
	err       error
	gotConvID string
	gotUserID string
}

func (f *fakeConvOwner) OwnsConversation(_ context.Context, convID, userID string) (bool, error) {
	f.gotConvID = convID
	f.gotUserID = userID
	return f.owned, f.err
}

// A caller who supplies a conversation_id they don't own must be
// rejected (403) BEFORE the session is bound or the pipeline runs —
// this is the IDOR fix from EXTERNAL-READINESS-REVIEW.md P0. The
// nil pipeline proves we never reach it on the reject path.
func TestChat_RejectsUnownedConversationID(t *testing.T) {
	owner := &fakeConvOwner{owned: false}
	a := &Adapter{sessions: session.NewManager()}
	a.SetSessionReader(fakeSessionReader{uid: "alice", ok: true})
	a.SetConversationOwner(owner)

	body := `{"message":"hi","conversation_id":"11111111-1111-1111-1111-111111111111"}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleChat(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if owner.gotUserID != "alice" {
		t.Errorf("ownership checked for userID %q, want alice", owner.gotUserID)
	}
	if owner.gotConvID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("ownership checked for convID %q", owner.gotConvID)
	}
}

// An ownership-check DB error must fail closed (500), not fall
// through to binding the session.
func TestChat_OwnershipErrorFailsClosed(t *testing.T) {
	owner := &fakeConvOwner{err: context.DeadlineExceeded}
	a := &Adapter{sessions: session.NewManager()}
	a.SetSessionReader(fakeSessionReader{uid: "alice", ok: true})
	a.SetConversationOwner(owner)

	body := `{"message":"hi","conversation_id":"11111111-1111-1111-1111-111111111111"}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleChat(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

type recordingHandler struct {
	paths []string
}

func (r *recordingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.paths = append(r.paths, req.URL.Path)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func TestBuildMux_AdminHandlerMountedAtBothConsoleAndAdmin(t *testing.T) {
	rec := &recordingHandler{}
	a := &Adapter{}
	a.SetAdminHandler(rec)

	mux := a.buildMux()

	cases := []string{
		"/console/api/auth/status",
		"/console/api/shards",
		"/console/app.js",
		"/admin/api/auth/status",
		"/admin/",
	}
	for _, path := range cases {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, w.Code)
		}
	}
	if len(rec.paths) != len(cases) {
		t.Fatalf("recorded %d paths, want %d", len(rec.paths), len(cases))
	}
}

// Both /api/chat and the shards mount must coexist — /v1/shards/* lands
// on the shards handler and /api/chat lands on the chat handler.
func TestBuildMux_ChatRouteRegisteredAlongsideShards(t *testing.T) {
	shards := &recordingHandler{}
	a := &Adapter{}
	a.SetShardsHandler(shards)
	mux := a.buildMux()

	// POST /api/chat with an empty body should reach the handler
	// (returning 400 from the empty-message gate, not 404 from a
	// missing route).
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"message":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Errorf("POST /api/chat returned 404 — route not registered")
	}

	// /v1/shards/foo/invoke routes to the shards handler.
	req = httptest.NewRequest("POST", "/v1/shards/foo/invoke", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("shards path didn't reach handler: %d", w.Code)
	}
}

// /api/chat with a missing message field must 400, not 500.
func TestChatRequestEmptyMessageRejected(t *testing.T) {
	a := &Adapter{}
	mux := a.buildMux()
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"message":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty message: status = %d, want 400", w.Code)
	}
}

// The per-user concurrency cap rejects the (maxInflightPerUser+1)th
// in-flight request for one user with 429, and frees the slot when a
// request returns. We simulate held slots by pre-filling the map.
func TestChat_PerUserConcurrencyCap(t *testing.T) {
	a := &Adapter{sessions: session.NewManager()}
	a.SetSessionReader(fakeSessionReader{uid: "alice", ok: true})

	// Saturate alice's slots.
	for i := 0; i < maxInflightPerUser; i++ {
		if !a.acquireSlot("alice") {
			t.Fatalf("acquireSlot %d unexpectedly failed", i)
		}
	}
	// Next chat request must be rejected with 429 (no conversation_id
	// so it never needs the pipeline — the cap fires first).
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleChat(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}

	// Free one slot; a fresh acquire must now succeed.
	a.releaseSlot("alice")
	if !a.acquireSlot("alice") {
		t.Fatal("slot not freed after releaseSlot")
	}
	// A different user is unaffected by alice's saturation.
	if !a.acquireSlot("bob") {
		t.Fatal("bob's slots should be independent of alice's")
	}
}

// /api/chat/title is LLM compute and must require a session cookie:
// no sessionReader (or an invalid cookie) → 401.
func TestTitle_RequiresAuth(t *testing.T) {
	// No sessionReader wired → fail closed.
	a := &Adapter{}
	req := httptest.NewRequest("POST", "/api/chat/title", strings.NewReader(`{"user_message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleTitle(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthed title: status = %d, want 401", w.Code)
	}

	// Invalid cookie → 401.
	a2 := &Adapter{}
	a2.SetSessionReader(fakeSessionReader{ok: false})
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/chat/title", strings.NewReader(`{"user_message":"hi"}`))
	a2.handleTitle(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("invalid-cookie title: status = %d, want 401", w2.Code)
	}
}

func TestHealthEndpoint(t *testing.T) {
	a := &Adapter{}
	mux := a.buildMux()
	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("health: status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("health body = %q", w.Body.String())
	}
}
