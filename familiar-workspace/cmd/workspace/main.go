// familiar-workspace serves the workspace UI and reverse-proxies API
// calls to familiar-gateway. Per FAMILIAR-WORKSPACE-SPEC, this is
// the user-facing TLS-terminating service; the gateway is API-only
// on localhost.
//
// Routing:
//
//	/v1/*           → reverse proxy → gateway (shard invocation)
//	/console/api/*  → reverse proxy → gateway (auth, data CRUD)
//	/api/*          → reverse proxy → gateway (native chat, health)
//	/events/*       → reverse proxy → gateway (memlog SSE)
//	everything else → static file from disk, with SPA fallback
//	                   to index.html for unknown paths so the
//	                   client-side router owns navigation.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/familiar/workspace/internal/config"
	"github.com/familiar/workspace/internal/proxy"
)

func main() {
	configPath := flag.String("config", "", "path to workspace.toml (default: ./workspace.toml)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("[workspace] failed to load config: %v", err)
	}

	staticDir := cfg.ResolvedStaticDir()
	if _, err := os.Stat(staticDir); err != nil {
		log.Fatalf("[workspace] static dir %s missing: %v", staticDir, err)
	}
	if _, err := os.Stat(filepath.Join(staticDir, "index.html")); err != nil {
		log.Fatalf("[workspace] %s/index.html missing — copy the console assets here before starting", staticDir)
	}

	gatewayProxy, err := proxy.New(cfg.GatewayURL)
	if err != nil {
		log.Fatalf("[workspace] proxy setup: %v", err)
	}

	mux := http.NewServeMux()

	// API surface: every request under these prefixes is forwarded
	// verbatim to the gateway. Order matters — these match before
	// the catch-all static handler.
	mux.Handle("/v1/", gatewayProxy)
	mux.Handle("/console/api/", gatewayProxy)
	mux.Handle("/api/", gatewayProxy)    // native chat (/api/chat) + health (/api/health) — added by CHAT-REARCH S7
	mux.Handle("/events/", gatewayProxy) // memlog SSE (/events/{session_id})
	mux.Handle("/p/", gatewayProxy)      // public page-share render — gateway host-gates by config

	// Back-compat: anyone with a bookmark from the /admin era gets
	// 301'd to the /console equivalent. The redirect used to live
	// in the gateway; it moved here in Phase 0 because URL bookmarks
	// land on the workspace's hostname now, not the gateway.
	mux.HandleFunc("/admin/", redirectAdminToConsole)
	mux.HandleFunc("/admin", redirectAdminToConsole)

	// Cross-domain passkey enrollment landing page
	// (CROSS-DOMAIN-ENROLLMENT.md). Standalone HTML — NOT the SPA
	// shell — because the user isn't authenticated yet. The page
	// reads ?token=... from the URL, calls the gateway's
	// /console/api/auth/enroll/{begin,finish} endpoints, and runs
	// the WebAuthn ceremony inline.
	enrollPath := filepath.Join(staticDir, "enroll.html")
	mux.HandleFunc("/enroll", func(w http.ResponseWriter, r *http.Request) {
		docSecurityHeaders(w)
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		http.ServeFile(w, r, enrollPath)
	})

	// Static + SPA fallback. The handler tries to serve a file from
	// staticDir; if the file doesn't exist OR the requested path
	// has no extension (i.e. it's a client-side route, not an asset),
	// it serves index.html so the SPA can take over. On the bare
	// "/" path (or unknown SPA route), the User-Agent is sniffed so
	// phones get mobile.html and everyone else gets index.html — no
	// client-side redirect, the URL stays the same.
	mux.HandleFunc("/", makeStaticHandler(staticDir))

	log.Printf("[workspace] static_dir=%s gateway=%s", staticDir, cfg.GatewayURL)
	log.Printf("[workspace] listening on %s tls=%v", cfg.ListenAddr, cfg.TLS.Enabled())

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	if cfg.TLS.Enabled() {
		log.Fatal(srv.ListenAndServeTLS(cfg.TLS.Cert, cfg.TLS.Key))
	} else {
		log.Fatal(srv.ListenAndServe())
	}
}

