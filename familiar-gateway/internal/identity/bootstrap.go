package identity

import (
	"context"
	"fmt"
	"strings"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/db"
)

// Bootstrap idempotently seeds identity_map from operator-supplied
// config. Replaces the old hardcoded `phase_c_identity_seed` schema
// migration so seed data is portable: every deployment defines its
// own [identity.seed] entries in gateway.toml rather than inheriting
// whichever Slack workspace's IDs happened to ship in the schema.
//
// Re-running on every startup is intentional. Operator edits to
// gateway.toml propagate without manual SQL, and ON CONFLICT DO
// NOTHING means no overwrite of post-seed admin-console changes.
//
// Returns nil and no-ops when seeds is empty (the common path on
// fresh boxes that haven't configured identity yet).
func Bootstrap(ctx context.Context, pool *db.Pool, seeds []config.IdentitySeed) error {
	if pool == nil || pool.DB == nil {
		return fmt.Errorf("identity bootstrap: nil pool")
	}
	if len(seeds) == 0 {
		return nil
	}

	// Validate up front so a single typo doesn't surface as a half-
	// applied seed mid-loop. All seeds either pass or none run.
	for i, s := range seeds {
		if strings.TrimSpace(s.Platform) == "" {
			return fmt.Errorf("identity.seed[%d]: platform is required", i)
		}
		if strings.TrimSpace(s.PlatformID) == "" {
			return fmt.Errorf("identity.seed[%d] (%s): platform_id is required", i, s.Platform)
		}
		if strings.TrimSpace(s.CanonicalID) == "" {
			return fmt.Errorf("identity.seed[%d] (%s/%s): canonical_id is required",
				i, s.Platform, s.PlatformID)
		}
	}

	for _, s := range seeds {
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO identity_map (platform, platform_id, canonical_id, display_name)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (platform, platform_id) DO NOTHING
		`, s.Platform, s.PlatformID, s.CanonicalID, s.DisplayName); err != nil {
			return fmt.Errorf("identity bootstrap %s/%s -> %s: %w",
				s.Platform, s.PlatformID, s.CanonicalID, err)
		}
	}
	return nil
}
