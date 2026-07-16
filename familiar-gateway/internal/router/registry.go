package router

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/llm"
)

// ModelEntry holds runtime state for a model.
type ModelEntry struct {
	Config      config.ModelConfig
	Status      string // "online", "offline", "unknown"
	LastHealthy time.Time
}

// Registry tracks model availability and constructs providers on demand.
type Registry struct {
	entries map[string]*ModelEntry
	mu      sync.RWMutex
}

// NewRegistry initialises a registry from a slice of model configs.
func NewRegistry(models []config.ModelConfig) *Registry {
	r := &Registry{
		entries: make(map[string]*ModelEntry, len(models)),
	}
	for _, m := range models {
		m := m // copy
		r.entries[m.ID] = &ModelEntry{
			Config: m,
			Status: "unknown",
		}
	}
	return r
}

// GetProvider returns a live Provider for the given model ID.
// apiKeyFn is called with the model's VaultKey to retrieve a secret.
func (r *Registry) GetProvider(modelID string, apiKeyFn func(string) string) (llm.Provider, error) {
	r.mu.RLock()
	entry, ok := r.entries[modelID]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown model %q", modelID)
	}

	apiKey := resolveAPIKey(entry.Config, apiKeyFn)
	return buildProvider(entry.Config, apiKey)
}

// ByRole returns the ID of the model carrying the given role
// (classifier / embedder / summarizer), or "" if none. Validation
// has already enforced at-most-one model per role at config load
// time, so this is an O(n) scan looking for first hit.
func (r *Registry) ByRole(role string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, e := range r.entries {
		if e.Config.Role == role {
			return id
		}
	}
	return ""
}

// EndpointForRole returns the Endpoint of the model carrying the
// given role, or "" if none. Used by the sidecar Client to derive
// its slot endpoints from registry entries instead of the literal
// URLs in [sidecar]. Satisfies sidecar.EndpointResolver.
//
// "small" matches the legacy "classifier" role too — pre-CHAT-REARCH
// gateway.toml configs labeled the slot "classifier", and we accept
// either spelling so existing deployments don't have to migrate
// config to upgrade.
func (r *Registry) EndpointForRole(role string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.entries {
		if e.Config.Role == role {
			return e.Config.Endpoint
		}
		if role == config.ModelSlotSmall && e.Config.Role == config.ModelRoleClassifier {
			return e.Config.Endpoint
		}
	}
	return ""
}

// EndpointForModel returns the Endpoint of the model with the given
// ID, or "" if no such model is registered. Used by the sidecar
// Client to resolve the explicit task→model assignments from
// [sidecar] (SIDECAR-SLOT-FIXES). Satisfies sidecar.EndpointResolver.
func (r *Registry) EndpointForModel(modelID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.entries[modelID]; ok {
		return e.Config.Endpoint
	}
	return ""
}

// List returns every registered model's config + current status,
// in stable (sorted-by-ID) order. Used by the admin model-catalog
// endpoint that drives the chat UI's model menu.
func (r *Registry) List() []ModelEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Config.ID < out[j].Config.ID })
	return out
}

// Online returns the IDs of all currently online models.
func (r *Registry) Online() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var ids []string
	for id, e := range r.entries {
		if e.Status == "online" {
			ids = append(ids, id)
		}
	}
	return ids
}

// StartHealthChecks runs periodic health checks in the background.
func (r *Registry) StartHealthChecks(ctx context.Context, apiKeyFn func(string) string) {
	r.mu.RLock()
	ids := make([]string, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	r.mu.RUnlock()

	for _, id := range ids {
		id := id
		go func() {
			// Run initial check immediately.
			r.checkOne(ctx, id, apiKeyFn)

			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					r.checkOne(ctx, id, apiKeyFn)
				}
			}
		}()
	}
}

func (r *Registry) checkOne(ctx context.Context, modelID string, apiKeyFn func(string) string) {
	r.mu.RLock()
	entry, ok := r.entries[modelID]
	r.mu.RUnlock()
	if !ok {
		return
	}

	apiKey := resolveAPIKey(entry.Config, apiKeyFn)
	provider, err := buildProvider(entry.Config, apiKey)
	if err != nil {
		r.setStatus(modelID, "offline")
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := provider.HealthCheck(checkCtx); err != nil {
		log.Printf("[router] model %s health check failed: %v", modelID, err)
		r.setStatus(modelID, "offline")
		return
	}

	r.setStatus(modelID, "online")
}

// SetStatusForTest is an exported wrapper around setStatus for use in tests.
func (r *Registry) SetStatusForTest(modelID, status string) {
	r.setStatus(modelID, status)
}

func (r *Registry) setStatus(modelID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[modelID]; ok {
		e.Status = status
		if status == "online" {
			e.LastHealthy = time.Now()
		}
	}
}

// resolveAPIKey picks the best API key for a model config.
func resolveAPIKey(cfg config.ModelConfig, apiKeyFn func(string) string) string {
	if cfg.VaultKey != "" && apiKeyFn != nil {
		if key := apiKeyFn(cfg.VaultKey); key != "" {
			return key
		}
	}
	return cfg.APIKey
}

// buildProvider constructs the right Provider for a model config.
//
// "llama-completion" hits llama-server's raw /completion endpoint
// with prompts the gateway builds itself via a ModelFormatter.
// CHAT-REARCH familiar-raw-completion-design.md — the turbo fork's
// /v1/chat/completions reasoning parser broke under ROCm 7.2.3 but
// /completion still works. Operators flip a model to the new path
// by setting `provider = "llama-completion"` and `formatter =
// "qwen35"` in gateway.toml.
func buildProvider(cfg config.ModelConfig, apiKey string) (llm.Provider, error) {
	switch cfg.Provider {
	case "llama-server", "openai", "ollama", "vllm":
		return llm.NewOpenAIProvider(cfg.Provider+"/"+cfg.ID, cfg.Endpoint, apiKey), nil
	case "llama-completion":
		formatter, err := pickFormatter(cfg.Formatter)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", cfg.ID, err)
		}
		p := llm.NewLlamaCompletionProvider(cfg.Provider+"/"+cfg.ID, cfg.Endpoint, apiKey, formatter)
		if overrides := cfg.Sampling.AsMap(); len(overrides) > 0 {
			p.WithSampling(overrides)
		}
		return p, nil
	default:
		return nil, fmt.Errorf("unknown provider %q for model %q", cfg.Provider, cfg.ID)
	}
}

// pickFormatter returns the ModelFormatter named by the model's
// `formatter` config field. Defaults to qwen35 — the original
// raw-completion formatter. Add new model families here.
func pickFormatter(name string) (llm.ModelFormatter, error) {
	switch name {
	case "", "qwen35":
		return llm.NewQwen35Formatter(), nil
	case "cohere2":
		return llm.NewCohere2Formatter(), nil
	default:
		return nil, fmt.Errorf("unknown formatter %q", name)
	}
}

// GetModelConfig returns the config for a model ID, or nil if not found.
func (r *Registry) GetModelConfig(modelID string) *config.ModelConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.entries[modelID]; ok {
		cfg := e.Config
		return &cfg
	}
	return nil
}

// StatusOf returns the current health status for a model ID
// ("online", "offline", "unknown"), or the empty string if the model
// is not registered. Used by the admin dashboard to render a status
// column without exposing the entire ModelEntry.
func (r *Registry) StatusOf(modelID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.entries[modelID]; ok {
		return e.Status
	}
	return ""
}

// ModelIDs returns all registered model IDs regardless of health status.
func (r *Registry) ModelIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	return ids
}
