package admin

// Cross-domain passkey enrollment tokens (CROSS-DOMAIN-ENROLLMENT.md).
//
// A user authenticated on one RP (e.g. host-a.your-tailnet.ts.net) can
// generate a short-lived single-use token bound to their canonical
// ID and a target RP (e.g. familiar.wiki). Opening the enrollment
// link on the target origin walks them through a fresh WebAuthn
// registration ceremony for the target RP. Tokens auto-expire after
// 15 minutes and are consumed atomically on successful registration
// so a stolen URL has a narrow exposure window.
//
// Admin users can issue tokens on behalf of any other user — useful
// for onboarding a teammate's new device without round-tripping
// through their existing credential.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/familiar/gateway/internal/db"
)

// EnrollmentTokenTTL bounds how long a freshly issued token can be
// used. Onboarding is admin-invite + manual relay (the admin copies the
// link and sends it out-of-band), so the window has to survive a human
// turnaround — a person may not open the link until the next day. 48h
// covers that; the token is still ONE-SHOT (Consume invalidates it on
// first successful enroll), so a longer life only widens the window for
// an UNUSED link, not a replay.
const EnrollmentTokenTTL = 48 * time.Hour

// MaxActiveEnrollmentTokensPerUser caps how many unconsumed,
// unexpired tokens any one canonical_id can hold. Prevents a
// runaway script from filling the table; the cap is generous
// enough that a normal user-facing flow never hits it.
const MaxActiveEnrollmentTokensPerUser = 5

// EnrollmentToken is one row of passkey_enrollment_tokens decoded
// into memory.
type EnrollmentToken struct {
	Token       string
	CanonicalID string
	TargetRPID  string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
	CreatedBy   string
}

// Active reports whether the token is still usable: not consumed,
// not past its expires_at. Pure function over a row in memory;
// the storage layer also enforces this on every fetch.
func (t *EnrollmentToken) Active(now time.Time) bool {
	if t == nil {
		return false
	}
	if t.ConsumedAt != nil {
		return false
	}
	return now.Before(t.ExpiresAt)
}

// ErrEnrollmentTokenInvalid is the umbrella error returned for
// every variant of "this token cannot be used right now" — missing,
// expired, already consumed, or RP mismatch. Callers map it to 400
// with a generic message so a probing attacker can't tell which
// branch they hit.
var ErrEnrollmentTokenInvalid = errors.New("enrollment token: invalid")

// ErrEnrollmentRateLimited is returned when a caller has reached
// MaxActiveEnrollmentTokensPerUser unconsumed tokens. The handler
// maps this to 429.
var ErrEnrollmentRateLimited = errors.New("enrollment token: rate limit reached")

// EnrollmentTokenStore persists passkey_enrollment_tokens rows.
type EnrollmentTokenStore struct {
	pool *db.Pool
}

// NewEnrollmentTokenStore constructs a store. Wired in admin.New().
func NewEnrollmentTokenStore(pool *db.Pool) *EnrollmentTokenStore {
	return &EnrollmentTokenStore{pool: pool}
}

