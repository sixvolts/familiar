package admin

// ShardPasskeyStore + the shardWebAuthnUser adapter implement
// SHARD-AUTH-SPEC Phase 1's "shard as login principal" plumbing.
// A row in shard_passkeys binds a WebAuthn credential to a shard
// (rather than a user). The store mirrors CredentialStore — same
// scan / decode shape, same credential_blob round-trip into the
// go-webauthn library — but keyed on shard_id.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/shards"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/lib/pq"
)

// ShardPasskeyStore persists shard_passkeys rows. Same posture as
// CredentialStore: random base64url credential ID is the primary
// access key; the marshaled webauthn.Credential blob lets login
// reconstruct the full struct without flattening every field
// into its own SQL column.
type ShardPasskeyStore struct {
	pool *db.Pool
}

// StoredShardPasskey is one shard_passkeys row, decoded.
type StoredShardPasskey struct {
	ID         string // UUID, the table's primary key
	ShardID    string
	CredID     string // base64url-encoded credential ID
	Credential webauthn.Credential
	SignCount  uint32
	Transports []string
	Label      string
	CreatedBy  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// IsActive reports whether this passkey can still be used to
// authenticate. Revoked passkeys are kept as audit; they just
// don't appear in allowCredentials and won't match on login.
func (p StoredShardPasskey) IsActive() bool { return p.RevokedAt == nil }

// NewShardPasskeyStore wraps the shared pool. Returns nil on a nil
// pool so the constructor mirrors the other in-admin stores; the
// ensure-* gates in the handlers turn that into a 503.
func NewShardPasskeyStore(pool *db.Pool) *ShardPasskeyStore {
	if pool == nil || pool.DB == nil {
		return nil
	}
	return &ShardPasskeyStore{pool: pool}
}

// Insert records a freshly minted shard credential. credID is the
// raw bytes the WebAuthn library produced; we base64url-encode it
// for the unique-by-credential-id index. createdBy is the user who
// enrolled the key (for audit — distinct from the shard's owner
// in the cross-user-admin case where an admin enrolls a key on
// behalf of another owner's shard).
func (s *ShardPasskeyStore) Insert(ctx context.Context, shardID, label, createdBy string, cred *webauthn.Credential) (string, error) {
	blob, err := json.Marshal(cred)
	if err != nil {
		return "", fmt.Errorf("admin: marshal shard credential: %w", err)
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	var id string
	err = s.pool.QueryRowContext(ctx, `
		INSERT INTO shard_passkeys
		    (shard_id, credential_id, credential_blob, public_key,
		     sign_count, transports, aaguid, label, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id::text`,
		shardID, cred.ID, blob, cred.PublicKey,
		int64(cred.Authenticator.SignCount),
		pq.StringArray(transports), cred.Authenticator.AAGUID, label, createdBy,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("admin: insert shard passkey: %w", err)
	}
	return id, nil
}

// ListByShard returns every passkey enrolled on the shard. By
// default skips revoked rows; pass includeRevoked=true for the
// admin audit view. Newest first so the UI list reads naturally.
func (s *ShardPasskeyStore) ListByShard(ctx context.Context, shardID string, includeRevoked bool) ([]StoredShardPasskey, error) {
	q := `
		SELECT id::text, shard_id, credential_id, credential_blob,
		       sign_count, COALESCE(transports, '{}'::text[]),
		       COALESCE(label, ''), created_by, created_at,
		       last_used_at, revoked_at
		  FROM shard_passkeys
		 WHERE shard_id = $1`
	if !includeRevoked {
		q += ` AND revoked_at IS NULL`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.pool.QueryContext(ctx, q, shardID)
	if err != nil {
		return nil, fmt.Errorf("admin: list shard passkeys: %w", err)
	}
	defer rows.Close()
	out := make([]StoredShardPasskey, 0)
	for rows.Next() {
		p, err := scanShardPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListAllActive returns every non-revoked passkey across every
// shard, used by loginBegin to populate the discoverable-credential
// allowList alongside webauthn_credentials. Phase 1's userless-
// resolution login path scans both tables to figure out which
// kind a presented credential is.
func (s *ShardPasskeyStore) ListAllActive(ctx context.Context) ([]StoredShardPasskey, error) {
	rows, err := s.pool.QueryContext(ctx, `
		SELECT id::text, shard_id, credential_id, credential_blob,
		       sign_count, COALESCE(transports, '{}'::text[]),
		       COALESCE(label, ''), created_by, created_at,
		       last_used_at, revoked_at
		  FROM shard_passkeys
		 WHERE revoked_at IS NULL
		 ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("admin: list active shard passkeys: %w", err)
	}
	defer rows.Close()
	out := make([]StoredShardPasskey, 0)
	for rows.Next() {
		p, err := scanShardPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetByRawID looks up a passkey by its raw (pre-encoding) credential
// ID. Same shape as CredentialStore.GetByRawID — the WebAuthn library
// hands the raw bytes back during finishLogin and we resolve to a
// shard from there.
func (s *ShardPasskeyStore) GetByRawID(ctx context.Context, rawID []byte) (*StoredShardPasskey, error) {
	row := s.pool.QueryRowContext(ctx, `
		SELECT id::text, shard_id, credential_id, credential_blob,
		       sign_count, COALESCE(transports, '{}'::text[]),
		       COALESCE(label, ''), created_by, created_at,
		       last_used_at, revoked_at
		  FROM shard_passkeys
		 WHERE credential_id = $1
		   AND revoked_at IS NULL`, rawID)
	p, err := scanShardPasskey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateSignCount bumps the replay-protection counter after a
// successful assertion AND stamps last_used_at. Mirrors
// CredentialStore.UpdateSignCount.
func (s *ShardPasskeyStore) UpdateSignCount(ctx context.Context, rawID []byte, newCount uint32) error {
	_, err := s.pool.ExecContext(ctx, `
		UPDATE shard_passkeys
		   SET sign_count = $1, last_used_at = NOW()
		 WHERE credential_id = $2`,
		int64(newCount), rawID)
	return err
}

// Revoke soft-deletes a passkey by stamping revoked_at. The row
// stays for audit but ListByShard's default filter and
// ListAllActive both skip it.
func (s *ShardPasskeyStore) Revoke(ctx context.Context, id string) error {
	res, err := s.pool.ExecContext(ctx, `
		UPDATE shard_passkeys SET revoked_at = NOW()
		 WHERE id = $1::uuid AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("admin: revoke shard passkey: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanShardPasskey(sc rowScanner) (StoredShardPasskey, error) {
	var (
		p          StoredShardPasskey
		blob       []byte
		signCount  int64
		credID     []byte
		transports pq.StringArray
		lastUsed   sql.NullTime
		revokedAt  sql.NullTime
	)
	if err := sc.Scan(&p.ID, &p.ShardID, &credID, &blob, &signCount,
		&transports, &p.Label, &p.CreatedBy, &p.CreatedAt,
		&lastUsed, &revokedAt,
	); err != nil {
		return p, err
	}
	if err := json.Unmarshal(blob, &p.Credential); err != nil {
		return p, fmt.Errorf("admin: unmarshal shard credential %s: %w", p.ID, err)
	}
	p.CredID = encodeCredentialID(credID)
	p.SignCount = uint32(signCount)
	p.Transports = []string(transports)
	if lastUsed.Valid {
		t := lastUsed.Time
		p.LastUsedAt = &t
	}
	if revokedAt.Valid {
		t := revokedAt.Time
		p.RevokedAt = &t
	}
	return p, nil
}

// AttachShardPasskeyStore wires the store onto the handler. Same
// pattern as the other Attach* hooks; nil tolerated so deploys
// without the SHARD-AUTH-SPEC migrations still boot.
func (h *Handler) AttachShardPasskeyStore(s *ShardPasskeyStore) { h.shardPasskeys = s }

// shardWebAuthnUser implements webauthn.User but binds the
// ceremony to a SHARD instead of a person. The user handle is the
// shard's ID, so the device's authenticator stores credentials
// against the shard rather than its owner — that's what makes a
// passkey resolve to a shard session at login time.
type shardWebAuthnUser struct {
	shard       *shards.Shard
	credentials []webauthn.Credential
}

func newShardWebAuthnUser(sh *shards.Shard, existing []StoredShardPasskey) *shardWebAuthnUser {
	creds := make([]webauthn.Credential, 0, len(existing))
	for _, p := range existing {
		creds = append(creds, p.Credential)
	}
	return &shardWebAuthnUser{shard: sh, credentials: creds}
}

func (u *shardWebAuthnUser) WebAuthnID() []byte                         { return []byte(u.shard.ID) }
func (u *shardWebAuthnUser) WebAuthnName() string                       { return u.shard.ID }
func (u *shardWebAuthnUser) WebAuthnDisplayName() string                { return u.shard.Name }
func (u *shardWebAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }
