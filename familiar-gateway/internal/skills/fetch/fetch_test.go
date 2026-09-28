package fetch

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
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
