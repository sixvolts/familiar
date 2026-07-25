// Package modelrole resolves a logical role (chat, classify, embedder,
// …) to a concrete model ID at call time, walking that role's
// primary→backup→fallback chain and skipping any candidate the
// heartbeat currently reports offline.
//
// The resolver is the single place failover and auto-failback happen:
// it holds no state of its own beyond the static chains, so both
// behaviors fall out of reading live health. When a primary's health
// flips to "offline" (after the registry's debounce threshold) Resolve
// starts returning the backup; when it flips back to "online" Resolve
// returns the primary again — no toggles, no timers here.
//
// Health is injected as a func so this package imports nothing but the
// standard library (no router/registry import cycle), mirroring how
// internal/maintenance keeps itself decoupled.
package modelrole

import (
	"sort"
	"strings"
	"sync"
)

// Health status strings, matching router.Registry.StatusOf.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
	StatusUnknown = "unknown"
)

// HealthFn reports a model's current health: "online", "offline", or
// "unknown" (also "" for an unregistered id). Only "offline" removes a
// candidate from consideration — "unknown" (never probed / probe
// pending) stays usable so a cold-started primary is preferred over its
// backup until a probe actually condemns it.
type HealthFn func(modelID string) string

// Resolver maps role → ordered candidate chain and resolves against
// live health. Safe for concurrent use.
type Resolver struct {
	mu     sync.RWMutex
	chains map[string][]string
	health HealthFn
}

// New builds a resolver from role → ordered candidate IDs (already
// including the global fallback as the terminal element, e.g. from
// config.RolesConfig.ResolvedChains). A nil health func treats every
// model as usable (every status reads as "unknown"), which is the
// correct degrade for a resolver wired before health checks start.
func New(chains map[string][]string, health HealthFn) *Resolver {
	cp := make(map[string][]string, len(chains))
	for role, cands := range chains {
		cp[role] = append([]string(nil), cands...)
	}
	return &Resolver{chains: cp, health: health}
}

// SetHealth swaps the health source (used when the registry is built
// after the resolver, or in tests).
func (r *Resolver) SetHealth(fn HealthFn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health = fn
}

func (r *Resolver) status(id string) string {
	if r.health == nil {
		return StatusUnknown
	}
	if s := r.health(id); s != "" {
		return s
	}
	return StatusUnknown
}

// Resolve returns the model ID to use for a role, its tier (0 =
// primary, 1 = backup, 2 = fallback, …), and ok=false when the role
// names no models at all.
//
// It returns the first candidate whose health is not "offline" —
// priority order dominates, so the primary is used unless it is
// confirmed offline, then the backup, then the fallback. When every
// candidate is offline it returns the primary (tier 0) as a
// best-effort last resort, leaving the actual failure to the caller's
// provider call rather than pretending the role is unconfigured.
func (r *Resolver) Resolve(role string) (modelID string, tier int, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	chain := r.chains[role]
	if len(chain) == 0 {
		return "", 0, false
	}
	for i, id := range chain {
		if r.status(id) != StatusOffline {
			return id, i, true
		}
	}
	return chain[0], 0, true
}

// Status reports a model's current health ("online" / "offline" /
// "unknown"). Exposed so consumers that already hold a resolver (the
// sidecar client, the admin surface) don't need a second handle on the
// registry just to check whether a resolved model is usable.
func (r *Resolver) Status(modelID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status(modelID)
}

// ResolveID is the common case: just the model ID (empty when the role
// is unconfigured).
func (r *Resolver) ResolveID(role string) string {
	id, _, ok := r.Resolve(role)
	if !ok {
		return ""
	}
	return id
}

// Chain returns a copy of a role's ordered candidate list (for startup
// logging and the admin status surface).
func (r *Resolver) Chain(role string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.chains[role]...)
}

// Roles returns the configured role names in sorted order.
func (r *Resolver) Roles() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.chains))
	for role := range r.chains {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

// CandidateStatus pairs a candidate model ID with its live health and
// whether it is the currently-serving tier — the shape the admin
// dashboard renders per role.
type CandidateStatus struct {
	ModelID string `json:"model_id"`
	Tier    int    `json:"tier"`
	Status  string `json:"status"`
	Active  bool   `json:"active"`
}

// RoleStatus is a role's full chain with live health and the active
// tier resolved once, for the admin/status surface.
type RoleStatus struct {
	Role       string            `json:"role"`
	ActiveID   string            `json:"active_id"`
	ActiveTier int               `json:"active_tier"`
	Candidates []CandidateStatus `json:"candidates"`
}

// Snapshot resolves every role once and reports the chain + live health
// + active tier. Used by the dashboard and startup log.
func (r *Resolver) Snapshot() []RoleStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	roles := make([]string, 0, len(r.chains))
	for role := range r.chains {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	out := make([]RoleStatus, 0, len(roles))
	for _, role := range roles {
		chain := r.chains[role]
		// Active tier: first non-offline, else tier 0 (mirrors Resolve).
		activeTier := 0
		for i, id := range chain {
			if r.status(id) != StatusOffline {
				activeTier = i
				break
			}
		}
		rs := RoleStatus{Role: role, ActiveTier: activeTier}
		if len(chain) > 0 {
			rs.ActiveID = chain[activeTier]
		}
		for i, id := range chain {
			rs.Candidates = append(rs.Candidates, CandidateStatus{
				ModelID: id,
				Tier:    i,
				Status:  r.status(id),
				Active:  i == activeTier,
			})
		}
		out = append(out, rs)
	}
	return out
}

// FormatChain renders a role's chain for a one-line startup log, e.g.
// "chat → gpu/qwen(online), gpu/qwen-30b(unknown)".
func (r *Resolver) FormatChain(role string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	chain := r.chains[role]
	parts := make([]string, 0, len(chain))
	for _, id := range chain {
		parts = append(parts, id+"("+r.status(id)+")")
	}
	return role + " → " + strings.Join(parts, ", ")
}
