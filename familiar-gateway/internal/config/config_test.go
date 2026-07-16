package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg == nil {
		t.Fatal("DefaultConfig returned nil")
	}
	if !cfg.Router.Enabled {
		t.Fatal("expected router to be enabled by default")
	}
	if cfg.Embedder.Dimension != 768 {
		t.Fatalf("expected dimension 768, got %d", cfg.Embedder.Dimension)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	got := expandPath("~/some/path")
	expected := filepath.Join(home, "some/path")
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestExpandPathNoTilde(t *testing.T) {
	got := expandPath("/absolute/path")
	if got != "/absolute/path" {
		t.Fatalf("expected /absolute/path, got %q", got)
	}
}

func TestLoadFromFile(t *testing.T) {
	tomlContent := `
[[models]]
id = "test-model"
provider = "openai"
endpoint = "https://test.example.com"

[router]
enabled = false
fallback_model = "test-model"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(tomlContent), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if len(cfg.Models) != 1 || cfg.Models[0].ID != "test-model" {
		t.Fatalf("unexpected models: %+v", cfg.Models)
	}
	if cfg.Router.Enabled {
		t.Fatal("expected router disabled")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("FAMILIAR_TEST_VAR", "hello")
	got := expandEnv("prefix-$FAMILIAR_TEST_VAR-suffix")
	if !strings.Contains(got, "hello") {
		t.Fatalf("expected env expansion, got %q", got)
	}
}
