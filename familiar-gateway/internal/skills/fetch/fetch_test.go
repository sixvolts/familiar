package fetch

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// TestIsBlockedIP is the SSRF guard's truth table. The blocked rows
// are the ones a prompt-injection would aim fetch_page at; the
// allowed rows must keep working (public web + the Tailscale CGNAT
// range this deployment uses).
func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
		why     string
	}{
		{"169.254.169.254", true, "cloud metadata endpoint (link-local)"},
		{"127.0.0.1", true, "loopback"},
		{"::1", true, "loopback v6"},
		{"10.1.2.3", true, "RFC1918 10/8"},
		{"172.16.5.5", true, "RFC1918 172.16/12"},
		{"192.168.1.1", true, "RFC1918 192.168/16"},
		{"fc00::1", true, "IPv6 ULA"},
		{"fe80::1", true, "IPv6 link-local"},
		{"0.0.0.0", true, "unspecified"},
		{"224.0.0.1", true, "multicast"},

		{"100.64.0.1", true, "CGNAT (Tailscale): the host's own tailnet address reaches its local services"},
		{"100.100.100.100", true, "Tailscale MagicDNS"},
		{"::ffff:100.64.0.1", true, "CGNAT, IPv4-mapped"},
		{"::ffff:127.0.0.1", true, "loopback, IPv4-mapped"},
		{"0.1.2.3", true, "0.0.0.0/8"},
		{"198.18.0.1", true, "benchmark range"},
		{"240.0.0.1", true, "reserved"},
		{"255.255.255.255", true, "broadcast"},
		{"64:ff9b::a00:1", true, "NAT64 of 10.0.0.1"},
		{"2001:db8::1", true, "IPv6 documentation"},

		{"8.8.8.8", false, "public DNS"},
		{"1.1.1.1", false, "public DNS"},
		{"93.184.216.34", false, "public web (example.com)"},
		{"2606:4700:4700::1111", false, "public IPv6"},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := isBlockedIP(ip); got != c.blocked {
			t.Errorf("isBlockedIP(%s) = %v, want %v (%s)", c.ip, got, c.blocked, c.why)
		}
	}
}

// An operator can allow specific non-public ranges (a tailnet host that
// should be fetchable); nothing else opens up.
func TestIsBlockedIP_OperatorAllowlist(t *testing.T) {
	SetAllowedCIDRs([]netip.Prefix{netip.MustParsePrefix("100.101.102.103/32")})
	defer SetAllowedCIDRs(nil)
	if isBlockedIP(net.ParseIP("100.101.102.103")) {
		t.Error("an allowed tailnet host is blocked")
	}
	if !isBlockedIP(net.ParseIP("100.101.102.104")) {
		t.Error("the allowlist opened more than the listed host")
	}
}

// A connection-level failure doesn't say why: distinct "refused",
// "timed out" and "blocked" messages let a caller map which internal
// hosts and ports answer.
func TestFetchPage_ConnectionErrorsAreGeneric(t *testing.T) {
	s := New()
	res, err := s.Execute(context.Background(), "fetch_page", json.RawMessage(`{"url":"http://127.0.0.1:9/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != "fetch failed: could not retrieve the page" {
		t.Errorf("error = %q, want the generic message", res.Error)
	}
}

// servers starts a "public" page server on 127.0.0.1 (allowlisted for
// the test, as an operator would a host) and an internal one on [::1]
// (blocked), which the public one can redirect to.
func servers(t *testing.T, public http.HandlerFunc) (pub *httptest.Server, internal *httptest.Server, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	internal = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("<html><body>internal secret</body></html>"))
	}))
	internal.Listener = ln
	internal.Start()
	t.Cleanup(internal.Close)
	pub = httptest.NewServer(public)
	t.Cleanup(pub.Close)
	SetAllowedCIDRs([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	t.Cleanup(func() { SetAllowedCIDRs(nil) })
	return pub, internal, hits
}

// The dial guard, end to end through the real transport: an internal
// address is refused directly and as a redirect target. Only the IP
// table was tested; a transport change (a proxy, another dialer) could
// have dropped the guard with every test passing.
func TestSafeTransport_RefusesInternalDirectAndViaRedirect(t *testing.T) {
	var internalURL string
	pub, internal, hits := servers(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, internalURL, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("<html><head><title>Public</title></head><body>hello</body></html>"))
	})
	internalURL = internal.URL + "/"
	s := New()

	if title, _, err := s.fetchAndExtract(context.Background(), pub.URL+"/"); err != nil || title != "Public" {
		t.Fatalf("allowed page: title=%q err=%v", title, err)
	}
	for _, u := range []string{internalURL, pub.URL + "/redirect"} {
		_, _, err := s.fetchAndExtract(context.Background(), u)
		if err == nil || !strings.Contains(err.Error(), "refusing to connect to non-public address") {
			t.Errorf("fetch %s: err = %v, want the dial refusal", u, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the internal server was reached %d times", n)
	}
}

// Pages are decoded from their charset — the Content-Type's, else a
// <meta charset> — instead of being read as UTF-8 (mojibake).
func TestFetchPage_DecodesCharset(t *testing.T) {
	body := strings.Repeat("filler text ", 30)
	pub, _, _ := servers(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latin1":
			w.Header().Set("Content-Type", "text/html; charset=windows-1252")
			_, _ = w.Write([]byte("<html><head><title>Caf\xe9</title></head><body><article>na\xefve " + body + "</article></body></html>"))
		case "/sjis":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><meta charset="shift_jis"><title>` + "\x93\xfa\x96\x7b" + `</title></head><body><article>` + body + `</article></body></html>`))
		}
	})
	s := New()
	for path, want := range map[string]string{"/latin1": "Café", "/sjis": "日本"} {
		title, text, err := s.fetchAndExtract(context.Background(), pub.URL+path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if title != want {
			t.Errorf("%s: title = %q, want %q", path, title, want)
		}
		if path == "/latin1" && !strings.Contains(text, "naïve") {
			t.Errorf("%s: body not decoded: %q", path, text[:40])
		}
	}
}
