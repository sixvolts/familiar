// Package identity resolves platform-specific user identities (Slack
// user IDs, OpenAI "user" field, CLI session IDs) to a canonical user
// identity used by the profile store, memory scoping, and fact
// attribution.
//
// The mapping lives in the identity_map table and is fully cached in
// memory at startup — the table stays tiny (< 20 rows in practice) and
// lookups are on the hot path for every request.
package identity

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/familiar/gateway/internal/db"
)

// Resolver maps (platform, platform_id) pairs to canonical user IDs.
// Nil resolvers are safe — Resolve on nil reports "unmapped" so callers
// can decide whether to error out or hand-roll a fallback. OWNER-
// MIGRATION dropped the legacy defaultID escape hatch; identity that
// can't be resolved must now produce a hard error at the call site
// rather than silently becoming the implicit "owner" user.
type Resolver struct {
	db *db.Pool

	mu         sync.RWMutex
	cache      map[string]string     // "platform:platform_id" → canonical id
	userStatus map[string]UserStatus // canonical id → status
	userName   map[string]string     // canonical id → display name
	warned     map[string]bool
}

// NewResolver creates a resolver backed by the identity_map table.
// The table must already exist (run db.Migrate first) — the resolver
// just loads the full table into its in-memory cache. Pass pool=nil
// to get an in-memory-only resolver (useful for tests).
func NewResolver(ctx context.Context, pool *db.Pool) (*Resolver, error) {
	r := &Resolver{
		db:         pool,
		cache:      make(map[string]string),
		userStatus: make(map[string]UserStatus),
		userName:   make(map[string]string),
		warned:     make(map[string]bool),
	}
	if pool == nil {
		return r, nil
	}
	if err := r.reload(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Resolver) reload(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx,
		`SELECT platform, platform_id, canonical_id FROM identity_map`)
	if err != nil {
		return fmt.Errorf("identity: load cache: %w", err)
	}
	defer rows.Close()

	fresh := make(map[string]string)
	for rows.Next() {
		var platform, pid, canonical string
		if err := rows.Scan(&platform, &pid, &canonical); err != nil {
			return err
		}
		fresh[platform+":"+pid] = canonical
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	// Load user status alongside the link cache so ResolveWithStatus
	// can answer without a DB round-trip.
	userRows, err := r.db.QueryContext(ctx,
		`SELECT id, display_name, status FROM users`)
	if err != nil {
		return fmt.Errorf("identity: load users: %w", err)
	}
	defer userRows.Close()

	freshStatus := make(map[string]UserStatus)
	freshName := make(map[string]string)
	for userRows.Next() {
		var id, name, status string
		if err := userRows.Scan(&id, &name, &status); err != nil {
			return err
		}
		freshStatus[id] = UserStatus(status)
		freshName[id] = name
	}
	if err := userRows.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	r.cache = fresh
	r.userStatus = freshStatus
	r.userName = freshName
	r.mu.Unlock()
	return nil
}

// Resolve maps a platform-specific identity to the canonical user ID.
// Returns (canonicalID, true) on a hit, ("", false) when no mapping
// exists. The "unmapped" path is a one-time WARN per (platform,
// platform_id) pair so operators notice unexpected connections without
// being flooded — but callers MUST treat ok=false as a hard error and
// reject the request. OWNER-MIGRATION removed the legacy defaultID
// fallback that used to silently route every unknown sender to the
// "owner" canonical id, leaking data across users.
func (r *Resolver) Resolve(platform, platformID string) (string, bool) {
	if r == nil || platform == "" || platformID == "" {
		return "", false
	}
	key := platform + ":" + platformID

	r.mu.RLock()
	canonical, ok := r.cache[key]
	r.mu.RUnlock()
	if ok {
		return canonical, true
	}

	r.mu.Lock()
	if !r.warned[key] {
		r.warned[key] = true
		log.Printf("[identity] unmapped %s — rejecting (no defaultID fallback after OWNER-MIGRATION)", key)
	}
	r.mu.Unlock()
	return "", false
}

// Register adds or updates a mapping, writing through to the database
// when one is configured and always updating the in-memory cache.
func (r *Resolver) Register(ctx context.Context, platform, platformID, canonicalID, displayName string) error {
	if r == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	if platform == "" || platformID == "" || canonicalID == "" {
		return fmt.Errorf("identity: platform, platform_id, canonical_id are required")
	}

	if r.db != nil {
		_, err := r.db.ExecContext(ctx,
			`INSERT INTO identity_map (platform, platform_id, canonical_id, display_name)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (platform, platform_id) DO UPDATE
			 SET canonical_id = EXCLUDED.canonical_id,
			     display_name = EXCLUDED.display_name`,
			platform, platformID, canonicalID, displayName)
		if err != nil {
			return fmt.Errorf("identity: register: %w", err)
		}
	}

	key := platform + ":" + platformID
	r.mu.Lock()
	r.cache[key] = canonicalID
	delete(r.warned, key)
	r.mu.Unlock()
	return nil
}

// Count reports how many mappings are currently cached. Used only for
// startup logging.
func (r *Resolver) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}
