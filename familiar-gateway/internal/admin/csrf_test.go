package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/familiar/gateway/internal/config"
)

// A state-changing request that carries the session cookie is refused
// unless it comes from one of the app's own origins. There was no check:
// SameSite=Lax still sends the cookie from a sibling subdomain.
func TestCSRFGuard(t *testing.T) {
	h := &Handler{cfg: config.AdminConfig{RPID: "familiar.example", RPOrigins: []string{"https://familiar.example"}}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	guard := h.CSRFGuard(ok)
	for _, tc := range []struct {
		name, method, origin, fetchSite string
		cookie                          bool
		want                            int
	}{
		{"sibling subdomain with the cookie", "POST", "https://evil.familiar.example", "", true, 403},
		{"delete from another site", "DELETE", "https://attacker.example", "", true, 403},
		{"opaque origin", "PATCH", "null", "", true, 403},
		{"no Origin, browser says cross-site", "POST", "", "same-site", true, 403},
		{"the app itself", "POST", "https://familiar.example", "same-origin", true, 200},
		{"the app, trailing slash, other case", "POST", "HTTPS://Familiar.example/", "", true, 200},
		{"a script without Origin", "POST", "", "", true, 200},
		{"no session cookie (API key, webhook)", "POST", "https://attacker.example", "", false, 200},
		{"a read", "GET", "https://attacker.example", "", true, 200},
	} {
		req := httptest.NewRequest(tc.method, "/console/api/memories/x", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
		}
		if tc.cookie {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
		}
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
