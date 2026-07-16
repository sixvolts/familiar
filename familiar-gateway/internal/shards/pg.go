package shards

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// TokenPlaintextPrefix is the literal string every shard-token
// plaintext begins with, before the random portion. It is visible in
// the plaintext and in the stored token_prefix column, which makes
// shard tokens easy to distinguish from other bearer credentials in
// logs and greppable in code that looks for them. Renamed from
// `puppet_` to `shard_` in FAMILIAR-CONSOLE-SPEC Phase A; existing
// `puppet_*` plaintexts in any deploy must be re-minted (they were
// never persisted to the DB outside of operator notes — only their
// bcrypt hashes — but the prefix index lookup will miss after the
// rename, so the practical effect is the same: revoke + re-mint).
const TokenPlaintextPrefix = "shard_"

// tokenPrefixLen is how many leading characters of the plaintext we
// keep unhashed for UI display and for indexing ValidateToken lookups.
// Matches FAMILIAR-SHARDS-PHASE1-SPEC §"shard_tokens table".
const tokenPrefixLen = 8

// bcryptCost trades hash time for brute-force resistance. Default 10
// is ~60ms on commodity hardware, fine for the low-QPS invoke path.
const bcryptCost = bcrypt.DefaultCost

// PGStore is the production Store backed by the shared gateway pool.
// Constructed via NewStore; the pool is borrowed, so Close is a no-op.
type PGStore struct {
	db *db.Pool
}

// Compile-time assertion that PGStore satisfies Store. If a later
// change to the interface breaks the impl, the build fails here
// instead of at the first handler that tries to use it.
var _ Store = (*PGStore)(nil)

// NewStore returns a Store backed by the shared Postgres pool. The
// shards and shard_tokens tables must already exist — run db.Migrate
// before calling this. Kept minimal deliberately: no caching, no
// retries, no background goroutines. Higher layers add what they need.
func NewStore(pool *db.Pool) (*PGStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("shards: nil pool")
	}
	return &PGStore{db: pool}, nil
}

// Close is a no-op. The store borrows its pool from db.Open.
func (s *PGStore) Close() error { return nil }

// -----------------------------------------------------------------------------
// Shard CRUD
// -----------------------------------------------------------------------------

// validateShard is the shared pre-flight for CreateShard and
// UpdateShard. Passing known=nil skips the registry check (callers that
// don't have a live registry handy just validate shape).
func validateShard(s *Shard, known map[string]bool) error {
	if s == nil {
		return fmt.Errorf("shards: nil shard")
	}
	if err := ValidateSlug(s.ID); err != nil {
		return err
	}
	if s.OwnerID == "" {
		return fmt.Errorf("shards: empty owner_id")
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("shards: empty name")
	}
	if !s.Persistence.Valid() {
		return fmt.Errorf("%w: persistence=%q", ErrInvalidMode, s.Persistence)
	}
	if !s.Visibility.Valid() {
		return fmt.Errorf("%w: visibility=%q", ErrInvalidMode, s.Visibility)
	}
	if err := ValidateScopeTag(s.ScopeTag); err != nil {
		return err
	}
	// SHARD-AUTH-SPEC: a shard with chat and API both disabled is a
	// "zombie" — console sessions only, no prompt-driven inference.
	// Skip the prompt requirement so console-only shards can be
	// saved without a placeholder system prompt.
	if s.SystemPrompt == "" && (s.ChatEnabled || s.APIEnabled) {
		return fmt.Errorf("shards: empty system_prompt")
	}
	if s.ModelPreference != "" && s.TierPreference != "" {
		return fmt.Errorf("shards: model_preference and tier_preference are mutually exclusive")
	}
	if s.MaxTokens <= 0 {
		return fmt.Errorf("shards: max_tokens must be positive")
	}
	if s.Temperature < 0 || s.Temperature > 2 {
		return fmt.Errorf("shards: temperature out of range [0,2]")
	}
	if err := ValidateAllowlist(s.ToolAllowlist, s.Persistence, known); err != nil {
		return err
	}
	return nil
}

// textArray adapts a string slice for the NOT NULL TEXT[] columns
// (console_panels, book_access): pq.StringArray(nil) drives a SQL NULL,
// which the schema rejects — a nil slice must land as '{}'.
func textArray(v []string) pq.StringArray {
	if v == nil {
		return pq.StringArray{}
	}
	return pq.StringArray(v)
}

