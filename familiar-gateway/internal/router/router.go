package router

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/llm"
	"github.com/familiar/gateway/internal/sidecar"
)

// Router selects an LLM provider for each incoming message.
type Router struct {
	cfg      config.RouterConfig
	registry *Registry
	sidecar  *sidecar.Client
	compiled []*compiledRule
}

type compiledRule struct {
	ForceRule config.ForceRule
	re        *regexp.Regexp
}

// NewRouter constructs a Router from config and a model registry.
// sidecarClient may be nil if the sidecar is not configured.
func NewRouter(cfg config.RouterConfig, registry *Registry, sidecarClient ...*sidecar.Client) *Router {
	r := &Router{
		cfg:      cfg,
		registry: registry,
	}

	if len(sidecarClient) > 0 {
		r.sidecar = sidecarClient[0]
	}

	for _, rule := range cfg.Rules.Force {
		rule := rule
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			// Skip invalid patterns.
			continue
		}
		r.compiled = append(r.compiled, &compiledRule{ForceRule: rule, re: re})
	}

	return r
}

// SetSidecar attaches or replaces the sidecar client used for smart routing.
func (r *Router) SetSidecar(sc *sidecar.Client) {
	r.sidecar = sc
}

// Select picks a chat model for the given message + channel.
// Returns (modelID, provider, error).
//
// CHAT-REARCH: classification is no longer a router concern —
// the pipeline calls sidecar.Client.Classify directly. The router
// only picks WHICH model to dispatch to. Two layers:
//  1. force rules (regex match on the message)
//  2. rule-based fallback (first online model, prefer-local
//     honored when set)
func (r *Router) Select(ctx context.Context, msg string, channelID string, apiKeyFn func(string) string) (string, llm.Provider, error) {
	if !r.cfg.Enabled {
		return r.selectFallback(apiKeyFn)
	}

	// 1. Check force rules (highest priority — explicit overrides).
	for _, rule := range r.compiled {
		if rule.re.MatchString(msg) {
			if rule.ForceRule.Channel == "" || rule.ForceRule.Channel == channelID {
				p, err := r.registry.GetProvider(rule.ForceRule.Model, apiKeyFn)
				if err == nil {
					return rule.ForceRule.Model, p, nil
				}
			}
		}
	}

	// 2. Rule-based fallback: prefer local if configured.
	return r.selectRuleBased(apiKeyFn)
}

// selectRuleBased is the original rule-based routing logic.
func (r *Router) selectRuleBased(apiKeyFn func(string) string) (string, llm.Provider, error) {
	if r.cfg.PreferLocal {
		for _, id := range r.registry.Online() {
			r.registry.mu.RLock()
			entry := r.registry.entries[id]
			r.registry.mu.RUnlock()

			if entry != nil && entry.Config.LatencyProfile == "local" {
				p, err := r.registry.GetProvider(id, apiKeyFn)
				if err == nil {
					return id, p, nil
				}
			}
		}
	}

	for _, id := range r.registry.Online() {
		p, err := r.registry.GetProvider(id, apiKeyFn)
		if err == nil {
			return id, p, nil
		}
	}

	return "", nil, fmt.Errorf("no online models available")
}

// GetRegistry returns the underlying registry.
func (r *Router) GetRegistry() *Registry {
	return r.registry
}

// GetChatModelID returns the ID of a registered model that has no
// support role tag (i.e., the chat backend). Picks the first
// matching ID in stable lex order. Returns "" when every
// registered model carries a role.
//
// Replaces GetFallbackModelID under the single-chat-model
// architecture from CHAT-REARCH: there is no separate "fallback"
// model anymore; chat requests go to the single non-support
// model. Used by shard tier-hint mapping during the transition;
// will collapse with the rest of tier-based routing in a later
// step.
func (r *Router) GetChatModelID() string {
	r.registry.mu.RLock()
	defer r.registry.mu.RUnlock()
	var ids []string
	for id, e := range r.registry.entries {
		if e.Config.Role == "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	return ids[0]
}

// GetSidecarModelID returns the canonical model ID for the sidecar from
// the registry, suitable for GetProvider/GetModelConfig lookups.
func (r *Router) GetSidecarModelID() string {
	r.registry.mu.RLock()
	defer r.registry.mu.RUnlock()
	for id := range r.registry.entries {
		if strings.HasPrefix(id, "sidecar/") {
			return id
		}
	}
	return ""
}

// selectFallback is the safe-degradation path when the sidecar
// classifier returns low-confidence or fails entirely. With
// FallbackModel removed, it just delegates to rule-based —
// kept as a named function so the existing call sites stay
// readable. CHAT-REARCH S1.2.
func (r *Router) selectFallback(apiKeyFn func(string) string) (string, llm.Provider, error) {
	return r.selectRuleBased(apiKeyFn)
}
