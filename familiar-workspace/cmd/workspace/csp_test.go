package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shells' CSP keeps forms from posting off-site: sanitized content
// can't carry a <form> any more, and this is the layer behind that.
func TestDocSecurityHeaders_FormActionSelf(t *testing.T) {
	w := httptest.NewRecorder()
	docSecurityHeaders(w)
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("CSP lacks form-action 'self': %s", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP lost frame-ancestors: %s", csp)
	}
}

// An HTML file requested by name gets the shell's headers. /mobile.html
// and /enroll.html used to go out through plain ServeFile, with no CSP
// and frameable.
func TestStaticHandler_HTMLByNameGetsShellHeaders(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"index.html", "mobile.html", "enroll.html"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("<!doctype html><title>"+f+"</title>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := makeStaticHandler(dir, newAssetStamper(dir))
	for _, path := range []string{"/mobile.html", "/enroll.html", "/index.html", "/MOBILE.HTML"} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, path, nil))
		if path == "/MOBILE.HTML" && w.Code == http.StatusNotFound {
			continue // case-sensitive filesystem: no such file, nothing served
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: status %d, no CSP", path, w.Code)
		}
	}
	// Other assets are served as before, without the document headers.
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") != "" {
		t.Errorf("/app.js: status %d, CSP %q", w.Code, w.Header().Get("Content-Security-Policy"))
	}
}
