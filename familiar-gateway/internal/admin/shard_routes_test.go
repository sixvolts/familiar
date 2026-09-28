package admin

// Shard (kiosk) sessions carry their owner's user id. These tests pin
// the deny-by-default route gate that keeps that id from reaching owner
// surfaces: the allow-list is checked against the real route table, and
// a DB-backed test drives an actual shard session cookie through the
// real middleware chain.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/shards"
	"github.com/familiar/gateway/internal/testutil"
)

// consoleRoutes returns every authed console pattern the real Mux
// registers.
func consoleRoutes(t *testing.T) []string {
	t.Helper()
	h := &Handler{}
	h.Mux(nil)
	if len(h.consolePatterns) < 50 {
		t.Fatalf("recorded only %d console routes; the recorder isn't seeing registrations", len(h.consolePatterns))
	}
	return h.consolePatterns
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// requestFor builds a request that the given pattern matches.
func requestFor(pattern string) *http.Request {
	method, path, _ := strings.Cut(pattern, " ")
	return httptest.NewRequest(method, pathParam.ReplaceAllString(path, "x"), nil)
}

// gateOverStub mounts every real console pattern on a stub mux that
// answers 200, behind the real gate. A 403 therefore comes from the
// gate alone.
func gateOverStub(t *testing.T) http.Handler {
	t.Helper()
	stub := http.NewServeMux()
	for _, p := range consoleRoutes(t) {
		stub.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	}
	return (&Handler{}).shardRouteGate(stub)
}

func serveAs(g http.Handler, au AuthUser, req *http.Request) int {
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req.WithContext(ctxWithAuth(req.Context(), au)))
	return w.Code
}

func kioskUser(panels []string, canChat bool) AuthUser {
	return AuthUser{
		UserID: "owner", Role: "admin", PrincipalType: PrincipalTypeShard,
		PrincipalID: "kitchen", ShardID: "kitchen",
		Permissions: &SessionPermissions{Panels: panels, CanChat: canChat},
	}
}

