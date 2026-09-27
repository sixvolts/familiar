package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/testutil"
)

// routeAuthzHarness is the real console route table behind the real
// auth chain, with a plain user's and an admin's session.
func routeAuthzHarness(t *testing.T) (http.Handler, *Handler, string) {
	t.Helper()
	pool := testutil.PgTestPool(t)
	h := &Handler{
		sessions: NewSessionStore(pool),
		users: &fakeUserManager{users: map[string]*identity.User{
			"route-sam":  {ID: "route-sam", Role: "user", Status: identity.StatusApproved},
			"route-boss": {ID: "route-boss", Role: "admin", Status: identity.StatusApproved},
		}, adminCount: -1},
	}
	userTok, err := h.sessions.Create(context.Background(), "route-sam", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return h.Mux(nil), h, userTok
}

// serve runs one request, treating a panic past the gates (a handler
// meeting a dependency this harness doesn't wire) as "got through".
func serve(mux http.Handler, req *http.Request) (rec *httptest.ResponseRecorder) {
	rec = httptest.NewRecorder()
	defer func() { _ = recover() }()
	mux.ServeHTTP(rec, req)
	return rec
}

func consoleRequest(pattern, token string) *http.Request {
	req := requestFor(pattern)
	req.Body = http.NoBody
	if token != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	}
	return req
}

// adminRoutes is every console route that requires the admin role. A
// route dropping its adminOnly wrapper (or one added without it that
// belongs here) fails TestRouteTable_AdminRoutes.
var adminRoutes = []string{
	"DELETE /console/api/skillpacks/{id}",
	"DELETE /console/api/users/{id}/enrollment-tokens",
	"DELETE /console/api/users/{id}/identities/{platform}/{platform_id}",
	"GET /console/api/controls/backfill-relationships",
	"GET /console/api/status",
	"GET /console/api/users",
	"GET /console/api/users/{id}",
	"PATCH /console/api/users/{id}",
	"POST /console/api/controls/backfill-relationships",
	"POST /console/api/maintenance",
	"POST /console/api/skillpacks/import",
	"POST /console/api/skillpacks/rescan",
	"POST /console/api/skillpacks/{id}/chat",
	"POST /console/api/skillpacks/{id}/disable",
	"POST /console/api/skillpacks/{id}/enable",
	"POST /console/api/users",
	"POST /console/api/users/{id}/identities",
	"POST /console/api/users/{id}/status",
	"PUT /console/api/system-prompt",
}

// Every console route refuses an anonymous caller.
func TestRouteTable_AnonymousIsRefusedEverywhere(t *testing.T) {
	mux, h, _ := routeAuthzHarness(t)
	if len(h.consolePatterns) < 50 {
		t.Fatalf("only %d console routes recorded", len(h.consolePatterns))
	}
	for _, p := range h.consolePatterns {
		if rec := serve(mux, consoleRequest(p, "")); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: %d, want 401", p, rec.Code)
		}
	}
}

// Exactly the admin routes refuse a signed-in non-admin at the admin
// gate. Before this, adminOnly was tested as middleware only, so a
// route that lost its wrapper stayed green.
func TestRouteTable_AdminRoutes(t *testing.T) {
	mux, h, userTok := routeAuthzHarness(t)
	var gated []string
	for _, p := range h.consolePatterns {
		rec := serve(mux, consoleRequest(p, userTok))
		if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "admin role required") {
			gated = append(gated, p)
		}
	}
	sort.Strings(gated)
	want := append([]string(nil), adminRoutes...)
	sort.Strings(want)
	if strings.Join(gated, "\n") != strings.Join(want, "\n") {
		t.Errorf("admin-gated routes changed.\n got:\n  %s\nwant:\n  %s", strings.Join(gated, "\n  "), strings.Join(want, "\n  "))
	}
}