// redirectAdminToConsole 301s legacy /admin/* paths to the /console
// equivalent so bookmarks from before the FAMILIAR-CONSOLE-SPEC
// Phase B rename keep working. Preserves the trailing path + raw
// query so /admin/api/auth/status?foo=1 lands on
// /console/api/auth/status?foo=1.
func redirectAdminToConsole(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/admin")
	target := "/console" + suffix
	if target == "/console" {
		target = "/console/"
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// makeStaticHandler returns the fallthrough handler for everything
// not under /v1/ or /console/api/. It serves files from staticDir
// directly (cache headers + content-type detection courtesy of
// http.ServeFile) and falls back to index.html for unknown paths
// so the client-side router on the workspace SPA can own navigation
// without 404s.
//
// SPA-fallback paths (the bare "/" plus any unknown route) sniff the
// User-Agent and serve mobile.html on phones, index.html on every-
// thing else. Static asset requests (anything ending in .css/.js/
// .svg/etc.) are handed back unchanged so both shells share the same
// favicon, manifest, and font assets.
// docSecurityHeaders sets defense-in-depth headers on HTML document
// responses. The CSP is deliberately permissive on style + eval — the
// vendored TOAST UI editor and Mermaid need inline styles and Function()
// — while still blocking inline <script>/event-handler injection,
// framing (clickjacking), and cross-origin connect/img exfiltration.
// DOMPurify remains the primary XSS defense; this is the second layer.
// Tighten (drop 'unsafe-eval', add nonces) once those deps are CSP-clean.
func docSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'self'; object-src 'none'; "+
			"frame-ancestors 'none'; img-src 'self' data: blob:; "+
			"font-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"script-src 'self' 'unsafe-eval'; worker-src 'self' blob:; "+
			"connect-src 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
}

func makeStaticHandler(staticDir string) http.HandlerFunc {
	indexPath := filepath.Join(staticDir, "index.html")
	mobilePath := filepath.Join(staticDir, "mobile.html")
	hasMobile := false
	if _, err := os.Stat(mobilePath); err == nil {
		hasMobile = true
	}

	pickShell := func(r *http.Request) string {
		if !hasMobile {
			return indexPath
		}
		// Override knobs so devs can force either shell from any
		// device: ?ui=mobile / ?ui=desktop wins over UA sniffing.
		switch r.URL.Query().Get("ui") {
		case "mobile":
			return mobilePath
		case "desktop":
			return indexPath
		}
		if isMobileUA(r.UserAgent()) {
			return mobilePath
		}
		return indexPath
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Resolve the requested path inside staticDir, defending
		// against directory traversal via filepath.Clean + a prefix
		// check. Trailing slash on / serves the right shell per UA.
		rel := strings.TrimPrefix(r.URL.Path, "/")
		if rel == "" {
			docSecurityHeaders(w)
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
			http.ServeFile(w, r, pickShell(r))
			return
		}
		full := filepath.Join(staticDir, filepath.Clean("/"+rel))
		if !strings.HasPrefix(full, staticDir) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// File exists → serve it. Otherwise fall back to a shell so
		// /chat or /notes/foo (client-side routes) resolve to the
		// right SPA — mobile or desktop based on UA.
		info, err := os.Stat(full)
		if err == nil && !info.IsDir() {
			http.ServeFile(w, r, full)
			return
		}
		docSecurityHeaders(w)
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		http.ServeFile(w, r, pickShell(r))
	}
}

// isMobileUA returns true when the User-Agent looks like a phone.
// Tablets are intentionally excluded — iPads have desktop-class
// viewports and modern iPadOS even reports as Macintosh, so they
// land on the desktop shell. The ?ui=mobile query override exists
// for the "I want mobile on my tablet" case.
func isMobileUA(ua string) bool {
	if ua == "" {
		return false
	}
	ua = strings.ToLower(ua)
	for _, needle := range []string{"iphone", "ipod", "android", "mobi", "windows phone", "blackberry", "iemobile", "opera mini"} {
		if strings.Contains(ua, needle) {
			// Android tablets identify as "Android" but omit the
			// "Mobile" token — Chrome's docs document this. So an
			// Android UA without "mobi" should NOT match.
			if needle == "android" && !strings.Contains(ua, "mobi") {
				continue
			}
			return true
		}
	}
	return false
}
