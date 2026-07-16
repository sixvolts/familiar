package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/go-webauthn/webauthn/webauthn"
)

// CredentialStore persists WebAuthn credentials in the shared pool.
// Each row binds a credential ID (base64url) to the marshaled
// webauthn.Credential blob and the canonical user_id that owns it.
type CredentialStore struct {
	pool *db.Pool
}

// StoredCredential is a database row decoded into memory.
type StoredCredential struct {
	ID     string
	UserID string
	// UserHandle is the WebAuthn user handle baked into the
	// authenticator at registration — the value the device echoes
	// back on every assertion. It is held separately from UserID so
	// a canonical-id rename (e.g. OWNER-MIGRATION) never invalidates
	// the passkey. For credentials registered normally it equals the
	// UserID at creation time.
	UserHandle  string
	DisplayName string
	Credential  webauthn.Credential
	SignCount   uint32
	CreatedAt   time.Time
	LastUsed    *time.Time
}

// credentialColumns is the shared SELECT list for credential rows.
// COALESCE keeps a pre-migration row (handle column not yet
// backfilled) usable by falling back to user_id.
const credentialColumns = `id, credential_blob, user_id,
	COALESCE(webauthn_user_handle, user_id), COALESCE(display_name, ''),
	sign_count, created_at, last_used`

func NewCredentialStore(pool *db.Pool) *CredentialStore {
	return &CredentialStore{pool: pool}
}

// Count returns the total number of credentials stored. A count of
// zero is the signal that first-time registration should be allowed
// without an authenticated session.
func (s *CredentialStore) Count(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM webauthn_credentials`).Scan(&n)
	return n, err
}

// CountsByUser returns the number of registered passkeys per canonical
// user, as a user_id→count map. The admin Users panel uses it to show
// who has actually enrolled a passkey vs. who's still invited-but-
// passkeyless (and therefore needs an enrollment link). One grouped
// query; the table is tiny.
func (s *CredentialStore) CountsByUser(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.QueryContext(ctx,
		`SELECT user_id, COUNT(*) FROM webauthn_credentials GROUP BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var uid string
		var n int
		if err := rows.Scan(&uid, &n); err != nil {
			return nil, err
		}
		out[uid] = n
	}
	return out, rows.Err()
}

// Insert stores a freshly minted credential. id is the base64url
// credential ID; the full Credential struct is marshaled into the
// credential_blob column so Get can reconstruct it without needing
// every field to have its own SQL column.
func (s *CredentialStore) Insert(ctx context.Context, userID, displayName string, cred *webauthn.Credential) error {
	blob, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("admin: marshal credential: %w", err)
	}
	id := encodeCredentialID(cred.ID)
	// webauthn_user_handle is set to userID at creation — that IS the
	// handle baked into the authenticator. It lives in its own column
	// so a later user_id rename can't orphan the passkey.
	_, err = s.pool.ExecContext(ctx, `
		INSERT INTO webauthn_credentials
			(id, credential_blob, user_id, webauthn_user_handle, display_name, sign_count)
		VALUES ($1, $2, $3, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, id, blob, userID, displayName, int64(cred.Authenticator.SignCount))
	return err
}

// ListAll returns every stored credential. Used by BeginLogin when we
// want the browser to present the full set of possible authenticators.
func (s *CredentialStore) ListAll(ctx context.Context) ([]StoredCredential, error) {
	rows, err := s.pool.QueryContext(ctx, `
		SELECT `+credentialColumns+`
		FROM webauthn_credentials
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredCredential
	for rows.Next() {
		sc, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ListByUser returns every credential owned by one canonical user.
func (s *CredentialStore) ListByUser(ctx context.Context, userID string) ([]StoredCredential, error) {
	rows, err := s.pool.QueryContext(ctx, `
		SELECT `+credentialColumns+`
		FROM webauthn_credentials
		WHERE user_id = $1
		ORDER BY created_at ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredCredential
	for rows.Next() {
		sc, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ErrNotFound is returned by Get when no credential matches the lookup.
var ErrNotFound = errors.New("admin: credential not found")

// GetByRawID looks up a credential by its raw (pre-encoding) ID. The
// raw ID is what the library hands back during Finish calls.
func (s *CredentialStore) GetByRawID(ctx context.Context, rawID []byte) (*StoredCredential, error) {
	return s.getByID(ctx, encodeCredentialID(rawID))
}

func (s *CredentialStore) getByID(ctx context.Context, id string) (*StoredCredential, error) {
	row := s.pool.QueryRowContext(ctx, `
		SELECT `+credentialColumns+`
		FROM webauthn_credentials
		WHERE id = $1
	`, id)
	sc, err := scanCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// UpdateSignCount bumps the replay-protection counter after a
// successful assertion. Called from the login finish handler.
func (s *CredentialStore) UpdateSignCount(ctx context.Context, rawID []byte, newCount uint32) error {
	_, err := s.pool.ExecContext(ctx, `
		UPDATE webauthn_credentials
		SET sign_count = $1, last_used = NOW()
		WHERE id = $2
	`, int64(newCount), encodeCredentialID(rawID))
	return err
}

// Delete removes a credential by its base64url ID. Used when an admin
// revokes a key via the dashboard.
func (s *CredentialStore) Delete(ctx context.Context, id string) error {
	_, err := s.pool.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE id = $1`, id)
	return err
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanCredential can serve both.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCredential(r rowScanner) (StoredCredential, error) {
	var (
		sc        StoredCredential
		blob      []byte
		signCount int64
		lastUsed  sql.NullTime
	)
	if err := r.Scan(&sc.ID, &blob, &sc.UserID, &sc.UserHandle, &sc.DisplayName, &signCount, &sc.CreatedAt, &lastUsed); err != nil {
		return sc, err
	}
	if err := json.Unmarshal(blob, &sc.Credential); err != nil {
		return sc, fmt.Errorf("admin: unmarshal credential %s: %w", sc.ID, err)
	}
	sc.SignCount = uint32(signCount)
	if lastUsed.Valid {
		t := lastUsed.Time
		sc.LastUsed = &t
	}
	return sc, nil
}