// CreateShard inserts a new shard row. Runs full validation before
// touching the DB; any integrity violation (duplicate id, duplicate
// (owner, scope_tag)) surfaces as a wrapped DB error.
func (s *PGStore) CreateShard(ctx context.Context, sh *Shard) error {
	if err := validateShard(sh, nil); err != nil {
		return err
	}
	allowlist, err := json.Marshal(sh.ToolAllowlist)
	if err != nil {
		return fmt.Errorf("shards: marshal allowlist: %w", err)
	}
	inputSchema := nullableJSON(sh.InputSchema)
	outputSchema := nullableJSON(sh.OutputSchema)

	consolePanels := textArray(sh.ConsolePanels)
	bookAccess := textArray(sh.BookAccess)
	var sessionMaxAge sql.NullInt32
	if sh.SessionMaxAge != nil {
		sessionMaxAge = sql.NullInt32{Int32: int32(*sh.SessionMaxAge), Valid: true}
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO shards (
		    id, owner_id, name, description, persistence, visibility,
		    scope_tag, tool_allowlist, system_prompt, model_preference,
		    tier_preference, input_schema, output_schema, max_tokens,
		    temperature, console_access, console_panels, book_access,
		    chat_enabled, api_enabled, session_max_age,
		    created_at, updated_at
		) VALUES (
		    $1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11,
		    $12::jsonb, $13::jsonb, $14, $15, $16, $17, $18,
		    $19, $20, $21, NOW(), NOW()
		)
	`,
		sh.ID, sh.OwnerID, sh.Name, sh.Description,
		string(sh.Persistence), string(sh.Visibility),
		sh.ScopeTag, string(allowlist), sh.SystemPrompt,
		sh.ModelPreference, sh.TierPreference,
		inputSchema, outputSchema,
		sh.MaxTokens, sh.Temperature,
		sh.ConsoleAccess, consolePanels, bookAccess,
		sh.ChatEnabled, sh.APIEnabled, sessionMaxAge,
	)
	if err != nil {
		return fmt.Errorf("shards: create %s: %w", sh.ID, err)
	}
	return nil
}

// GetShard loads a shard by ID. Returns ErrShardNotFound if the row
// does not exist; disabled shards are returned (callers decide).
func (s *PGStore) GetShard(ctx context.Context, id string) (*Shard, error) {
	row := s.db.QueryRowContext(ctx, selectShardColumns+` FROM shards WHERE id = $1`, id)
	sh, err := scanShard(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrShardNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("shards: get %s: %w", id, err)
	}
	return sh, nil
}

// ListShards returns every shard owned by the given user, newest
// first. An owner with no shards gets an empty slice, not nil.
func (s *PGStore) ListShards(ctx context.Context, ownerID string) ([]*Shard, error) {
	if ownerID == "" {
		return nil, fmt.Errorf("shards: empty owner_id")
	}
	return s.queryShards(ctx,
		selectShardColumns+` FROM shards WHERE owner_id = $1 ORDER BY created_at DESC`,
		ownerID)
}

// ListAllShards returns every shard on the instance, newest first.
// Admin-only at the handler layer; the store doesn't enforce role
// (it's just a SELECT without an owner predicate).
func (s *PGStore) ListAllShards(ctx context.Context) ([]*Shard, error) {
	return s.queryShards(ctx,
		selectShardColumns+` FROM shards ORDER BY created_at DESC`)
}

// queryShards is the shared scan loop for any SELECT producing rows
// in selectShardColumns shape. Kept here so ListShards / ListAllShards
// can't drift on error wrapping or empty-slice semantics.
func (s *PGStore) queryShards(ctx context.Context, query string, args ...any) ([]*Shard, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("shards: query: %w", err)
	}
	defer rows.Close()

	out := make([]*Shard, 0)
	for rows.Next() {
		sh, err := scanShard(rows)
		if err != nil {
			return nil, fmt.Errorf("shards: scan: %w", err)
		}
		out = append(out, sh)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shards: rows: %w", err)
	}
	return out, nil
}

// UpdateShard rewrites the editable fields on a shard. Identity
// (id, owner_id) and timestamps (created_at) are immutable here;
// updated_at is bumped to NOW().
func (s *PGStore) UpdateShard(ctx context.Context, sh *Shard) error {
	if err := validateShard(sh, nil); err != nil {
		return err
	}
	allowlist, err := json.Marshal(sh.ToolAllowlist)
	if err != nil {
		return fmt.Errorf("shards: marshal allowlist: %w", err)
	}
	consolePanels := textArray(sh.ConsolePanels)
	bookAccess := textArray(sh.BookAccess)
	var sessionMaxAge sql.NullInt32
	if sh.SessionMaxAge != nil {
		sessionMaxAge = sql.NullInt32{Int32: int32(*sh.SessionMaxAge), Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE shards SET
		    name              = $2,
		    description       = $3,
		    persistence       = $4,
		    visibility        = $5,
		    scope_tag         = $6,
		    tool_allowlist    = $7::jsonb,
		    system_prompt     = $8,
		    model_preference  = $9,
		    tier_preference   = $10,
		    input_schema      = $11::jsonb,
		    output_schema     = $12::jsonb,
		    max_tokens        = $13,
		    temperature       = $14,
		    console_access    = $15,
		    console_panels    = $16,
		    book_access       = $17,
		    chat_enabled      = $18,
		    api_enabled       = $19,
		    session_max_age   = $20,
		    updated_at        = NOW()
		WHERE id = $1
	`,
		sh.ID, sh.Name, sh.Description,
		string(sh.Persistence), string(sh.Visibility),
		sh.ScopeTag, string(allowlist), sh.SystemPrompt,
		sh.ModelPreference, sh.TierPreference,
		nullableJSON(sh.InputSchema), nullableJSON(sh.OutputSchema),
		sh.MaxTokens, sh.Temperature,
		sh.ConsoleAccess, consolePanels, bookAccess,
		sh.ChatEnabled, sh.APIEnabled, sessionMaxAge,
	)
	if err != nil {
		return fmt.Errorf("shards: update %s: %w", sh.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrShardNotFound
	}
	return nil
}

// DisableShard sets disabled_at=NOW() on a shard.
func (s *PGStore) DisableShard(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shards SET disabled_at = NOW(), updated_at = NOW() WHERE id = $1 AND disabled_at IS NULL`,
		id,
	)
	if err != nil {
		return fmt.Errorf("shards: disable %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the row doesn't exist or it's already disabled. Disambiguate.
		if _, getErr := s.GetShard(ctx, id); errors.Is(getErr, ErrShardNotFound) {
			return ErrShardNotFound
		}
	}
	return nil
}

// EnableShard clears disabled_at.
func (s *PGStore) EnableShard(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shards SET disabled_at = NULL, updated_at = NOW() WHERE id = $1 AND disabled_at IS NOT NULL`,
		id,
	)
	if err != nil {
		return fmt.Errorf("shards: enable %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, getErr := s.GetShard(ctx, id); errors.Is(getErr, ErrShardNotFound) {
			return ErrShardNotFound
		}
	}
	return nil
}

