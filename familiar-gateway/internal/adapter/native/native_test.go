package native

// Mux registration tests for the native HTTP adapter.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/session"
)

// fakeSessionReader stands in for *admin.Handler's cookie→user
// resolution. Returns a fixed identity so the chat handler reaches
// the conversation-ownership gate via the cookie path (no resolver
// needed).
type fakeSessionReader struct {
	uid     string
	ok      bool
	shardID string // non-empty = a shard (kiosk) session owned by uid
	canChat bool
}

func (f fakeSessionReader) PrincipalFromRequest(*http.Request) (string, string, bool, bool) {
	return f.uid, f.shardID, f.canChat, f.ok
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

// ── Shard (kiosk) sessions ─────────────────────────────────────────
//
// A shard session's user id is its owner's. These pin that it can never
// reach the owner's trusted path: the pipeline is nil in every case, so
// a request that slipped past the gate would panic instead of passing.

const kioskConv = "22222222-2222-2222-2222-222222222222"

func kioskAdapter(canChat bool, target *ShardChatTarget) *Adapter {
	a := &Adapter{sessions: session.NewManager()}
	a.SetSessionReader(fakeSessionReader{uid: "owner", ok: true, shardID: "kitchen", canChat: canChat})
	a.SetConversationOwner(&fakeConvOwner{owned: true})
	a.SetShardChatResolver(func(context.Context, string, string) (*ShardChatTarget, string, error) {
		return target, "", nil
	})
	return a
}

func postChat(t *testing.T, a *Adapter, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleChat(w, req)
	return w
}

func TestChat_ShardSessionWithoutConversationRefused(t *testing.T) {
	a := kioskAdapter(true, &ShardChatTarget{ShardID: "kitchen"})
	w := postChat(t, a, `{"message":"what do you remember about me"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: the no-conversation path is the owner's trusted session (body: %s)", w.Code, w.Body.String())
	}
}

func TestChat_ShardSessionChatDisabledRefused(t *testing.T) {
	a := kioskAdapter(false, &ShardChatTarget{ShardID: "kitchen"})
	w := postChat(t, a, `{"message":"hi","conversation_id":"`+kioskConv+`"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a shard with chat disabled (body: %s)", w.Code, w.Body.String())
	}
}

func TestChat_ShardSessionOnTrustedConversationRefused(t *testing.T) {
	// The owner owns the conversation, but it isn't shard-bound: the
	// resolver returns no target, which means "run the trusted path".
	a := kioskAdapter(true, nil)
	w := postChat(t, a, `{"message":"read my journal","conversation_id":"`+kioskConv+`"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an owner conversation not bound to the shard (body: %s)", w.Code, w.Body.String())
	}
}

func TestChat_ShardSessionOnOtherShardsConversationRefused(t *testing.T) {
	a := kioskAdapter(true, &ShardChatTarget{ShardID: "garage"})
	w := postChat(t, a, `{"message":"hi","conversation_id":"`+kioskConv+`"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a conversation bound to a different shard (body: %s)", w.Code, w.Body.String())
	}
}

func TestStop_ShardSessionCannotStopOwnerTurn(t *testing.T) {
	a := kioskAdapter(true, nil) // key resolves to a trusted (owner) conversation
	a.sessions.GetOrCreateWithID(kioskConv, "workspace", "owner").SetIdentity("workspace", "owner")
	req := httptest.NewRequest("POST", "/api/chat/stop", strings.NewReader(`{"session_id":"`+kioskConv+`"}`))
	w := httptest.NewRecorder()
	a.handleStop(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a kiosk must not address its owner's sessions (body: %s)", w.Code, w.Body.String())
	}
}

func TestEphemeralConversation(t *testing.T) {
	conv := "3f2c9a1e-7b4d-4e8a-9c1f-2a6b8d0e4f13"
	cases := map[string]bool{
		conv + ":1727350000000000000": true,
		conv:                          false, // plain conversation key
		conv + ":":                    false,
		conv + ":12ab":                false,
		"not-a-uuid:123":              false,
		"shard:kitchen:123":           false,
	}
	for key, want := range cases {
		got, ok := ephemeralConversation(key)
		if ok != want || (ok && got != conv) {
			t.Errorf("ephemeralConversation(%q) = (%q, %v), want ok=%v", key, got, ok, want)
		}
	}
}

// Stop and status must reach an ephemeral shard turn (it runs under an
// unregistered per-message session), proving ownership through the
// conversation, and must refuse anyone who doesn't own it.
func TestTurnOwnership_EphemeralTurnsGoThroughTheConversation(t *testing.T) {
	conv := "3f2c9a1e-7b4d-4e8a-9c1f-2a6b8d0e4f13"
	eph := conv + ":1727350000000000000"
	a := &Adapter{sessions: session.NewManager()}

	a.SetConversationOwner(&fakeConvOwner{owned: true})
	if owned, live := a.turnOwnership(context.Background(), eph, "alice", ""); !owned || !live {
		t.Errorf("owner's ephemeral turn: owned=%v live=%v, want both true", owned, live)
	}
	a.SetConversationOwner(&fakeConvOwner{owned: false})
	if owned, _ := a.turnOwnership(context.Background(), eph, "mallory", ""); owned {
		t.Error("a non-owner was allowed to stop an ephemeral turn")
	}

	held := a.sessions.GetOrCreateWithID(conv, "workspace", "alice")
	held.ClaimIdentity("workspace", "alice")
	if owned, _ := a.turnOwnership(context.Background(), conv, "mallory", ""); owned {
		t.Error("a non-owner was allowed to stop a registered session")
	}
	if owned, live := a.turnOwnership(context.Background(), "no-such-session", "alice", ""); !owned || live {
		t.Errorf("unknown key: owned=%v live=%v, want owned with nothing live", owned, live)
	}
}

// A live session another user holds is refused, never re-homed onto the
// caller (the pipeline is nil, so re-homing would panic instead).
func TestChat_SessionHeldByAnotherUserRefused(t *testing.T) {
	conv := "3f2c9a1e-7b4d-4e8a-9c1f-2a6b8d0e4f13"
	a := &Adapter{sessions: session.NewManager()}
	a.SetSessionReader(fakeSessionReader{uid: "bob", ok: true})
	a.SetConversationOwner(&fakeConvOwner{owned: true}) // a colliding id that passes the store check
	held := a.sessions.GetOrCreateWithID(conv, "workspace", "alice")
	held.ClaimIdentity("workspace", "alice")
	w := postChat(t, a, `{"message":"hi","conversation_id":"`+conv+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
	if held.UserID() != "alice" {
		t.Fatalf("alice's session was re-homed to %q", held.UserID())
	}
}

func TestServerPersistsReply(t *testing.T) {
	conv := "3f2c9a1e-7b4d-4e8a-9c1f-2a6b8d0e4f13"
	cases := []struct {
		name      string
		conv      string
		haveStore bool
		target    *ShardChatTarget
		want      bool
	}{
		{"workspace conversation", conv, true, nil, true},
		{"persistent shard conversation", conv, true, &ShardChatTarget{ShardID: "k"}, true},
		{"ephemeral shard turn", conv, true, &ShardChatTarget{ShardID: "k", Ephemeral: true}, false},
		{"no conversation", "", true, nil, false},
		{"no store", conv, false, nil, false},
	}
	for _, c := range cases {
		if got := serverPersistsReply(c.conv, c.haveStore, c.target); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A stream must outlive the server's WriteTimeout: it is one deadline per
// request, and it cut every chat stream at ten minutes while turns may
// run for thirty.
func TestClearWriteDeadline_StreamOutlivesWriteTimeout(t *testing.T) {
	for _, clear := range []bool{true, false} {
		mux := http.NewServeMux()
		mux.HandleFunc("/s", func(w http.ResponseWriter, r *http.Request) {
			if clear {
				clearWriteDeadline(w)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(400 * time.Millisecond)
			fmt.Fprint(w, "event: done\ndata: {}\n\n")
		})
		srv := httptest.NewUnstartedServer(mux)
		srv.Config.WriteTimeout = 150 * time.Millisecond
		srv.Start()
		resp, err := http.Get(srv.URL + "/s")
		var body []byte
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		srv.Close()
		got := strings.Contains(string(body), "event: done")
		if got != clear {
			t.Errorf("clearWriteDeadline=%v: late event delivered=%v", clear, got)
		}
	}
}
