package fetch

import (
	"net"
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

		{"8.8.8.8", false, "public DNS"},
		{"1.1.1.1", false, "public DNS"},
		{"93.184.216.34", false, "public web (example.com)"},
		{"100.64.0.1", false, "Tailscale CGNAT — intentionally allowed"},
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
