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
