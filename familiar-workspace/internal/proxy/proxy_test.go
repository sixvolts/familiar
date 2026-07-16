package proxy

// Reverse-proxy regression guard: the workspace MUST forward the
// inbound Host header verbatim. WebAuthn registration + assertion
// validation depend on it (the gateway's library compares the
// incoming Origin to its rp_origins list). If this test breaks,
// passkey login through the workspace breaks.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPreservesHostHeader stands up a fake "gateway" httptest server
// that records the Host header it receives. The proxy is pointed at
// that server; the test request carries Host=host-a.example. Assert
// the gateway saw the same.
func TestPreservesHostHeader(t *testing.T) {
	var sawHost string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()

	p, err := New(gateway.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/console/api/auth/status", nil)
	req.Host = "host-a.example.test"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if sawHost != "host-a.example.test" {
		t.Errorf("gateway saw Host=%q, want host-a.example.test (proxy rewrote it)", sawHost)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestSetsForwardedHeaders ensures X-Forwarded-{For,Proto,Host} are
// populated so gateway logs name the real client rather than the
// proxy's loopback connection.
func TestSetsForwardedHeaders(t *testing.T) {
	var got http.Header
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()

	p, err := New(gateway.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/anything", nil)
	req.Host = "host-a.example.test"
	req.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if got.Get("X-Forwarded-For") == "" {
		t.Error("X-Forwarded-For not set")
	}
	if !strings.Contains(got.Get("X-Forwarded-For"), "203.0.113.7") {
		t.Errorf("X-Forwarded-For = %q, want it to contain client IP", got.Get("X-Forwarded-For"))
	}
	if got.Get("X-Forwarded-Proto") == "" {
		t.Error("X-Forwarded-Proto not set")
	}
	if got.Get("X-Forwarded-Host") != "host-a.example.test" {
		t.Errorf("X-Forwarded-Host = %q, want host-a.example.test", got.Get("X-Forwarded-Host"))
	}
}

// TestErrorHandlerOn502 verifies the proxy returns 502 with a
// readable message when the gateway is unreachable, instead of
// httputil's silent connection-drop default.
func TestErrorHandlerOn502(t *testing.T) {
	// Point at a port nothing is listening on. Closing the test
	// server immediately after capturing its URL gives us a stable
	// "connection refused" target.
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := gateway.URL
	gateway.Close()

	p, err := New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/foo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "gateway unreachable") {
		t.Errorf("body = %q, want it to mention 'gateway unreachable'", string(body))
	}
}

func TestNew_RejectsBadInputs(t *testing.T) {
	cases := []string{
		"",
		"not-a-url",
		"://malformed",
		"localhost:8000", // no scheme
	}
	for _, in := range cases {
		if _, err := New(in); err == nil {
			t.Errorf("New(%q) succeeded, want error", in)
		}
	}
}

// TestStripsIdentityHeaders verifies the proxy drops every
// client-supplied identity header before forwarding to the gateway.
// The gateway's /api/chat handler historically trusted X-User-Email
// as caller identity; allowing it through the public proxy would
// be an authentication bypass.
func TestStripsIdentityHeaders(t *testing.T) {
	var got http.Header
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()

	p, err := New(gateway.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/chat", nil)
	req.Host = "host-a.example.test"
	headers := []string{
		"X-User-Email",
		"X-User-Id",
		"X-User-ID",
		"X-Sender-Id",
		"X-Sender-ID",
		"X-Familiar-User",
		"X-Familiar-Sender",
	}
	for _, h := range headers {
		req.Header.Set(h, "victim@example.com")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, h := range headers {
		if v := got.Get(h); v != "" {
			t.Errorf("gateway saw %s=%q, want stripped", h, v)
		}
	}
}
