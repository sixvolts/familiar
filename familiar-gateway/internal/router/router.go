package router

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/llm"
)

// Router selects an LLM provider for each incoming message.
type Router struct {
	cfg      config.RouterConfig
	registry *Registry

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
	// Chain returns the role's ordered candidate model IDs
	// (primary → backup → global fallback) for completion-level failover.
	Chain(role string) []string
}

// NewRouter constructs a Router from config and a model registry.
func NewRouter(cfg config.RouterConfig, registry *Registry) *Router {
	return &Router{cfg: cfg, registry: registry}
}

// SetChatRole attaches the role resolver backing GetChatModelID, so the
// chat model follows its [roles.chat] failover chain.
func (r *Router) SetChatRole(res ChatRoleResolver) {
	r.chatRole = res
}

// GetChatChain returns the ordered [roles.chat] candidate model IDs
// (primary → backup → global fallback) for completion-level failover.
// Empty when no chat-role resolver is wired.
func (r *Router) GetChatChain() []string {
	if r.chatRole == nil {
		return nil
	}
	return r.chatRole.Chain(config.RoleChat)
}

// Select picks a chat model when no chat model resolves (the pipeline
// calls it only then): an online model that can answer chat, local
// first when prefer_local is set, in id order. Returns (modelID,
// provider, error).
//
// It used to try [[router.rules.force]] first, which therefore applied
// only in configs with no chat model at all, and then took a random
// online model (map order), embeddings models included. Force rules are
// no longer read (Validate warns when they're set).
func (r *Router) Select(ctx context.Context, msg string, channelID string, apiKeyFn func(string) string) (string, llm.Provider, error) {
	return r.selectRuleBased(apiKeyFn)
}

// selectRuleBased is the rule-based pick behind Select.
func (r *Router) selectRuleBased(apiKeyFn func(string) string) (string, llm.Provider, error) {
	online := r.registry.Online()
	sort.Strings(online)
	var local, rest []string
	for _, id := range online {
		r.registry.mu.RLock()
		entry := r.registry.entries[id]
		r.registry.mu.RUnlock()
		if entry == nil || entry.Config.Provider == "embeddings" {
			continue
		}
		if r.cfg.PreferLocal && entry.Config.LatencyProfile == "local" {
			local = append(local, id)
		} else {
			rest = append(rest, id)
		}
	}
	for _, id := range append(local, rest...) {
		if p, err := r.registry.GetProvider(id, apiKeyFn); err == nil {
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
		// An embeddings model can't answer chat (see
		// config.deriveChatModelID).
		if e.Config.Provider == "embeddings" {
			continue
		}
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

// GetSidecarModelID returns the model shard tier1/tier2 invocations run
// on: what the classify role resolves to right now (its failover chain,
// offline candidates skipped), the fast model the operator put on the
// critical path. It used to be the first registry id starting
// "sidecar/", in map order: a prefix the example config calls
// informational ("mac/gemma" broke every tier1 shard), a random pick
// between two such models, and no health check. With no classify
// model (no sidecar configured) they run on the chat model, logged:
// failing every such shard would be worse. Without a role resolver
// (tests) the "sidecar/" prefix is still used, in id order.
func (r *Router) GetSidecarModelID() string {
	if r.chatRole != nil {
		if id, _, ok := r.chatRole.Resolve(config.RoleClassify); ok && id != "" {
			return id
		}
		id := r.GetChatModelID()
		log.Printf("[router] tier1/tier2 shard: no classify model resolves; running on the chat model %q", id)
		return id
	}
	r.registry.mu.RLock()
	defer r.registry.mu.RUnlock()
	var ids []string
	for id := range r.registry.entries {
		if strings.HasPrefix(id, "sidecar/") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
