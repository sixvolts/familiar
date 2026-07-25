package router

import (
	"context"
	"testing"

	"github.com/familiar/gateway/internal/config"
)

func noKey(string) string { return "" }

func makeRegistryWithModels(models ...config.ModelConfig) *Registry {
	r := NewRegistry(models)
	for _, m := range models {
		r.setStatus(m.ID, "online")
	}
	return r
}

func TestRouterSelectDisabled(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "fallback-model", Provider: "openai", Endpoint: "https://example.test"},
	)
	router := NewRouter(config.RouterConfig{
		Enabled: false,
	}, reg)

	modelID, p, err := router.Select(context.Background(), "hello", "cli", noKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if modelID != "fallback-model" {
		t.Fatalf("expected fallback-model, got %q", modelID)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestRouterSelectForceRule(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "big-model", Provider: "openai", Endpoint: "https://example.test"},
		config.ModelConfig{ID: "small-model", Provider: "openai", Endpoint: "https://example.test"},
	)
	router := NewRouter(config.RouterConfig{
		Enabled: true,
		Rules: config.RouterRules{
			Force: []config.ForceRule{
				{Pattern: "(?i)analyze", Model: "big-model"},
			},
		},
	}, reg)

	modelID, _, err := router.Select(context.Background(), "please Analyze this", "cli", noKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if modelID != "big-model" {
		t.Fatalf("expected big-model, got %q", modelID)
	}
}

func TestRouterSelectForceRuleChannelMismatch(t *testing.T) {
	// big-model is NOT online — only default-model is.
	// If the force rule incorrectly fires, it'll try big-model and fail.
	reg := NewRegistry([]config.ModelConfig{
		{ID: "big-model", Provider: "openai", Endpoint: "https://example.test"},
		{ID: "default-model", Provider: "openai", Endpoint: "https://example.test"},
	})
	reg.setStatus("default-model", "online")
	// big-model stays "unknown" (offline)

	router := NewRouter(config.RouterConfig{
		Enabled: true,
		Rules: config.RouterRules{
			Force: []config.ForceRule{
				{Pattern: "analyze", Channel: "slack", Model: "big-model"},
			},
		},
	}, reg)

	// Channel is "cli", not "slack" — rule should not match, falls through to default-model
	modelID, _, err := router.Select(context.Background(), "analyze this", "cli", noKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if modelID != "default-model" {
		t.Fatalf("expected default-model, got %q", modelID)
	}
}

func TestRouterSelectPreferLocal(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "remote-model", Provider: "openai", Endpoint: "https://example.test", LatencyProfile: "remote"},
		config.ModelConfig{ID: "local-model", Provider: "llama-server", Endpoint: "http://localhost:8080", LatencyProfile: "local"},
	)
	router := NewRouter(config.RouterConfig{
		Enabled:     true,
		PreferLocal: true,
	}, reg)

	modelID, _, err := router.Select(context.Background(), "hello", "cli", noKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if modelID != "local-model" {
		t.Fatalf("expected local-model, got %q", modelID)
	}
}

func TestRouterSelectFallbackNoModels(t *testing.T) {
	// Empty registry, no fallback configured
	reg := NewRegistry(nil)
	router := NewRouter(config.RouterConfig{
		Enabled: true,
	}, reg)

	_, _, err := router.Select(context.Background(), "hello", "cli", noKey)
	if err == nil {
		t.Fatal("expected error when no models and no fallback")
	}
}

func TestRouterSelectFirstOnline(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "model-a", Provider: "openai", Endpoint: "https://example.test"},
		config.ModelConfig{ID: "model-b", Provider: "openai", Endpoint: "https://example.test"},
	)
	router := NewRouter(config.RouterConfig{Enabled: true}, reg)

	modelID, _, err := router.Select(context.Background(), "hello", "cli", noKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should get one of the online models
	if modelID != "model-a" && modelID != "model-b" {
		t.Fatalf("expected model-a or model-b, got %q", modelID)
	}
}

// stubChatRole is a minimal ChatRoleResolver for the chat-failover tests.
type stubChatRole struct {
	chain  []string
	health map[string]string
}

func (s stubChatRole) Resolve(role string) (string, int, bool) {
	if role != config.RoleChat || len(s.chain) == 0 {
		return "", 0, false
	}
	for i, id := range s.chain {
		if s.health[id] != "offline" {
			return id, i, true
		}
	}
	return s.chain[0], 0, true
}

// With a chat-role resolver attached, GetChatModelID follows the chain
// instead of the chat=true / lex-order config selection.
func TestGetChatModelIDUsesRoleChain(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "primary", Provider: "openai", Endpoint: "https://e", Chat: true},
		config.ModelConfig{ID: "backup", Provider: "openai", Endpoint: "https://e"},
	)
	rtr := NewRouter(config.RouterConfig{}, reg)
	health := map[string]string{"primary": "online", "backup": "online"}
	rtr.SetChatRole(stubChatRole{chain: []string{"primary", "backup"}, health: health})
	rtr.SetChatPrimary("primary")

	if got := rtr.GetChatModelID(); got != "primary" {
		t.Fatalf("healthy primary should serve, got %q", got)
	}
	if tier, ok := rtr.ChatModelTier(); !ok || tier != 0 {
		t.Fatalf("want tier 0, got %d (ok=%v)", tier, ok)
	}

	// Primary demoted → chat follows the chain to the backup, but
	// ChatPrimaryID still names the configured primary.
	health["primary"] = "offline"
	if got := rtr.GetChatModelID(); got != "backup" {
		t.Fatalf("offline primary should fail over to backup, got %q", got)
	}
	if tier, _ := rtr.ChatModelTier(); tier != 1 {
		t.Fatalf("want tier 1 while on backup, got %d", tier)
	}
	if got := rtr.ChatPrimaryID(); got != "primary" {
		t.Fatalf("ChatPrimaryID must keep naming the configured primary, got %q", got)
	}

	// Primary recovers → auto-failback.
	health["primary"] = "online"
	if got := rtr.GetChatModelID(); got != "primary" {
		t.Fatalf("recovered primary should take traffic back, got %q", got)
	}
}

// No resolver wired → the historical selection still applies, so a
// pre-roles deployment behaves exactly as before.
func TestGetChatModelIDFallsBackToConfigSelection(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "zeta", Provider: "openai", Endpoint: "https://e"},
		config.ModelConfig{ID: "alpha", Provider: "openai", Endpoint: "https://e", Chat: true},
	)
	rtr := NewRouter(config.RouterConfig{}, reg)
	if got := rtr.GetChatModelID(); got != "alpha" {
		t.Fatalf("chat=true model should win with no resolver, got %q", got)
	}
	if _, ok := rtr.ChatModelTier(); ok {
		t.Fatal("ChatModelTier should report ok=false with no resolver")
	}
}

// An unconfigured chat role falls through to the config selection
// rather than returning empty.
func TestGetChatModelIDUnconfiguredRoleFallsThrough(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "only", Provider: "openai", Endpoint: "https://e"},
	)
	rtr := NewRouter(config.RouterConfig{}, reg)
	rtr.SetChatRole(stubChatRole{}) // resolves nothing
	if got := rtr.GetChatModelID(); got != "only" {
		t.Fatalf("empty chat chain should fall through to config selection, got %q", got)
	}
}