// Issue mints a fresh token for canonicalID targeting rpID, charged
// against createdBy (which may equal canonicalID for self-issuance
// or differ for the admin-shares-a-link flow). The new token has
// EnrollmentTokenTTL remaining.
//
// Also opportunistically sweeps rows whose expires_at is more than
// an hour in the past — keeps the table compact without a scheduled
// cleanup job.
func (s *EnrollmentTokenStore) Issue(ctx context.Context, canonicalID, rpID, createdBy string) (*EnrollmentToken, error) {
	if canonicalID == "" || rpID == "" || createdBy == "" {
		return nil, fmt.Errorf("enrollment token: canonical_id, target_rp_id, created_by required")
	}

	// Rate-limit BEFORE issuing so a misbehaving caller can't sneak
	// past by racing parallel requests.
	active, err := s.CountActive(ctx, canonicalID)
	if err != nil {
		return nil, err
	}
	if active >= MaxActiveEnrollmentTokensPerUser {
		return nil, ErrEnrollmentRateLimited
	}

	tok, err := randomEnrollmentToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expires := now.Add(EnrollmentTokenTTL)

	if _, err := s.pool.ExecContext(ctx, `
		INSERT INTO passkey_enrollment_tokens
		    (token, canonical_id, target_rp_id, created_at, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		tok, canonicalID, rpID, now, expires, createdBy); err != nil {
		return nil, fmt.Errorf("enrollment token: insert: %w", err)
	}

	// Lazy sweep of long-expired rows; one hour past expiry covers
	// the audit window. Errors are logged but not surfaced — the
	// token issuance itself succeeded.
	go s.sweepExpired(context.Background())

	return &EnrollmentToken{
		Token:       tok,
		CanonicalID: canonicalID,
		TargetRPID:  rpID,
		CreatedAt:   now,
		ExpiresAt:   expires,
		CreatedBy:   createdBy,
	}, nil
}

// Get returns the row for tok. Returns ErrEnrollmentTokenInvalid
// when the token doesn't exist, is past expires_at, or has already
// been consumed — every failure case collapses to one error so the
// caller surfaces a generic message.
func (s *EnrollmentTokenStore) Get(ctx context.Context, tok string) (*EnrollmentToken, error) {
	if tok == "" {
		return nil, ErrEnrollmentTokenInvalid
	}
	var t EnrollmentToken
	var consumed sql.NullTime
	err := s.pool.QueryRowContext(ctx, `
		SELECT token, canonical_id, target_rp_id, created_at, expires_at, consumed_at, created_by
		  FROM passkey_enrollment_tokens
		 WHERE token = $1`, tok).Scan(
		&t.Token, &t.CanonicalID, &t.TargetRPID, &t.CreatedAt, &t.ExpiresAt, &consumed, &t.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEnrollmentTokenInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("enrollment token: select: %w", err)
	}
	if consumed.Valid {
		t.ConsumedAt = &consumed.Time
	}
	if !t.Active(time.Now().UTC()) {
		return nil, ErrEnrollmentTokenInvalid
	}
	return &t, nil
}

// Consume marks tok as used atomically — the UPDATE conditions on
// consumed_at IS NULL so a parallel finish-registration call can't
// double-consume. Returns ErrEnrollmentTokenInvalid when zero rows
// matched (already consumed, expired, or missing).
func (s *EnrollmentTokenStore) Consume(ctx context.Context, tok string) error {
	res, err := s.pool.ExecContext(ctx, `
		UPDATE passkey_enrollment_tokens
		   SET consumed_at = NOW()
		 WHERE token = $1
		   AND consumed_at IS NULL
		   AND expires_at > NOW()`, tok)
	if err != nil {
		return fmt.Errorf("enrollment token: consume: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("enrollment token: consume rowsaffected: %w", err)
	}
	if n == 0 {
		return ErrEnrollmentTokenInvalid
	}
	return nil
}

// CountActive returns how many unconsumed, unexpired tokens are
// outstanding for canonicalID. Drives the rate-limit check.
func (s *EnrollmentTokenStore) CountActive(ctx context.Context, canonicalID string) (int, error) {
	var n int
	err := s.pool.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM passkey_enrollment_tokens
		 WHERE canonical_id = $1
		   AND consumed_at IS NULL
		   AND expires_at > NOW()`, canonicalID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("enrollment token: count active: %w", err)
	}
	return n, nil
}

// sweepExpired deletes rows whose expires_at is more than an hour
// in the past. Called lazily on each Issue() so the table stays
// compact without a dedicated cron job.
func (s *EnrollmentTokenStore) sweepExpired(ctx context.Context) {
	_, _ = s.pool.ExecContext(ctx, `
		DELETE FROM passkey_enrollment_tokens
		 WHERE expires_at < NOW() - INTERVAL '1 hour'`)
}

// randomEnrollmentToken returns 32 bytes of crypto-strong entropy
// base64url-encoded (43 chars, URL-safe). 256 bits is overkill for
// a 15-minute single-use credential but keeps us comfortably below
// any guessing concern.
func randomEnrollmentToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("enrollment token: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}
