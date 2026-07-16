package admin

// Multi-RP routing tests (PUBLIC-PROXY-MIGRATION).
//
// Coverage:
//   - stripPort: drops :port, preserves IPv6 brackets, no-op on bare host
//   - AdminConfig.EffectiveRelyingParties: legacy fallback synthesises one RP
//   - Handler.webauthnFor: routes by inbound Host (case-insensitive,
//     port-stripped), rejects unmatched hosts
//
// Full HTTP round-trip with two RPs answering /console/api/auth/login/begin
// belongs to the integration suite — the handler entry points need a live
// CredentialStore which the unit tests in this package don't construct.

import (
	"net/http/httptest"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/go-webauthn/webauthn/webauthn"
)

func TestStripPort(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"host-a.familiar.wiki", "host-a.familiar.wiki"},
		{"host-a.familiar.wiki:8000", "host-a.familiar.wiki"},
		{"host-a.your-tailnet.ts.net", "host-a.your-tailnet.ts.net"},
		{"localhost:8081", "localhost"},
		{"[::1]:8000", "[::1]"},
		{"[::1]", "[::1]"},
	}
	for _, tc := range cases {
		if got := stripPort(tc.in); got != tc.want {
			t.Errorf("stripPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveRelyingParties_NewShape(t *testing.T) {
	cfg := config.AdminConfig{
		RelyingParties: []config.RelyingPartyConfig{
			{RPID: "familiar.wiki", Origins: []string{"https://host-a.familiar.wiki"}, Hosts: []string{"host-a.familiar.wiki"}},
			{RPID: "host-a.your-tailnet.ts.net", Origins: []string{"https://host-a.your-tailnet.ts.net"}, Hosts: []string{"host-a.your-tailnet.ts.net"}},
		},
	}
	rps := cfg.EffectiveRelyingParties()
	if len(rps) != 2 {
		t.Fatalf("got %d RPs, want 2", len(rps))
	}
	if rps[0].RPID != "familiar.wiki" || rps[1].RPID != "host-a.your-tailnet.ts.net" {
		t.Errorf("RP order or values wrong: %+v", rps)
	}
}

func TestEffectiveRelyingParties_LegacyFallback(t *testing.T) {
	cfg := config.AdminConfig{
		RPID:      "host-a.your-tailnet.ts.net",
		RPOrigins: []string{"https://host-a.your-tailnet.ts.net", "https://host-a.your-tailnet.ts.net:8443"},
	}
	rps := cfg.EffectiveRelyingParties()
	if len(rps) != 1 {
		t.Fatalf("got %d RPs, want 1 from legacy fallback", len(rps))
	}
	rp := rps[0]
	if rp.RPID != "host-a.your-tailnet.ts.net" {
		t.Errorf("rp_id = %q", rp.RPID)
	}
	if len(rp.Origins) != 2 {
		t.Errorf("origins = %v", rp.Origins)
	}
	// Hosts deduped + port-stripped from both origins → single entry.
	if len(rp.Hosts) != 1 || rp.Hosts[0] != "host-a.your-tailnet.ts.net" {
		t.Errorf("hosts = %v, want [host-a.your-tailnet.ts.net]", rp.Hosts)
	}
}

func TestEffectiveRelyingParties_PrefersNewShapeOverLegacy(t *testing.T) {
	// When both forms are present, the new-shape list wins and the
	// legacy fields are ignored entirely.
	cfg := config.AdminConfig{
		RPID:      "legacy.example.com",
		RPOrigins: []string{"https://legacy.example.com"},
		RelyingParties: []config.RelyingPartyConfig{
			{RPID: "new.example.com", Origins: []string{"https://new.example.com"}, Hosts: []string{"new.example.com"}},
		},
	}
	rps := cfg.EffectiveRelyingParties()
	if len(rps) != 1 || rps[0].RPID != "new.example.com" {
		t.Errorf("expected new-shape RP to win, got %+v", rps)
	}
}

func TestEffectiveRelyingParties_EmptyReturnsNil(t *testing.T) {
	cfg := config.AdminConfig{}
	if rps := cfg.EffectiveRelyingParties(); len(rps) != 0 {
		t.Errorf("empty config should return zero RPs, got %+v", rps)
	}
}

func TestWebauthnFor_Routing(t *testing.T) {
	// Build two real *webauthn.WebAuthn instances so the assertion
	// at the end can compare pointers — we don't need the RP to be
	// network-functional, just distinguishable.
	waA, err := webauthn.New(&webauthn.Config{
		RPID:                 "familiar.wiki",
		RPDisplayName:        "Familiar",
		RPOrigins:            []string{"https://host-a.familiar.wiki"},
		EncodeUserIDAsString: true,
	})
	if err != nil {
		t.Fatalf("waA: %v", err)
	}
	waB, err := webauthn.New(&webauthn.Config{
		RPID:                 "host-a.your-tailnet.ts.net",
		RPDisplayName:        "Familiar",
		RPOrigins:            []string{"https://host-a.your-tailnet.ts.net"},
		EncodeUserIDAsString: true,
	})
	if err != nil {
		t.Fatalf("waB: %v", err)
	}
	h := &Handler{
		rps: map[string]*webauthn.WebAuthn{
			"host-a.familiar.wiki":       waA,
			"host-a.your-tailnet.ts.net": waB,
		},
	}

	cases := []struct {
		host string
		want *webauthn.WebAuthn
		ok   bool
	}{
		{"host-a.familiar.wiki", waA, true},
		{"HOST-A.FAMILIAR.WIKI", waA, true},      // case-insensitive
		{"host-a.familiar.wiki:8443", waA, true}, // port stripped
		{"host-a.your-tailnet.ts.net", waB, true},
		{"unknown.example.com", nil, false},
		{"", nil, false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/console/api/auth/login/begin", nil)
		req.Host = tc.host
		got, err := h.webauthnFor(req)
		if tc.ok && err != nil {
			t.Errorf("host=%q: unexpected error %v", tc.host, err)
			continue
		}
		if !tc.ok && err == nil {
			t.Errorf("host=%q: expected error, got %v", tc.host, got)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("host=%q: routed to wrong RP", tc.host)
		}
	}
}