// DeleteShard removes the shard row; tokens are cascaded by the FK.
func (s *PGStore) DeleteShard(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM shards WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("shards: delete %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrShardNotFound
	}
	return nil
}

// -----------------------------------------------------------------------------
// Tokens
// -----------------------------------------------------------------------------

// CreateToken mints a new bearer token. Validates that the target
// shard exists and is owned by ownerID before inserting so the FK/
// ownership constraint is enforced with a clearer error than the raw
// DB violation. Returns plaintext exactly once.
func (s *PGStore) CreateToken(ctx context.Context, ownerID, shardID, label string) (string, *Token, error) {
	if ownerID == "" {
		return "", nil, fmt.Errorf("shards: empty owner_id")
	}
	if shardID == "" {
		return "", nil, fmt.Errorf("shards: empty shard_id")
	}
	sh, err := s.GetShard(ctx, shardID)
	if err != nil {
		return "", nil, err
	}
	if sh.OwnerID != ownerID {
		return "", nil, fmt.Errorf("shards: owner mismatch: token owner %q != shard owner %q", ownerID, sh.OwnerID)
	}

	plaintext, err := generateTokenPlaintext()
	if err != nil {
		return "", nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
	if err != nil {
		return "", nil, fmt.Errorf("shards: bcrypt: %w", err)
	}
	prefix := plaintext[:tokenPrefixLen]
	id := uuid.NewString()

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO shard_tokens (id, shard_id, owner_id, label, token_hash, token_prefix, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, id, shardID, ownerID, label, string(hash), prefix)
	if err != nil {
		return "", nil, fmt.Errorf("shards: insert token: %w", err)
	}

	tok := &Token{
		ID:          id,
		ShardID:     shardID,
		OwnerID:     ownerID,
		Label:       label,
		TokenPrefix: prefix,
		CreatedAt:   time.Now().UTC(),
	}
	return plaintext, tok, nil
}

// ListTokens returns every token (active, expired, and revoked) for
// the shard, newest first. The plaintext is never reconstructible.
func (s *PGStore) ListTokens(ctx context.Context, shardID string) ([]*Token, error) {
	if shardID == "" {
		return nil, fmt.Errorf("shards: empty shard_id")
	}
	rows, err := s.db.QueryContext(ctx,
		selectTokenColumns+` FROM shard_tokens WHERE shard_id = $1 ORDER BY created_at DESC`,
		shardID,
	)
	if err != nil {
		return nil, fmt.Errorf("shards: list tokens %s: %w", shardID, err)
	}
	defer rows.Close()

	out := make([]*Token, 0)
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("shards: list tokens %s: scan: %w", shardID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shards: list tokens %s: rows: %w", shardID, err)
	}
	return out, nil
}

// ValidateToken checks a plaintext bearer against the candidate rows
// selected by token_prefix. A blank plaintext, an unknown prefix, or a
// prefix-match without a hash-match all return ErrTokenNotFound so the
// caller cannot use the error to distinguish "we have a row with this
// prefix" from "we don't" — a small timing hardening.
func (s *PGStore) ValidateToken(ctx context.Context, plaintext string) (*Token, error) {
	if len(plaintext) < tokenPrefixLen {
		return nil, ErrTokenNotFound
	}
	prefix := plaintext[:tokenPrefixLen]
	rows, err := s.db.QueryContext(ctx,
		selectTokenColumns+`, token_hash FROM shard_tokens WHERE token_prefix = $1`,
		prefix,
	)
	if err != nil {
		return nil, fmt.Errorf("shards: validate token: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		t, hash, err := scanTokenWithHash(rows)
		if err != nil {
			return nil, fmt.Errorf("shards: validate token: scan: %w", err)
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) != nil {
			continue
		}
		if t.RevokedAt != nil {
			return nil, ErrTokenRevoked
		}
		return t, nil
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shards: validate token: rows: %w", err)
	}
	return nil, ErrTokenNotFound
}

// TouchToken updates last_used_at=NOW().
func (s *PGStore) TouchToken(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE shard_tokens SET last_used_at = NOW() WHERE id = $1`,
		id,
	)
	if err != nil {
		return fmt.Errorf("shards: touch token %s: %w", id, err)
	}
	return nil
}

// RevokeToken sets revoked_at=NOW(). Idempotent: revoking an already-
// revoked token returns nil without touching the column a second time
// so the revocation timestamp stays truthful.
func (s *PGStore) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shard_tokens SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`,
		id,
	)
	if err != nil {
		return fmt.Errorf("shards: revoke token %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already revoked OR doesn't exist. Look it up to disambiguate.
		var dummy string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM shard_tokens WHERE id = $1`, id).Scan(&dummy)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTokenNotFound
		}
		// Row exists and is already revoked — idempotent success.
	}
	return nil
}

// -----------------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------------

// selectShardColumns keeps the SELECT list consistent between GetShard,
// ListShards, and anywhere else that scans a shard row. Column order
// must match scanShard.
const selectShardColumns = `SELECT id, owner_id, name, description, persistence, visibility,
    scope_tag, tool_allowlist, system_prompt, model_preference, tier_preference,
    input_schema, output_schema, max_tokens, temperature, created_at, updated_at, disabled_at,
    console_access, console_panels, book_access, chat_enabled, api_enabled, session_max_age`

// scanner is the subset of sql.Row / sql.Rows that scanShard needs.
type scanner interface {
	Scan(dest ...any) error
}

// scanShard decodes a row selected by selectShardColumns into a Shard.
// Keeps the JSON-unmarshal and nullable-handling logic in one place.
func scanShard(sc scanner) (*Shard, error) {
	var (
		sh            Shard
		persistence   string
		visibility    string
		allowlistRaw  []byte
		inputSchema   []byte
		outputSchema  []byte
		disabledAt    sql.NullTime
		consolePanels pq.StringArray
		bookAccess    pq.StringArray
		sessionMaxAge sql.NullInt32
	)
	err := sc.Scan(
		&sh.ID, &sh.OwnerID, &sh.Name, &sh.Description,
		&persistence, &visibility, &sh.ScopeTag,
		&allowlistRaw, &sh.SystemPrompt,
		&sh.ModelPreference, &sh.TierPreference,
		&inputSchema, &outputSchema,
		&sh.MaxTokens, &sh.Temperature,
		&sh.CreatedAt, &sh.UpdatedAt, &disabledAt,
		&sh.ConsoleAccess, &consolePanels, &bookAccess,
		&sh.ChatEnabled, &sh.APIEnabled, &sessionMaxAge,
	)
	if err != nil {
		return nil, err
	}
	sh.Persistence = PersistenceMode(persistence)
	sh.Visibility = VisibilityMode(visibility)

	if len(allowlistRaw) > 0 {
		if err := json.Unmarshal(allowlistRaw, &sh.ToolAllowlist); err != nil {
			return nil, fmt.Errorf("shards: decode allowlist: %w", err)
		}
	}
	if sh.ToolAllowlist == nil {
		sh.ToolAllowlist = []string{}
	}
	if len(inputSchema) > 0 {
		sh.InputSchema = json.RawMessage(inputSchema)
	}
	if len(outputSchema) > 0 {
		sh.OutputSchema = json.RawMessage(outputSchema)
	}
	if disabledAt.Valid {
		t := disabledAt.Time
		sh.DisabledAt = &t
	}
	sh.ConsolePanels = []string(consolePanels)
	sh.BookAccess = []string(bookAccess)
	if sh.ConsolePanels == nil {
		sh.ConsolePanels = []string{}
	}
	if sh.BookAccess == nil {
		sh.BookAccess = []string{}
	}
	if sessionMaxAge.Valid {
		v := int(sessionMaxAge.Int32)
		sh.SessionMaxAge = &v
	}
	return &sh, nil
}

// selectTokenColumns is the base list for token SELECTs that don't need
// the hash. ValidateToken appends `, token_hash` onto this constant at
// query time; scanTokenWithHash expects the hash at the end.
const selectTokenColumns = `SELECT id, shard_id, owner_id, label, token_prefix,
    created_at, last_used_at, expires_at, revoked_at`

func scanToken(sc scanner) (*Token, error) {
	var (
		t          Token
		lastUsedAt sql.NullTime
		expiresAt  sql.NullTime
		revokedAt  sql.NullTime
	)
	err := sc.Scan(
		&t.ID, &t.ShardID, &t.OwnerID, &t.Label, &t.TokenPrefix,
		&t.CreatedAt, &lastUsedAt, &expiresAt, &revokedAt,
	)
	if err != nil {
		return nil, err
	}
	if lastUsedAt.Valid {
		v := lastUsedAt.Time
		t.LastUsedAt = &v
	}
	if expiresAt.Valid {
		v := expiresAt.Time
		t.ExpiresAt = &v
	}
	if revokedAt.Valid {
		v := revokedAt.Time
		t.RevokedAt = &v
	}
	return &t, nil
}

func scanTokenWithHash(sc scanner) (*Token, string, error) {
	var (
		t          Token
		lastUsedAt sql.NullTime
		expiresAt  sql.NullTime
		revokedAt  sql.NullTime
		hash       string
	)
	err := sc.Scan(
		&t.ID, &t.ShardID, &t.OwnerID, &t.Label, &t.TokenPrefix,
		&t.CreatedAt, &lastUsedAt, &expiresAt, &revokedAt,
		&hash,
	)
	if err != nil {
		return nil, "", err
	}
	if lastUsedAt.Valid {
		v := lastUsedAt.Time
		t.LastUsedAt = &v
	}
	if expiresAt.Valid {
		v := expiresAt.Time
		t.ExpiresAt = &v
	}
	if revokedAt.Valid {
		v := revokedAt.Time
		t.RevokedAt = &v
	}
	return &t, hash, nil
}

// nullableJSON returns the driver value for a JSONB column that may be
// NULL. json.RawMessage{} (empty) also becomes NULL so callers don't
// have to discriminate between "unset" and "set to empty."
func nullableJSON(r json.RawMessage) interface{} {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

// generateTokenPlaintext mints a fresh token string. Format:
// `shard_` + base64url(32 random bytes), no padding. Total length
// is 6 + 43 = 49 chars. The leading literal makes tokens self-
// identifying in logs and in the wild.
func generateTokenPlaintext() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("shards: rand: %w", err)
	}
	return TokenPlaintextPrefix + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}
