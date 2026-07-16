package main

import (
	"context"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/skills"
)

// gatewayStatusProvider implements admin.StatusProvider against the
// live gateway subsystems. It is constructed once in main() after
// every dependency is known and handed to the admin handler so the
// admin package never has to import router/memory/identity/skills.
type gatewayStatusProvider struct {
	startTime time.Time
	registry  *router.Registry
	memStore  *memory.PgVectorStore // optional
	resolver  *identity.Resolver    // optional
	sessions  *session.Manager
	skills    *skills.Registry // optional
}

func (p *gatewayStatusProvider) Snapshot(ctx context.Context) (admin.StatusSnapshot, error) {
	snap := admin.StatusSnapshot{
		Gateway: admin.GatewayStatus{
			UptimeSeconds: int64(time.Since(p.startTime).Seconds()),
		},
	}

	// Models — one entry per registered router model with current
	// health status and provider. Endpoint is included so misrouted
	// configs are visible from the dashboard.
	if p.registry != nil {
		for _, id := range p.registry.ModelIDs() {
			cfg := p.registry.GetModelConfig(id)
			if cfg == nil {
				continue
			}
			snap.Models = append(snap.Models, admin.ModelStatus{
				ID:       id,
				Provider: cfg.Provider,
				Status:   p.registry.StatusOf(id),
				Endpoint: cfg.Endpoint,
			})
		}
	}

	// Memory — total rows only. by_scope / by_user aggregates could
	// land later if the dashboard wants the breakdown; for now the
	// single count keeps the query cheap and the UI simple.
	if p.memStore != nil {
		total, err := p.memStore.CountMemories(ctx, memory.MemoryFilter{})
		if err == nil {
			snap.Memory.Total = total
		}
	}

	// Users — bucket by status. ListUsers with a nil filter returns
	// every row regardless of status, and we count locally.
	if p.resolver != nil {
		users, err := p.resolver.ListUsers(ctx, nil)
		if err == nil {
			snap.Users.Total = len(users)
			for _, u := range users {
				switch u.Status {
				case identity.StatusApproved:
					snap.Users.Approved++
				case identity.StatusPending:
					snap.Users.Pending++
				}
			}
		}
	}

	// Sessions — in-memory count from the session manager.
	if p.sessions != nil {
		snap.Sessions.Active = p.sessions.Count()
	}

	// Skills — registered skill names (not tools). The dashboard
	// renders this as a single line, so the shape stays flat.
	if p.skills != nil {
		snap.Skills = p.skills.SkillNames()
	}
	if snap.Skills == nil {
		snap.Skills = []string{}
	}
	if snap.Models == nil {
		snap.Models = []admin.ModelStatus{}
	}

	return snap, nil
}
