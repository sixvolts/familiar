package admin

// Public-share helper tests: the share-key shape gate, key
// generation, the host gate, URL composition, and — the part that
// matters most — the two-pass markdown sanitization that stands
// between attacker-controlled note content and an anonymous
// browser. The E2E share spec proves the wiring; these pin the
// pure functions so a refactor can't quietly widen them.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/config"
)

func TestIsValidShareKey(t *testing.T) {
	cases := []struct {
		key string
		ok  bool
	}{
		{"AAAAbbbbCCCC1234", true},
		{"abcdefghij123456", true},
		{"", false},
		{"short", false},
		{"AAAAbbbbCCCC123", false},   // 15
		{"AAAAbbbbCCCC12345", false}, // 17
		{"AAAAbbbbCCCC123-", false},  // symbol
		{"AAAAbbbbCCCC123 ", false},  // space
		{"AAAAbbbbCCCC12\x004", false},
		{"ααααββββγγγγδδδδ", false}, // non-ASCII, 16 runes but not bytes/alnum
	}
	for _, c := range cases {
		if got := isValidShareKey(c.key); got != c.ok {
			t.Errorf("isValidShareKey(%q) = %v, want %v", c.key, got, c.ok)
		}
	}
}

func TestNewShareKeyShapeAndUniqueness(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		k, err := newShareKey()
		if err != nil {
			t.Fatalf("newShareKey: %v", err)
		}
		if !isValidShareKey(k) {
			t.Fatalf("generated key %q fails its own validator", k)
		}
		if seen[k] {
			t.Fatalf("duplicate key %q within 200 draws", k)
		}
		seen[k] = true
	}
}

func TestIsPublicHost(t *testing.T) {
	h := &Handler{}
	h.AttachSharing(config.SharingConfig{PublicHosts: []string{"Share.Example.COM", " spaced.example.com ", "ported.example.com:8443"}})

	cases := []struct {
		host string
		ok   bool
	}{
		{"share.example.com", true},
		{"SHARE.EXAMPLE.COM", true},       // case-insensitive
		{"share.example.com:443", true},   // inbound port stripped
		{"spaced.example.com", true},      // config whitespace trimmed
		{"ported.example.com", true},      // config port stripped too
		{"ported.example.com:9999", true}, // matching is host-only by design
		{"evil.example.com", false},
		{"share.example.com.evil.com", false},
		{"", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/p/AAAAbbbbCCCC1234", nil)
		r.Host = c.host
		if got := h.isPublicHost(r); got != c.ok {
			t.Errorf("isPublicHost(host=%q) = %v, want %v", c.host, got, c.ok)
		}
	}

	// Empty allow-list means sharing is off — every host refused.
	off := &Handler{}
	off.AttachSharing(config.SharingConfig{})
	r := httptest.NewRequest("GET", "/p/x", nil)
	r.Host = "share.example.com"
	if off.isPublicHost(r) {
		t.Error("empty allow-list must refuse every host")
	}
}

func TestPublicShareURL(t *testing.T) {
	h := &Handler{}
	h.AttachSharing(config.SharingConfig{PublicBaseURL: "https://share.example.com/"})
	if got := h.publicShareURL("AAAAbbbbCCCC1234"); got != "https://share.example.com/p/AAAAbbbbCCCC1234" {
		t.Errorf("with base: %q", got)
	}
	bare := &Handler{}
	bare.AttachSharing(config.SharingConfig{})
	if got := bare.publicShareURL("AAAAbbbbCCCC1234"); got != "/p/AAAAbbbbCCCC1234" {
		t.Errorf("without base: %q", got)
	}
}

func TestRenderShareMarkdown_StripsActiveContent(t *testing.T) {
	// Blank lines between probes keep each one its own CommonMark
	// block — consecutive raw-HTML lines would otherwise merge into
	// a single HTML block that swallows the following markdown.
	hostile := strings.Join([]string{
		"# Title",
		"<script>alert(1)</script>",
		`<img src="x" onerror="alert(2)">`,
		`<iframe src="https://evil.example"></iframe>`,
		"[js link](javascript:alert(3))",
		"[data link](data:text/html;base64,PHNjcmlwdD4=)",
		`<a href="https://ok.example" onclick="alert(4)">ok</a>`,
		"plain **bold** survives",
	}, "\n\n")

	out := string(renderShareMarkdown(hostile))

	for _, banned := range []string{"<script", "onerror", "<iframe", "javascript:", "data:text/html", "onclick"} {
		if strings.Contains(strings.ToLower(out), banned) {
			t.Errorf("rendered output still contains %q:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "<strong>bold</strong>") {
		t.Errorf("benign markdown should survive sanitization:\n%s", out)
	}
}

func TestRenderShareMarkdown_LinkHardening(t *testing.T) {
	out := string(renderShareMarkdown("[home](https://example.com) and http://plain.example.com"))

	if !strings.Contains(out, `href="https://example.com"`) {
		t.Fatalf("expected the https link to survive:\n%s", out)
	}
	for _, required := range []string{"nofollow", "noreferrer", `target="_blank"`} {
		if !strings.Contains(out, required) {
			t.Errorf("fully-qualified links must carry %s:\n%s", required, out)
		}
	}
	// GFM autolink keeps plain http URLs rendering (the policy allows
	// http so legacy links don't break).
	if !strings.Contains(out, "plain.example.com") {
		t.Errorf("plain http autolink should render:\n%s", out)
	}
}

func TestRenderShareMarkdown_GFMTables(t *testing.T) {
	out := string(renderShareMarkdown("| a | b |\n|---|---|\n| 1 | 2 |"))
	if !strings.Contains(out, "<table>") || !strings.Contains(out, "<td>1</td>") {
		t.Errorf("GFM table should render:\n%s", out)
	}
}
