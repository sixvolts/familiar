package config

import "testing"

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
	// The example's chat role should resolve to the heavy backend it
	// documents, via the chat=true/lex-order derivation.
	if cfg.Roles.Chat.Primary == "" {
		t.Error("expected the example to yield a chat role primary")
	}
	t.Logf("chat=%q classify=%q embedder=%q",
		cfg.Roles.Chat.Primary, cfg.Roles.Classify.Primary, cfg.Roles.Embedder.Primary)
}
