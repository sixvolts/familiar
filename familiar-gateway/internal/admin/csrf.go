package admin

import (
	"net/http"
	"strings"
)

// CSRFGuard refuses a state-changing request that carries the session
// cookie but comes from a page on another origin.
//
// The cookie is SameSite=Lax, which stops other SITES but not other
// origins of the same site: a page on a sibling subdomain could POST,
// PATCH or DELETE with the victim's cookie attached (a text/plain body
// needs no preflight, and handlers decode JSON whatever the
// Content-Type). Browsers send Origin on every such request, so an
// Origin that isn't one of the relying parties' origins is refused.
//
// Requests without the session cookie (bearer API keys, webhook tokens,
// scripts) aren't cross-site request forgery and pass untouched, as do
// safe methods. Wrap the gateway's whole mux with it: /api/chat takes
// the same cookie.
func (h *Handler) CSRFGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if _, err := r.Cookie(sessionCookieName); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			// No Origin but the browser says another site sent it.
			if s := r.Header.Get("Sec-Fetch-Site"); s == "cross-site" || s == "same-site" {
				writeJSONError(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !h.allowedOrigin(origin) {
			writeJSONError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedOrigin reports whether origin is one of the configured relying
// parties' origins (the only pages that serve the app).
func (h *Handler) allowedOrigin(origin string) bool {
	origin = strings.TrimRight(origin, "/")
	for _, rp := range h.cfg.EffectiveRelyingParties() {
		for _, o := range rp.Origins {
			if strings.EqualFold(strings.TrimRight(o, "/"), origin) {
				return true
			}
		}
	}
	return false
}
