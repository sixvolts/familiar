package shards

import "context"

// Store is the persistence contract for shards and their tokens. The
// Postgres implementation lives in pg.go; tests use the same interface
// so later phases can swap in a mock or an in-memory store without
// rewriting callers.
//
// All Get/List operations return zero-value slices (not nil) when the
// owner has no rows, so JSON marshalling produces `[]` rather than
// `null` for the admin UI.
type Store interface {
	// CreateShard inserts a new shard. Validates slug format, mode
	// enums, scope_tag, and ephemeral+write-tool rules before touching
	// the DB. Returns ErrInvalidSlug / ErrInvalidScopeTag / ErrInvalidMode
	// / ErrWriteToolOnEph on a pre-flight failure. Duplicate (owner,
	// scope_tag) or duplicate id are surfaced as a wrapped DB error.
	CreateShard(ctx context.Context, s *Shard) error

	// GetShard returns the shard by ID regardless of disabled state.
	// Callers (e.g. the invoke handler) check s.Active() themselves so
	// "disabled" can be reported distinctly from "not found."
	GetShard(ctx context.Context, id string) (*Shard, error)

	// ListShards returns all shards owned by the given user, newest
	// first. Disabled shards are included so admins can re-enable them.
	ListShards(ctx context.Context, ownerID string) ([]*Shard, error)

	// ListAllShards returns every shard on the instance, newest first.
	// Used by the admin role in the console — non-admin callers must
	// stick to ListShards with their own ownerID. The handler layer
	// enforces "only admin can call this." Disabled shards are
	// included for the same reason as ListShards.
	ListAllShards(ctx context.Context) ([]*Shard, error)

	// UpdateShard rewrites the mutable fields of a shard. id and
	// owner_id are not changeable; attempting to pass a different
	// owner_id is silently ignored (the WHERE clause keys on id alone).
	// Re-runs the same validation CreateShard does.
	UpdateShard(ctx context.Context, s *Shard) error

	// DisableShard sets disabled_at=NOW(). Tokens are not revoked —
	// they stop working because the shard is disabled, not because the
	// tokens are. Re-enabling the shard re-enables every non-revoked
	// token without manual intervention.
	DisableShard(ctx context.Context, id string) error

	// EnableShard clears disabled_at.
	EnableShard(ctx context.Context, id string) error

	// DeleteShard removes the shard and (via ON DELETE CASCADE) its
	// tokens and passkeys. Intended for shards that were mistakes; for
	// routine decommissioning, Disable is preferred because it
	// preserves audit history.
	//
	// Scheduled actions referencing the shard are NOT deleted — their
	// FK is ON DELETE SET NULL. The admin delete handler disables them
	// first (SCHEDULED-ACTIONS-SPEC: a deleted envelope must not
	// silently become a full-capability trusted run). Callers reaching
	// this store method directly owe their dependents the same care.
	DeleteShard(ctx context.Context, id string) error

	// CreateToken mints a new bearer token for an (owner, shard) pair.
	// Returns the plaintext value exactly once — the store keeps only
	// the bcrypt hash. The returned Token mirrors the inserted row.
	CreateToken(ctx context.Context, ownerID, shardID, label string) (plaintext string, t *Token, err error)

	// ListTokens returns all tokens for a shard, newest first, including
	// revoked tokens (so the UI can show a complete history).
	ListTokens(ctx context.Context, shardID string) ([]*Token, error)

	// ValidateToken checks the plaintext bearer against stored hashes.
	// Returns ErrTokenNotFound if no row matches, ErrTokenRevoked if the
	// match is revoked. A nil error guarantees a non-nil, active token
	// whose shard row can be loaded. Does NOT update last_used_at —
	// callers invoke TouchToken once they've confirmed the full
	// authorization (shard match, owner match) succeeded, so failed
	// attempts don't tread on timing-sensitive columns.
	ValidateToken(ctx context.Context, plaintext string) (*Token, error)

	// TouchToken updates last_used_at=NOW() on a successful invocation.
	// Errors here are logged but not fatal — a failed touch must not
	// fail the request.
	TouchToken(ctx context.Context, id string) error

	// RevokeToken sets revoked_at=NOW(). Idempotent: revoking an
	// already-revoked token is a no-op and returns nil.
	RevokeToken(ctx context.Context, id string) error
}
