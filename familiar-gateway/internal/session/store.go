package session

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/familiar/gateway/internal/db"
)

// Store persists the rolling summary across gateway restarts. It shares
// the connection pool owned by internal/db — Close is a no-op so pool
// lifetime stays with main().
//
// The table is keyed by the stable (channel_id, sender_id) pair rather
// than the per-process Session.ID, which rotates on every restart.
// Schema is managed by internal/db.Migrate, not here.
type Store struct {
	db *db.Pool
}

// NewStore returns a Store backed by the shared pool. The sessions
// table must already exist — run db.Migrate before constructing stores.
func NewStore(pool *db.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("session: nil pool")
	}
	return &Store{db: pool}, nil
}

// Load returns the persisted summary + summarized turn count for a
// session key. Missing rows return zero values and a nil error.
func (s *Store) Load(ctx context.Context, key string) (summary string, count int, err error) {
	if key == "" {
		return "", 0, nil
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT running_summary, summarized_count FROM sessions WHERE session_key = $1`,
		key,
	).Scan(&summary, &count)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("session: load %s: %w", key, err)
	}
	return summary, count, nil
}

// Save upserts the rolling summary for a session key.
//
// scopeTag is empty for top-level Familiar sessions and non-empty for
// sessions belonging to a persistent shard (FAMILIAR-SHARDS-PHASE1-SPEC).
// An empty value persists as SQL NULL via NULLIF, keeping the
// "no-shard" sentinel distinct from any literal scope_tag a future
// caller might use. The COALESCE in the conflict clause preserves an
// existing tag if a re-save comes through with an empty value — once a
// session is bound to a shard, an unscoped touch never unbinds it.
// (Top-level and shard-invoked sessions are expected to use disjoint
// session keys; this defensive COALESCE only matters if Step 7's
// invocation endpoint ever picks a colliding key.)
func (s *Store) Save(ctx context.Context, key, summary string, count int, scopeTag string) error {
	if key == "" {
		return fmt.Errorf("session: empty key")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_key, running_summary, summarized_count, scope_tag, updated_at)
		VALUES ($1, $2, $3, NULLIF($4::text, ''), NOW())
		ON CONFLICT (session_key) DO UPDATE
		SET running_summary  = EXCLUDED.running_summary,
		    summarized_count = EXCLUDED.summarized_count,
		    scope_tag        = COALESCE(EXCLUDED.scope_tag, sessions.scope_tag),
		    updated_at       = NOW()
	`, key, summary, count, scopeTag)
	if err != nil {
		return fmt.Errorf("session: save %s: %w", key, err)
	}
	return nil
}

// Close is a no-op — the Store borrows its *sql.DB.
func (s *Store) Close() error { return nil }

// Key returns the canonical persistence key for a (channelID, senderID)
// pair. Kept as a package-level helper so every call site agrees on the
// separator.
func Key(channelID, senderID string) string {
	return channelID + "|" + senderID
}
