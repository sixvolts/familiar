package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pre-SIDECAR-SLOT-FIXES config with only [sidecar].router_endpoint
// validated as configured while every task had no model (the client
// routes only through role chains): no classifier, no extraction, no
// summaries. The endpoint becomes a model that every unassigned task
// uses, in the small slot so chat is never derived onto it.
func TestNormalizeRoles_RouterEndpointBecomesTheTasksModel(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{}
	c.Models = []ModelConfig{{ID: "gpu/big", Provider: "llama-server", Endpoint: "http://big"}}
	c.Sidecar = SidecarConfig{Enabled: true, RouterEndpoint: "http://127.0.0.1:8200"}
	c.normalizeRoles()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var legacy *ModelConfig
	for i := range c.Models {
		if c.Models[i].ID == legacyRouterModelID {
			legacy = &c.Models[i]
		}
	}
	if legacy == nil || legacy.Endpoint != "http://127.0.0.1:8200" || legacy.Role != ModelSlotSmall {
		t.Fatalf("legacy model = %+v, want the router endpoint in the small slot", legacy)
	}
	for _, role := range []string{RoleClassify, RoleExpandQueries, RoleExtract, RoleSummarize, RoleConflict, RoleRelationship, RoleEntityGroup} {
		if got := c.Roles.Chain(role).Primary; got != legacyRouterModelID {
			t.Errorf("%s → %q, want %s", role, got, legacyRouterModelID)
		}
	}
	if got := c.Roles.Chat.Primary; got != "gpu/big" {
		t.Errorf("chat → %q, want gpu/big (never the sidecar)", got)
	}

	// A config with its own small-slot model is left alone.
	c2 := DefaultConfig()
	c2.Embedder = EmbedderConfig{}
	c2.Models = []ModelConfig{{ID: "sc/small", Provider: "llama-server", Endpoint: "http://s", Role: ModelSlotSmall}}
	c2.Sidecar = SidecarConfig{Enabled: true, RouterEndpoint: "http://other"}
	c2.normalizeRoles()
	if len(c2.Models) != 1 || c2.Roles.Classify.Primary != "sc/small" {
		t.Errorf("models %+v, classify %q: the declared small model must win", c2.Models, c2.Roles.Classify.Primary)
	}
}

// Keys that are parsed but read by nothing are named when the file sets
// them (and not when only the defaults do), so Validate warns instead of
// letting an operator believe they work.
func TestIgnoredKnobs(t *testing.T) {
	dir := t.TempDir()
	load := func(body string) *Config {
		path := filepath.Join(dir, "c.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := load("[router]\nenabled = true\n").ignored; len(got) != 0 {
		t.Errorf("a config setting none reports %v", got)
	}
	got := strings.Join(load(`[router]
use_sidecar_router = false
[[router.rules.force]]
pattern = "x"
model = "m"
[sidecar]
fallback_on_failure = true
connect_timeout_ms = 500
`).ignored, "|")
	for _, want := range []string{"[router].use_sidecar_router", "[router.rules].force", "[sidecar].fallback_on_failure", "[sidecar].connect_timeout_ms"} {
		if !strings.Contains(got, want) {
			t.Errorf("ignored knobs %q lack %s", got, want)
		}
	}
}
