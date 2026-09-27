package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped config.example.toml is what operators copy from, so it
// must parse, normalize, and validate. Catches a stale example after a
// schema change (a deprecated key removed, a required block renamed).
func TestConfigExampleParsesAndValidates(t *testing.T) {
	cfg, err := Load("../../../config.example.toml")
	if err != nil {
		t.Fatalf("config.example.toml does not load: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Fatal("expected the example to declare at least one model")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.example.toml does not validate: %v", err)
	}
	// Every key it shows is one the gateway reads: an unknown key is
	// ignored at boot, and operators copy them (working_context_ratio
	// sat here long after nothing read it).
	if len(cfg.unknown) > 0 {
		t.Errorf("config.example.toml sets keys the gateway doesn't know: %v", cfg.unknown)
	}
	// The example's chat role should resolve to the heavy backend it
	// documents, via the chat=true/lex-order derivation.
	if cfg.Roles.Chat.Primary == "" {
		t.Error("expected the example to yield a chat role primary")
	}
	t.Logf("chat=%q classify=%q embedder=%q",
		cfg.Roles.Chat.Primary, cfg.Roles.Classify.Primary, cfg.Roles.Embedder.Primary)
}

// The example tells operators to add a backup embedder. Doing exactly
// that (uncommenting its two embeddings models and [roles.embedder],
// with no [roles.chat]) used to route chat to "embed/nomic-a": the chat
// role was derived from the first role-less model in lex order, which
// no longer skipped embeddings models, and Validate passed.
func TestConfigExample_BackupEmbedderKeepsChat(t *testing.T) {
	raw, err := os.ReadFile("../../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	// Uncomment exactly the block from the "# [[models]]" line naming
	// embed/nomic-a through the [roles.embedder] backup line.
	lines := strings.Split(string(raw), "\n")
	var out []string
	inBlock := false
	for i, line := range lines {
		if line == "# [[models]]" && i+1 < len(lines) && strings.Contains(lines[i+1], `"embed/nomic-a"`) {
			inBlock = true
		}
		if inBlock {
			stripped := strings.TrimPrefix(strings.TrimPrefix(line, "#"), " ")
			if strings.HasPrefix(stripped, `backup  = "embed/nomic-b"`) {
				inBlock = false
			}
			line = stripped
		}
		out = append(out, line)
	}
	text := strings.Join(out, "\n")
	if !strings.Contains(text, "\n[roles.embedder]\n") {
		t.Fatal("test setup: couldn't uncomment the example's backup-embedder block")
	}
	// Also drop the heavy model's chat = true, the plain case the finding
	// reproduced: nothing marks the chat model, so it's derived.
	text = strings.Replace(text, "chat            = true", "", 1)
	path := filepath.Join(t.TempDir(), "gateway.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Roles.Embedder.Primary != "embed/nomic-a" {
		t.Fatalf("test setup: embedder = %q, the block wasn't live", cfg.Roles.Embedder.Primary)
	}
	if got := cfg.Roles.Chat.Primary; got != "gpu-host/qwen3.5-122b" {
		t.Errorf("chat primary = %q, want the generation model", got)
	}
}