// Every allow-list entry must name a route that exists. A stale entry
// would silently stop protecting (or granting) anything after a rename.
func TestShardRoutes_AllowListNamesRealRoutes(t *testing.T) {
	registered := map[string]bool{}
	for _, p := range consoleRoutes(t) {
		registered[p] = true
	}
	var stale []string
	for p := range shardRoutes {
		if !registered[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("shardRoutes lists patterns the console doesn't register:\n  %s", strings.Join(stale, "\n  "))
	}
}

// The routes from the review's findings: each one served the owner's
// data or powers to a kiosk. None may be reachable from a shard session
// whatever its envelope, even one that inherits every panel.
func TestShardRouteGate_OwnerSurfacesClosedToEveryKiosk(t *testing.T) {
	g := gateOverStub(t)
	owner := []string{
		"POST /console/api/auth/enrollment-token",
		"GET /console/api/auth/passkeys",
		"DELETE /console/api/auth/passkeys/{credential_id}",
		"GET /console/api/profile",
		"PATCH /console/api/profile",
		"PATCH /console/api/profile/me",
		"POST /console/api/push/subscribe",
		"GET /console/api/events/pages",
		"GET /console/api/home/pins",
		"POST /console/api/books/{slug}/members",
		"PATCH /console/api/books/{slug}",
		"POST /console/api/books/{slug}/page-by-id/{page_id}/share",
		"GET /console/api/users/lookup",
		"GET /console/api/status",
		"GET /console/api/chat/folders",
		"GET /console/api/sessions",
	}
	allPanels := kioskUser(nil, true) // nil Panels = inherit every panel
	for _, p := range owner {
		if code := serveAs(g, allPanels, requestFor(p)); code != http.StatusForbidden {
			t.Errorf("%s: status %d for a shard session, want 403", p, code)
		}
	}
}

// A books-only kiosk reaches exactly the book routes (plus the
// maintenance banner) and nothing else.
func TestShardRouteGate_BooksOnlyKiosk(t *testing.T) {
	g := gateOverStub(t)
	kiosk := kioskUser([]string{panelBooks}, false)
	for _, p := range consoleRoutes(t) {
		want := http.StatusForbidden
		if rule, ok := shardRoutes[p]; ok && (len(rule.panels) == 0 || containsString(rule.panels, panelBooks)) && !rule.chat {
			want = http.StatusOK
		}
		if code := serveAs(g, kiosk, requestFor(p)); code != want {
			t.Errorf("%s: status %d, want %d", p, code, want)
		}
	}
	for _, p := range []string{"GET /console/api/memories", "GET /console/api/conversations", "GET /console/api/dashboard/overview"} {
		if code := serveAs(g, kiosk, requestFor(p)); code != http.StatusForbidden {
			t.Errorf("%s: status %d for a books-only kiosk, want 403", p, code)
		}
	}
}

// chat_enabled gates writes even when the chat panel is visible.
func TestShardRouteGate_ChatPanelWithoutChatEnabled(t *testing.T) {
	g := gateOverStub(t)
	kiosk := kioskUser([]string{panelChat}, false)
	if code := serveAs(g, kiosk, requestFor("GET /console/api/conversations")); code != http.StatusOK {
		t.Errorf("list conversations: status %d, want 200", code)
	}
	if code := serveAs(g, kiosk, requestFor("POST /console/api/conversations")); code != http.StatusForbidden {
		t.Errorf("create conversation with chat disabled: status %d, want 403", code)
	}
}

// Unknown paths resolve to no pattern and are refused, not passed on.
func TestShardRouteGate_UnmatchedPathRefused(t *testing.T) {
	g := gateOverStub(t)
	req := httptest.NewRequest("GET", "/console/api/not-a-route", nil)
	if code := serveAs(g, kioskUser(nil, true), req); code != http.StatusForbidden {
		t.Errorf("unmatched path: status %d, want 403", code)
	}
}

// User sessions never consult the gate.
func TestShardRouteGate_UserSessionsUnaffected(t *testing.T) {
	g := gateOverStub(t)
	user := AuthUser{UserID: "owner", Role: "user", PrincipalType: PrincipalTypeUser, PrincipalID: "owner"}
	for _, p := range consoleRoutes(t) {
		if code := serveAs(g, user, requestFor(p)); code != http.StatusOK {
			t.Errorf("%s: status %d for a user session, want 200", p, code)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ── DB-backed: a real shard session cookie through the real chain ───

func shardSessionHarness(t *testing.T) (*Handler, *fakeShardStore) {
	t.Helper()
	pool := testutil.PgTestPool(t)
	st := newFakeShardStore()
	st.addShard(&shards.Shard{
		ID: "kitchen", OwnerID: "kiosk-owner", Name: "Kitchen",
		ConsoleAccess: true, ConsolePanels: []string{panelBooks},
	})
	h := &Handler{sessions: NewSessionStore(pool), shards: st}
	return h, st
}

func withCookie(req *http.Request, token string) *http.Request {
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	return req
}

func TestShardSession_RealChainRefusesOwnerRoutes(t *testing.T) {
	h, _ := shardSessionHarness(t)
	ctx := context.Background()
	token, err := h.sessions.CreateBound(ctx, PrincipalTypeShard, "kitchen", "kiosk-owner", "cred-kiosk", time.Hour)
	if err != nil {
		t.Fatalf("mint shard session: %v", err)
	}
	mux := h.Mux(nil)
	for _, p := range []string{
		"GET /console/api/memories",
		"GET /console/api/conversations",
		"PATCH /console/api/profile",
		"POST /console/api/push/subscribe",
		"POST /console/api/auth/enrollment-token",
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, withCookie(requestFor(p), token))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s via real chain: status %d, want 403 (body: %s)", p, w.Code, w.Body.String())
		}
	}
}

// registerBegin sits outside the authed mux, so the gate can't cover
// it. A shard session must not be able to add a passkey to its owner.
func TestShardSession_CannotRegisterOwnerPasskey(t *testing.T) {
	h, _ := shardSessionHarness(t)
	token, err := h.sessions.CreateBound(context.Background(), PrincipalTypeShard, "kitchen", "kiosk-owner", "cred-kiosk", time.Hour)
	if err != nil {
		t.Fatalf("mint shard session: %v", err)
	}
	if _, ok := h.userSession(withCookie(httptest.NewRequest("POST", "/", nil), token)); ok {
		t.Fatal("userSession accepted a shard session")
	}
	if _, ok := h.authenticatedUser(withCookie(httptest.NewRequest("POST", "/", nil), token)); ok {
		t.Fatal("authenticatedUser accepted a shard session")
	}
	uid, sid, _, ok := h.PrincipalFromRequest(withCookie(httptest.NewRequest("POST", "/api/chat", nil), token))
	if !ok || uid != "kiosk-owner" || sid != "kitchen" {
		t.Fatalf("PrincipalFromRequest = (%q, %q, ok=%v), want the owner plus shard kitchen", uid, sid, ok)
	}
}

// Revoking the shard (deleting it here) must end the session even if
// the same id comes back.
func TestShardSession_DeletedShardRevokesSession(t *testing.T) {
	h, st := shardSessionHarness(t)
	token, err := h.sessions.CreateBound(context.Background(), PrincipalTypeShard, "kitchen", "kiosk-owner", "cred-kiosk", time.Hour)
	if err != nil {
		t.Fatalf("mint shard session: %v", err)
	}
	st.mu.Lock()
	delete(st.shards, "kitchen")
	st.mu.Unlock()
	w := httptest.NewRecorder()
	h.Mux(nil).ServeHTTP(w, withCookie(requestFor("GET /console/api/books"), token))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("deleted shard: status %d, want 401 (body: %s)", w.Code, w.Body.String())
	}
}

// A kiosk with the chat panel sees only threads bound to its own shard.
// Before the fix it listed and read every owner conversation, and "New
// chat" created a trusted thread that ran as the owner.
func TestShardSession_ConversationsConfinedToOwnShard(t *testing.T) {
	pool := testutil.PgTestPool(t)
	cs := NewConversationStore(pool)
	ctx := context.Background()
	owner := fmt.Sprintf("kiosk-conv-owner-%d", time.Now().UnixNano())
	seedUser(t, cs, owner)
	st := newFakeShardStore()
	for _, id := range []string{"kitchen", "garage"} {
		st.addShard(&shards.Shard{ID: id, OwnerID: owner, ChatEnabled: true, ConsoleAccess: true})
	}
	h := &Handler{conversations: cs, shards: st}

	private, err := cs.Create(ctx, owner, "Owner's private thread", "familiar")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	kitchen, err := cs.Create(ctx, owner, "Kitchen thread", "shard:kitchen")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	garage, err := cs.Create(ctx, owner, "Garage thread", "shard:garage")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	kiosk := kioskUser([]string{panelChat}, true)
	kiosk.UserID = owner

	call := func(fn http.HandlerFunc, method, target string, body string, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if id != "" {
			req.SetPathValue("id", id)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req = req.WithContext(ctxWithAuth(req.Context(), kiosk))
		w := httptest.NewRecorder()
		fn(w, req)
		return w
	}

	w := call(h.listConversations, "GET", "/console/api/conversations", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), private.ID) || strings.Contains(w.Body.String(), garage.ID) {
		t.Errorf("kiosk list includes threads outside its shard: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), kitchen.ID) {
		t.Errorf("kiosk list is missing its own thread: %s", w.Body.String())
	}

	for name, id := range map[string]string{"owner's private": private.ID, "another shard's": garage.ID} {
		if w := call(h.getConversation, "GET", "/console/api/conversations/"+id, "", id); w.Code != http.StatusNotFound {
			t.Errorf("get %s thread: status %d, want 404", name, w.Code)
		}
		if w := call(h.listConversationMessages, "GET", "/console/api/conversations/"+id+"/messages", "", id); w.Code != http.StatusNotFound {
			t.Errorf("messages of %s thread: status %d, want 404", name, w.Code)
		}
		if w := call(h.deleteConversation, "DELETE", "/console/api/conversations/"+id, "", id); w.Code != http.StatusNotFound {
			t.Errorf("delete %s thread: status %d, want 404", name, w.Code)
		}
	}
	if w := call(h.getConversation, "GET", "/console/api/conversations/"+kitchen.ID, "", kitchen.ID); w.Code != http.StatusOK {
		t.Errorf("get own thread: status %d, want 200 (%s)", w.Code, w.Body.String())
	}

	w = call(h.createConversation, "POST", "/console/api/conversations", `{"title":"New","model":"familiar"}`, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model":"shard:kitchen"`) {
		t.Errorf("kiosk-created thread isn't bound to its shard: %s", w.Body.String())
	}
}
