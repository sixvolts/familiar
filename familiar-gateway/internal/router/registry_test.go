package router

import (
	"testing"

	"github.com/familiar/gateway/internal/config"
)

func TestRegistryOnlineEmpty(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{
		{ID: "m1", Provider: "openai"},
	})
	// All start as "unknown", so Online() should be empty.
	if got := r.Online(); len(got) != 0 {
		t.Fatalf("expected 0 online, got %d", len(got))
	}
}

func TestRegistrySetStatusOnline(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{
		{ID: "m1", Provider: "openai", Endpoint: "https://example.com"},
	})
	r.setStatus("m1", "online")

	online := r.Online()
	if len(online) != 1 || online[0] != "m1" {
		t.Fatalf("expected [m1] online, got %v", online)
	}
}

func TestRegistrySetStatusOffline(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{
		{ID: "m1", Provider: "openai"},
	})
	r.setStatus("m1", "online")
	r.setStatus("m1", "offline")

	if got := r.Online(); len(got) != 0 {
		t.Fatalf("expected 0 online after setting offline, got %d", len(got))
	}
}

func TestRegistryGetProviderUnknownModel(t *testing.T) {
	r := NewRegistry(nil)
	_, err := r.GetProvider("nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

// The anthropic backend was removed (local-first). A config that still
// names it must fail to build a provider rather than silently doing
// something — buildProvider's default case rejects it.
func TestRegistryGetProviderAnthropicRemoved(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{
		{ID: "claude", Provider: "anthropic", Endpoint: "https://api.anthropic.com"},
	})
	if _, err := r.GetProvider("claude", func(s string) string { return "test-key" }); err == nil {
		t.Fatal("expected error for removed anthropic provider, got nil")
	}
}

func TestRegistryGetProviderOpenAI(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{
		{ID: "local-llama", Provider: "llama-server", Endpoint: "http://localhost:8080"},
	})
	p, err := r.GetProvider("local-llama", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestResolveAPIKeyVaultPriority(t *testing.T) {
	cfg := config.ModelConfig{
		APIKey:   "config-key",
		VaultKey: "vault-secret",
	}
	got := resolveAPIKey(cfg, func(k string) string {
		if k == "vault-secret" {
			return "from-vault"
		}
		return ""
	})
	if got != "from-vault" {
		t.Fatalf("expected from-vault, got %q", got)
	}
}

func TestResolveAPIKeyFallbackToConfig(t *testing.T) {
	cfg := config.ModelConfig{
		APIKey:   "config-key",
		VaultKey: "vault-secret",
	}
	// apiKeyFn returns empty — should fall back to APIKey
	got := resolveAPIKey(cfg, func(k string) string { return "" })
	if got != "config-key" {
		t.Fatalf("expected config-key, got %q", got)
	}
}

func TestResolveAPIKeyNoVaultKey(t *testing.T) {
	cfg := config.ModelConfig{
		APIKey: "config-key",
	}
	got := resolveAPIKey(cfg, nil)
	if got != "config-key" {
		t.Fatalf("expected config-key, got %q", got)
	}
}
