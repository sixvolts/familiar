package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog collects what the package logs while f runs.
func captureLog(f func()) string {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	f()
	return buf.String()
}

// $$ is a literal $, and a $ naming no set variable is reported by
// field, not value. A password containing $ lost characters silently,
// while the example said nothing was expanded.
func TestExpandEnv_EscapeAndWarning(t *testing.T) {
	t.Setenv("FAMILIAR_TEST_PART", "v")
	if got := expandEnv("f", "a$$b${FAMILIAR_TEST_PART}"); got != "a$bv" {
		t.Errorf("expandEnv = %q, want a$bv", got)
	}
	var got string
	out := captureLog(func() { got = expandEnv("memory.local_dsn", "postgresql://u:pa$$w0rd$ecret@h/db") })
	if got != "postgresql://u:pa$w0rd@h/db" {
		t.Errorf("expandEnv = %q", got)
	}
	if !strings.Contains(out, "memory.local_dsn") || strings.Contains(out, "ecret") || strings.Contains(out, "w0rd") {
		t.Errorf("warning should name the field and nothing of the value: %q", out)
	}
	if out := captureLog(func() { expandEnv("f", "plain") }); out != "" {
		t.Errorf("a value without $ warned: %q", out)
	}
}

// The threshold ordering is checked on the values that run: an unset
// supersede_threshold runs at its default and went unchecked.
func TestValidate_ThresholdsUseDefaults(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig()
		c.Models = []ModelConfig{{ID: "m", Endpoint: "http://m", Provider: "llama-server", Chat: true}}
		c.normalizeRoles()
		return c
	}
	c := base()
	c.Memory.RelevanceThreshold = 0.8 // above the 0.75 supersede default
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "supersede_threshold") {
		t.Errorf("relevance above the default supersede threshold: err = %v", err)
	}
	c = base()
	c.Memory.RelevanceThreshold = 0.6
	c.Memory.DedupThreshold = 0.7 // below the 0.75 supersede default
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "supersede_threshold") {
		t.Errorf("dedup below the default supersede threshold: err = %v", err)
	}
	if err := base().Validate(); err != nil {
		t.Errorf("defaults don't validate: %v", err)
	}
}

// A provider or formatter the router can't build fails at boot, not on
// every call (the removed "anthropic" provider booted fine).
func TestValidate_UnknownProviderAndFormatter(t *testing.T) {
	for _, m := range []ModelConfig{
		{ID: "a", Endpoint: "e", Provider: "anthropic", Chat: true},
		{ID: "b", Endpoint: "e", Provider: "llama_server", Chat: true},
		{ID: "c", Endpoint: "e", Provider: "", Chat: true},
		{ID: "d", Endpoint: "e", Provider: "llama-completion", Formatter: "gemma", Chat: true},
	} {
		c := DefaultConfig()
		c.Models = []ModelConfig{m}
		c.normalizeRoles()
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("model %+v: err = %v, want unknown provider/formatter", m, err)
		}
	}
	c := DefaultConfig()
	c.Models = []ModelConfig{{ID: "ok", Endpoint: "e", Provider: "llama-completion", Formatter: "cohere2", Chat: true}}
	c.normalizeRoles()
	if err := c.Validate(); err != nil {
		t.Errorf("a known formatter failed: %v", err)
	}
}

// role = "embedder" was accepted and documented but read by nothing.
func TestNormalizeRoles_EmbedderRoleTag(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{}
	c.Models = []ModelConfig{
		{ID: "chat", Endpoint: "e", Provider: "llama-server", Chat: true},
		{ID: "embed/x", Endpoint: "e", Provider: "embeddings", Role: ModelRoleEmbedder, Dimension: 768},
	}
	c.normalizeRoles()
	if c.Roles.Embedder.Primary != "embed/x" {
		t.Errorf("embedder primary = %q, want the model tagged role=embedder", c.Roles.Embedder.Primary)
	}
}

// Unknown keys are reported (a README told operators to set
// [system_prompt].dir, which nothing reads), and so is a config file
// others can read.
func TestLoad_WarnsOnUnknownKeysAndLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.toml")
	body := "[system_prompt]\ndir = \"/x\"\n\n[[models]]\nid = \"m\"\nprovider = \"llama-server\"\nendpoint = \"http://m\"\nchat = true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var cfg *Config
	out := captureLog(func() {
		var err error
		if cfg, err = Load(path); err != nil {
			t.Fatal(err)
		}
		_ = cfg.Validate()
	})
	if !strings.Contains(out, "system_prompt.dir isn't a config key") {
		t.Errorf("unknown key not reported: %q", out)
	}
	if !strings.Contains(out, "readable by other users") {
		t.Errorf("0644 config not reported: %q", out)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := captureLog(func() { _, _ = Load(path) }); strings.Contains(out, "readable by other users") {
		t.Errorf("0600 config reported: %q", out)
	}
}

// FAMILIAR_HOME moves the default paths (the units and scripts set it,
// and nothing read it).
func TestFamiliarHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAMILIAR_HOME", dir)
	c := DefaultConfig()
	expandConfig(c)
	for name, got := range map[string]string{
		"prompt file": c.SystemPrompt.File, "prompt dir": c.SystemPrompt.Dir,
		"skills": c.Skills.Dir, "media": c.Media.Dir, "history": c.Adapter.CLI.HistoryFile,
	} {
		if !strings.HasPrefix(got, dir) {
			t.Errorf("%s = %q, want under FAMILIAR_HOME", name, got)
		}
	}
}
