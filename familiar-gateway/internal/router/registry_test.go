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

// A single failed probe must NOT demote an online model when the fail
// threshold is 2 — that's the anti-flap guarantee failover relies on.
func TestRecordProbeDebouncesFailover(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{{ID: "m1", Provider: "openai"}})
	r.SetHealthParams(0, 0, 2, 2) // failN=2, recoverN=2
	r.recordProbe("m1", true)     // unknown → online immediately
	if r.StatusOf("m1") != "online" {
		t.Fatalf("first ok should bring unknown → online, got %q", r.StatusOf("m1"))
	}
	r.recordProbe("m1", false) // one blip — must stay online
	if r.StatusOf("m1") != "online" {
		t.Fatalf("one fail under threshold 2 must not demote, got %q", r.StatusOf("m1"))
	}
	r.recordProbe("m1", false) // second consecutive fail — now offline
	if r.StatusOf("m1") != "offline" {
		t.Fatalf("two consecutive fails should demote to offline, got %q", r.StatusOf("m1"))
	}
}

// A recovering fail streak resets on any ok, so an alternating
// up/down/up model never crosses the fail threshold.
func TestRecordProbeStreakResets(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{{ID: "m1", Provider: "openai"}})
	r.SetHealthParams(0, 0, 2, 2)
	r.recordProbe("m1", true)
	r.recordProbe("m1", false) // fail streak 1
	r.recordProbe("m1", true)  // resets fail streak
	r.recordProbe("m1", false) // fail streak 1 again
	if r.StatusOf("m1") != "online" {
		t.Fatalf("alternating probes must not demote, got %q", r.StatusOf("m1"))
	}
}

// offline → online needs recoverThreshold consecutive oks (failback
// debounce), but unknown → online is immediate (cold start).
func TestRecordProbeDebouncesFailback(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{{ID: "m1", Provider: "openai"}})
	r.SetHealthParams(0, 0, 1, 3) // failN=1, recoverN=3
	r.recordProbe("m1", false)    // unknown → offline (failN=1)
	if r.StatusOf("m1") != "offline" {
		t.Fatalf("failN=1 should demote immediately, got %q", r.StatusOf("m1"))
	}
	r.recordProbe("m1", true) // 1 ok
	r.recordProbe("m1", true) // 2 oks — still under recoverN=3
	if r.StatusOf("m1") != "offline" {
		t.Fatalf("2 oks under recover threshold 3 must stay offline, got %q", r.StatusOf("m1"))
	}
	r.recordProbe("m1", true) // 3 oks — recover
	if r.StatusOf("m1") != "online" {
		t.Fatalf("3 consecutive oks should recover to online, got %q", r.StatusOf("m1"))
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

// provider="embeddings" builds an embeddings provider (so the embedder
// gets a heartbeat like every other model) and is reachable through the
// typed accessor the embed path uses.
func TestGetEmbeddingsProvider(t *testing.T) {
	r := NewRegistry([]config.ModelConfig{
		{ID: "embed/a", Provider: "embeddings", Endpoint: "http://127.0.0.1:8100",
			Model: "nomic-embed-text", Dimension: 768},
		{ID: "chat/a", Provider: "llama-server", Endpoint: "http://127.0.0.1:8080"},
	})

	p, err := r.GetEmbeddingsProvider("embed/a", nil)
	if err != nil {
		t.Fatalf("GetEmbeddingsProvider: %v", err)
	}
	if p.Dimension() != 768 || p.Model() != "nomic-embed-text" {
		t.Errorf("provider fields wrong: dim=%d model=%q", p.Dimension(), p.Model())
	}
	// Cached: the embed hot path shouldn't rebuild per call.
	p2, _ := r.GetEmbeddingsProvider("embed/a", nil)
	if p != p2 {
		t.Error("expected the embeddings provider to be cached by model ID")
	}
	// A chat model is not an embedder.
	if _, err := r.GetEmbeddingsProvider("chat/a", nil); err == nil {
		t.Error("expected an error for a non-embeddings provider")
	}
	if _, err := r.GetEmbeddingsProvider("nope", nil); err == nil {
		t.Error("expected an error for an unknown model")
	}
	// It also builds through the generic path, so health checks work.
	if _, err := r.GetProvider("embed/a", nil); err != nil {
		t.Errorf("embeddings model should build as a Provider for health checks: %v", err)
	}
}
