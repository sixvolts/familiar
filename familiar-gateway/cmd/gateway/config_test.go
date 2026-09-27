package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/familiar/gateway/internal/config"
)

// A model's key comes from its api_key or FAMILIAR_<ID>_KEY. The
// resolver asked an engine vault first, which is unsupported (a 3s
// timeout per resolution); it no longer touches the engine at all.
func TestAPIKeyFn_NoVault(t *testing.T) {
	t.Setenv("FAMILIAR_OPENAI_GPT_KEY", "from-env")
	fn := makeAPIKeyFn(nil, []config.ModelConfig{
		{ID: "openai/gpt", VaultKey: "gpt"},
		{ID: "inline", VaultKey: "inline", APIKey: "from-config"},
	})
	if got := fn("gpt"); got != "from-env" {
		t.Errorf("env key = %q", got)
	}
	if got := fn("inline"); got != "from-config" {
		t.Errorf("inline key = %q", got)
	}
}

// The config is looked for in $FAMILIAR_HOME (the units set it).
func TestLoadConfig_FamiliarHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAMILIAR_HOME", dir)
	body := "[[models]]\nid = \"from-home\"\nprovider = \"llama-server\"\nendpoint = \"http://m\"\nchat = true\n"
	if err := os.WriteFile(filepath.Join(dir, "gateway.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) == 0 || cfg.Models[0].ID != "from-home" {
		t.Errorf("loaded %+v, want the config in FAMILIAR_HOME", cfg.Models)
	}
}
