// Package proxy is the reverse-proxy half of familiar-workspace.
// It forwards /v1/* (LLM completions, shard invocation) and
// /console/api/* (auth, data CRUD) to familiar-gateway.
//
// The single critical detail here is Host-header preservation:
// WebAuthn registration and assertion bind credentials to the
// browser's origin (the workspace's hostname), and the gateway
// validates the assertion against its configured RP ID. If the
// proxy rewrites Host to "localhost:8000" before forwarding, the
// gateway sees the wrong origin and rejects every login. So we
// override httputil.NewSingleHostReverseProxy's default Director
// to keep the inbound Host as the gateway sees it.
//
// Cookies set by the gateway (admin session, pending-ceremony)
// flow back through the proxy to the browser unmodified. Because
// the browser sees one origin (the workspace's), the cookies'
// Domain attribute matches and SameSite=Lax stays valid.
package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// New returns an http.Handler that proxies every request to
// targetURL (typically the gateway's localhost:8000). Returns an
// error if targetURL is empty or unparseable.
//
// The returned handler preserves the inbound Host header on the
// outbound request so the gateway's WebAuthn library matches the
// configured RP origin. Without this, passkey login through the
// proxy returns "origin mismatch" errors during assertion
// validation.
func New(targetURL string) (http.Handler, error) {
	if targetURL == "" {
		return nil, fmt.Errorf("proxy: empty target url")
	}
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse %q: %w", targetURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("proxy: target must include scheme and host, got %q", targetURL)
	}

	rp := httputil.NewSingleHostReverseProxy(target)

	// Flush SSE events immediately — without this, the proxy buffers
	// the entire response body and streaming doesn't work.
	rp.FlushInterval = -1

	// Single Director that:
	//   1. Captures the browser-visible Host BEFORE httputil's
	//      default rewrites it to target.Host. WebAuthn RP-origin
	//      checks fail without this — the gateway would see
	//      "localhost:8000" and reject against its rp_origins list.
	//   2. Snapshots the client's IP + scheme for X-Forwarded-*
	//      headers so the gateway's logs name the real client, not
	//      the proxy.
	//   3. Runs httputil's default Director (rewrites scheme + host).
	//   4. Restores Host + sets the X-Forwarded headers.
	originalDirector := rp.Director
	rp.Director = func(r *http.Request) {
		inboundHost := r.Host
		clientIP := r.RemoteAddr
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		originalDirector(r)
		r.Host = inboundHost
		if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
			r.Header.Set("X-Forwarded-For", prior+", "+clientIP)
		} else {
			r.Header.Set("X-Forwarded-For", clientIP)
		}
		r.Header.Set("X-Forwarded-Proto", scheme)
		r.Header.Set("X-Forwarded-Host", inboundHost)

		// Strip client-supplied identity headers before forwarding.
		// The gateway's /api/chat handler historically accepted
		// X-User-Email as a caller identity. Allowing browsers /
		// curl users to set this header through the public proxy
		// is an authentication bypass — the gateway only resolves
		// "is this email approved?", never "did this caller prove
		// ownership of the email?". The session cookie is the
		// trusted identity signal through this proxy; identity
		// headers are scrubbed so the gateway must rely on it.
		// Direct-to-gateway callers (CLI / dev) are unaffected
		// because they bypass this proxy.
		for _, h := range []string{
			"X-User-Email",
			"X-User-Id",
			"X-User-ID",
			"X-Sender-Id",
			"X-Sender-ID",
			"X-Familiar-User",
			"X-Familiar-Sender",
		} {
			r.Header.Del(h)
		}
	}

	// ErrorHandler runs when the gateway is unreachable or returns a
	// transport error. Return 502 with a one-line message so curl
	// users + browsers both see something sensible — better than
	// httputil's silent connection drop.
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "workspace: gateway unreachable at %s: %v\n", target, err)
	}

	return rp, nil
}
