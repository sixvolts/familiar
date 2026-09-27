package router

import (
	"context"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/sidecar"
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

// [[router.rules.force]] is no longer read: Select runs only when no chat
// model resolves, so the rules never applied in a normal config (Validate
// warns when they're set). Select picks in id order, not map order.
func TestRouterSelectIgnoresForceRules(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "a-model", Provider: "openai", Endpoint: "https://example.test"},
		config.ModelConfig{ID: "z-model", Provider: "openai", Endpoint: "https://example.test"},
	)
	router := NewRouter(config.RouterConfig{
		Enabled: true,
		Rules:   config.RouterRules{Force: []config.ForceRule{{Pattern: "(?i)analyze", Model: "z-model"}}},
	}, reg)
	modelID, _, err := router.Select(context.Background(), "please Analyze this", "cli", noKey)
	if err != nil || modelID != "a-model" {
		t.Fatalf("Select = %q, %v; want a-model (id order, force rule ignored)", modelID, err)
	}
}

// Select never picks an embeddings model: it can't answer chat.
func TestRouterSelectSkipsEmbeddings(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "a-embed", Provider: "embeddings", Endpoint: "http://e"},
		config.ModelConfig{ID: "b-chat", Provider: "openai", Endpoint: "https://example.test"},
	)
	for i := 0; i < 20; i++ {
		if id, _, err := NewRouter(config.RouterConfig{Enabled: true}, reg).Select(context.Background(), "hi", "cli", noKey); err != nil || id != "b-chat" {
			t.Fatalf("Select = %q, %v; want b-chat", id, err)
		}
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

	for i := 0; i < 20; i++ { // map order would vary between runs
		modelID, _, err := router.Select(context.Background(), "hello", "cli", noKey)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if modelID != "model-a" {
			t.Fatalf("expected model-a (id order), got %q", modelID)
		}
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

func (s stubChatRole) Chain(role string) []string {
	if role != config.RoleChat {
		return nil
	}
	return s.chain
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

// Without a chat role, chat falls back to the first role-less model;
// an embeddings model is never it.
func TestChatModelIDFromConfigSkipsEmbeddings(t *testing.T) {
	r := NewRouter(config.RouterConfig{Enabled: true}, NewRegistry([]config.ModelConfig{
		{ID: "embed/a", Provider: "embeddings", Endpoint: "e"},
		{ID: "gpu-host/big", Provider: "llama-server", Endpoint: "e"},
	}))
	if got := r.GetChatModelID(); got != "gpu-host/big" {
		t.Errorf("chat model = %q, want the generation model", got)
	}
}

// The sidecar learns a model's configured request name through this
// interface; if the registry stopped satisfying it, sidecar requests
// would silently fall back to the id without its namespace.
var _ sidecar.RequestModelNamer = (*Registry)(nil)

// noClassifyRole resolves nothing.
type noClassifyRole struct{}

func (noClassifyRole) Resolve(string) (string, int, bool) { return "", 0, false }
func (noClassifyRole) Chain(string) []string              { return nil }

// classifyRole resolves the classify role to one model.
type classifyRole struct{ id string }

func (c classifyRole) Resolve(role string) (string, int, bool) {
	if role == config.RoleClassify {
		return c.id, 0, true
	}
	return "", 0, false
}
func (classifyRole) Chain(string) []string { return nil }

// Shard tier1/tier2 run on what the classify role resolves to. It was the
// first "sidecar/"-prefixed id in map order: "mac/gemma" broke every tier1
// shard, and two sidecar models were a coin toss.
func TestGetSidecarModelIDFollowsTheClassifyRole(t *testing.T) {
	reg := makeRegistryWithModels(
		config.ModelConfig{ID: "mac/gemma", Provider: "llama-server", Endpoint: "http://m"},
		config.ModelConfig{ID: "sidecar/other", Provider: "llama-server", Endpoint: "http://s"},
	)
	r := NewRouter(config.RouterConfig{Enabled: true}, reg)
	r.SetChatRole(classifyRole{id: "mac/gemma"})
	if got := r.GetSidecarModelID(); got != "mac/gemma" {
		t.Errorf("GetSidecarModelID = %q, want the classify role's mac/gemma", got)
	}
	// No classify model: the chat model rather than nothing.
	r.SetChatRole(noClassifyRole{})
	if got := r.GetSidecarModelID(); got != "mac/gemma" {
		t.Errorf("no classify role: GetSidecarModelID = %q, want the chat model (mac/gemma, first in id order)", got)
	}
	// No resolver: the prefix, in id order.
	reg2 := makeRegistryWithModels(
		config.ModelConfig{ID: "sidecar/zeta", Provider: "llama-server", Endpoint: "http://z"},
		config.ModelConfig{ID: "sidecar/alpha", Provider: "llama-server", Endpoint: "http://a"},
	)
	for i := 0; i < 20; i++ {
		if got := NewRouter(config.RouterConfig{}, reg2).GetSidecarModelID(); got != "sidecar/alpha" {
			t.Fatalf("GetSidecarModelID = %q, want sidecar/alpha", got)
		}
	}
}
