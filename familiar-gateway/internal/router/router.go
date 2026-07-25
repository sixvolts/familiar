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

	// chatRole resolves the "chat" role's primary→backup→fallback chain
	// against live health. When set, GetChatModelID returns whatever it
	// picks, so chat failover happens automatically. Optional: nil falls
	// back to the historical chat=true / first-role-less selection.
	chatRole ChatRoleResolver
	// chatPrimary is the configured tier-0 chat model, kept separately so
	// ChatPrimaryID can name it while a failover is in effect.
	chatPrimary string
}

// ChatRoleResolver is the slice of modelrole.Resolver the router needs:
// "which model should serve the chat role right now?". An interface
// keeps internal/router free of an import on the resolver package and
// makes the fallback behavior trivially testable.
type ChatRoleResolver interface {
	Resolve(role string) (modelID string, tier int, ok bool)
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

// SetChatRole attaches the role resolver backing GetChatModelID, so the
// chat model follows its [roles.chat] failover chain.
func (r *Router) SetChatRole(res ChatRoleResolver) {
	r.chatRole = res
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

// GetChatModelID returns the model ID that should serve chat right now.
//
// With a chat-role resolver attached (the normal path) it walks the
// [roles.chat] chain — primary, then backup, then the global fallback —
// skipping any candidate the heartbeat reports offline, so chat fails
// over and auto-fails-back with no operator action. Falls back to the
// historical selection (an explicit chat=true model, else the first
// role-less model in lex order) when no resolver is wired or the chat
// role names no models.
func (r *Router) GetChatModelID() string {
	if r.chatRole != nil {
		if id, _, ok := r.chatRole.Resolve(config.RoleChat); ok && id != "" {
			return id
		}
	}
	return r.chatModelIDFromConfig()
}

// ChatModelTier reports which tier of the chat chain is serving (0 =
// primary, 1 = backup, …) and whether a resolver is actually driving
// the choice. Used by the maintenance/status surfaces to say *which*
// model is answering.
func (r *Router) ChatModelTier() (int, bool) {
	if r.chatRole == nil {
		return 0, false
	}
	if _, tier, ok := r.chatRole.Resolve(config.RoleChat); ok {
		return tier, true
	}
	return 0, false
}

// ChatServing returns the model id currently serving chat and its tier
// in the chain. Satisfies the maintenance controller's serving probe.
func (r *Router) ChatServing() (string, int) {
	id := r.GetChatModelID()
	tier, _ := r.ChatModelTier()
	return id, tier
}

// ChatPrimaryID returns the chat role's *configured primary* — tier 0 of
// the chain — regardless of what is currently serving. This is the model
// maintenance mode replaces and the one the "primary offline" banner
// refers to; don't use GetChatModelID for that, since it deliberately
// returns the backup during a failover.
func (r *Router) ChatPrimaryID() string {
	if r.chatPrimary != "" {
		return r.chatPrimary
	}
	return r.chatModelIDFromConfig()
}

// SetChatPrimary records the configured tier-0 chat model (from
// [roles.chat].primary) so ChatPrimaryID can report it independently of
// live resolution.
func (r *Router) SetChatPrimary(id string) { r.chatPrimary = id }

// chatModelIDFromConfig is the pre-roles selection: an explicit
// chat=true model wins (so a heavy pin-target like research can share
// the registry without stealing chat), else the first role-less model
// in lex order. Retained as the no-resolver fallback.
func (r *Router) chatModelIDFromConfig() string {
	r.registry.mu.RLock()
	defer r.registry.mu.RUnlock()
	// Explicit wins: an operator-flagged chat=true model is the chat
	// backend regardless of id ordering, so a heavy pin-target (e.g. a
	// research model) can share the registry without stealing chat.
	var flagged, roleless []string
	for id, e := range r.registry.entries {
		if e.Config.Chat {
			flagged = append(flagged, id)
		}
		if e.Config.Role == "" {
			roleless = append(roleless, id)
		}
	}
	if len(flagged) > 0 {
		sort.Strings(flagged)
		return flagged[0]
	}
	// Back-compat: no chat flag set — first role-less model in lex order.
	if len(roleless) == 0 {
		return ""
	}
	sort.Strings(roleless)
	return roleless[0]
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
