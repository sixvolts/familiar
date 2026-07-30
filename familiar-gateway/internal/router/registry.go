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
	"github.com/familiar/gateway/internal/safego"
)

// ModelEntry holds runtime state for a model.
type ModelEntry struct {
	Config      config.ModelConfig
	Status      string // "online", "offline", "unknown"
	LastHealthy time.Time

	// Consecutive-probe streaks backing the debounce in recordProbe.
	// A single blip no longer flips Status; the transition only fires
	// after failThreshold consecutive fails (→ offline) or
	// recoverThreshold consecutive oks (offline → online). This is the
	// whole anti-flap mechanism, and it serves failover and failback
	// symmetrically.
	failStreak int
	okStreak   int
}

// Registry tracks model availability and constructs providers on demand.
type Registry struct {
	entries map[string]*ModelEntry
	mu      sync.RWMutex

	// embedders caches embeddings providers by model ID — the embed path
	// runs per memory write and per retrieval query, so it shouldn't
	// rebuild a provider each call.
	embedders map[string]*llm.EmbeddingsProvider

	// Heartbeat tuning. Zero values fall back to the historical
	// hardcoded constants / single-probe behavior via the *OrDefault
	// accessors, so a registry built without SetHealthParams behaves
	// exactly as before.
	interval         time.Duration
	timeout          time.Duration
	failThreshold    int
	recoverThreshold int
}

// NewRegistry initialises a registry from a slice of model configs.
func NewRegistry(models []config.ModelConfig) *Registry {
	r := &Registry{
		entries:   make(map[string]*ModelEntry, len(models)),
		embedders: make(map[string]*llm.EmbeddingsProvider),
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

// SetHealthParams configures the heartbeat cadence and debounce
// thresholds (typically from [roles]). Call before StartHealthChecks.
func (r *Registry) SetHealthParams(interval, timeout time.Duration, failThreshold, recoverThreshold int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interval = interval
	r.timeout = timeout
	r.failThreshold = failThreshold
	r.recoverThreshold = recoverThreshold
}

func (r *Registry) intervalOrDefault() time.Duration {
	if r.interval > 0 {
		return r.interval
	}
	return 30 * time.Second
}

func (r *Registry) timeoutOrDefault() time.Duration {
	if r.timeout > 0 {
		return r.timeout
	}
	return 15 * time.Second
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

	interval := r.intervalOrDefault()
	for _, id := range ids {
		id := id
		go func() {
			// Per-probe recovery, NOT a recover at the goroutine top. This
			// loop is what drives role failover: if it returns, the model is
			// never probed again, its status freezes, and failover quietly
			// stops working for the rest of the process lifetime. One bad
			// probe should be skipped, not fatal to the heartbeat.
			label := "health probe " + id
			safego.Do(label, func() { r.checkOne(ctx, id, apiKeyFn) })

			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					safego.Do(label, func() { r.checkOne(ctx, id, apiKeyFn) })
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
		r.recordProbe(modelID, false)
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, r.timeoutOrDefault())
	defer cancel()

	if err := provider.HealthCheck(checkCtx); err != nil {
		r.recordProbe(modelID, false)
		return
	}

	r.recordProbe(modelID, true)
}

// SetStatusForTest is an exported wrapper around setStatus for use in
// tests. It forces the status directly and resets the debounce streaks
// so a subsequent recordProbe starts from a clean slate.
func (r *Registry) SetStatusForTest(modelID, status string) {
	r.setStatus(modelID, status)
}

func (r *Registry) setStatus(modelID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[modelID]; ok {
		e.Status = status
		e.failStreak = 0
		e.okStreak = 0
		if status == "online" {
			e.LastHealthy = time.Now()
		}
	}
}

// recordProbe folds one health-probe result into a model's status
// through the debounce thresholds. The risky transitions are debounced;
// the safe ones are immediate:
//   - unknown → online: immediate (fast, honest cold-start UX).
//   - online/unknown → offline: only after failThreshold consecutive
//     fails (a single blip won't demote a primary and trigger failover).
//   - offline → online: only after recoverThreshold consecutive oks
//     (a marginal model won't flap back and forth).
//
// LastHealthy tracks the most recent ok regardless of the status flip.
func (r *Registry) recordProbe(modelID string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, exists := r.entries[modelID]
	if !exists {
		return
	}
	failN := r.failThreshold
	if failN < 1 {
		failN = 1
	}
	recoverN := r.recoverThreshold
	if recoverN < 1 {
		recoverN = 1
	}

	if ok {
		e.failStreak = 0
		e.okStreak++
		e.LastHealthy = time.Now()
		switch e.Status {
		case "online":
			// already up
		case "offline":
			if e.okStreak >= recoverN {
				r.logTransition(modelID, e.Status, "online")
				e.Status = "online"
			}
		default: // unknown
			r.logTransition(modelID, e.Status, "online")
			e.Status = "online"
		}
		return
	}

	e.okStreak = 0
	e.failStreak++
	if e.Status != "offline" && e.failStreak >= failN {
		r.logTransition(modelID, e.Status, "offline")
		e.Status = "offline"
	}
}

// logTransition prints a status change once (called under the write
// lock). Kept terse — the operator wants to see failover happen.
func (r *Registry) logTransition(modelID, from, to string) {
	log.Printf("[router] model %s health %s → %s", modelID, from, to)
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
	case "embeddings":
		// Text → vector only. Registered as a normal model so it gets
		// the shared heartbeat and can sit in a [roles.embedder] chain;
		// its Complete/CompleteStream return an error, so a config that
		// aims chat at it fails loudly.
		return llm.NewEmbeddingsProvider(cfg.Provider+"/"+cfg.ID, cfg.Endpoint, apiKey,
			cfg.ServedName(), cfg.Dimension, 0), nil
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

// GetEmbeddingsProvider returns a live embeddings provider for a model
// ID, or an error when the ID is unknown or the model isn't an
// embeddings backend. Providers are cheap value types (an endpoint plus
// an http.Client), but they're cached per model ID so the embed hot path
// doesn't rebuild one per call.
func (r *Registry) GetEmbeddingsProvider(modelID string, apiKeyFn func(string) string) (*llm.EmbeddingsProvider, error) {
	r.mu.RLock()
	entry, ok := r.entries[modelID]
	cached := r.embedders[modelID]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown model %q", modelID)
	}
	if cached != nil {
		return cached, nil
	}
	if entry.Config.Provider != "embeddings" {
		return nil, fmt.Errorf("model %q has provider %q, want \"embeddings\"", modelID, entry.Config.Provider)
	}

	apiKey := resolveAPIKey(entry.Config, apiKeyFn)
	p := llm.NewEmbeddingsProvider(entry.Config.Provider+"/"+entry.Config.ID,
		entry.Config.Endpoint, apiKey, entry.Config.ServedName(), entry.Config.Dimension, 0)

	r.mu.Lock()
	if existing := r.embedders[modelID]; existing != nil {
		p = existing // another goroutine won the race; reuse theirs
	} else {
		r.embedders[modelID] = p
	}
	r.mu.Unlock()
	return p, nil
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
