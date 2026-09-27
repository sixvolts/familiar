package native

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// With no listen_addr the gateway binds loopback. It used to bind
// 0.0.0.0, serving the whole API over plain HTTP past the workspace.
func TestListenAddr_DefaultsToLoopback(t *testing.T) {
	if got := listenAddr(""); got != "127.0.0.1:8000" {
		t.Errorf("default listen address %q, want 127.0.0.1:8000", got)
	}
	if got := listenAddr("0.0.0.0:9000"); got != "0.0.0.0:9000" {
		t.Errorf("an explicit address was replaced: %q", got)
	}
}

// The request guard (the gateway's CSRF check) wraps every route the
// adapter serves, /api/chat included.
func TestRootHandler_AppliesTheGuard(t *testing.T) {
	a := &Adapter{}
	a.SetRequestGuard(func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	})
	for _, path := range []string{"/api/chat", "/console/api/memories", "/v1/chat/completions"} {
		rec := httptest.NewRecorder()
		a.rootHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s: %d, the guard didn't run", path, rec.Code)
		}
	}
}
