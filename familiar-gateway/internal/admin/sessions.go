package admin

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/familiar/gateway/internal/db"
)

// SessionStore persists admin_sessions rows. Tokens are random 256-bit
// values, base64url-encoded, stored plaintext — they have the same
// sensitivity as a password hash lookup key, and rotate fast (default
// two-hour TTL).
type SessionStore struct {
	pool *db.Pool
}

// AdminSession is a validated session record. PrincipalType /
// PrincipalID make the table back both user and shard sessions
// (SHARD-AUTH-SPEC). UserID stays for backward compatibility — for
// a shard session it's the shard's owner so existing code that
// reads UserID still resolves to the canonical owner.
type AdminSession struct {
	Token         string
	UserID        string
	PrincipalType string // "user" | "shard"
	PrincipalID   string // user_id when type=user; shard_id when type=shard
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// PrincipalTypeUser / PrincipalTypeShard are the only legal values
// for AdminSession.PrincipalType. The DB enforces this via a CHECK
// constraint added in the shard_auth_phase1 migration.
const (
	PrincipalTypeUser  = "user"
	PrincipalTypeShard = "shard"
)

func NewSessionStore(pool *db.Pool) *SessionStore {
	return &SessionStore{pool: pool}
}

// Create mints a USER session — thin wrapper around CreatePrincipal
// kept for callers that pre-date the principal split. New callers
// should use CreatePrincipal so shard sessions go through the same
// path.
func (s *SessionStore) Create(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	return s.CreatePrincipal(ctx, PrincipalTypeUser, userID, userID, ttl)
}

// CreatePrincipal mints a session for either kind of principal.
// userID is the canonical owner — for a user session that's the
// principalID itself; for a shard session it's the shard's
// owner_id (so reads of admin_sessions.user_id still resolve to
// the right user). principalID is the shard or user id depending
// on principalType.
func (s *SessionStore) CreatePrincipal(ctx context.Context, principalType, principalID, userID string, ttl time.Duration) (string, error) {
	if principalType != PrincipalTypeUser && principalType != PrincipalTypeShard {
		return "", fmt.Errorf("admin: invalid principal_type %q", principalType)
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	expires := time.Now().Add(ttl)
	_, err = s.pool.ExecContext(ctx, `
		INSERT INTO admin_sessions (token, user_id, principal_type, principal_id, expires_at, ttl_seconds)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, token, userID, principalType, principalID, expires, int(ttl.Seconds()))
	if err != nil {
		return "", err
	}
	return token, nil
}

// ErrSessionInvalid covers both "token not in table" and "token expired".
var ErrSessionInvalid = errors.New("admin: session invalid or expired")

// Validate looks up a session token and rejects rows past their TTL.
// Returns the full AdminSession including principal_type / principal_id
// so the auth middleware can route user vs shard sessions without a
// second round-trip.
func (s *SessionStore) Validate(ctx context.Context, token string) (*AdminSession, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	var sess AdminSession
	var ttlSeconds sql.NullInt64
	err := s.pool.QueryRowContext(ctx, `
		SELECT token, user_id, principal_type, principal_id, created_at, expires_at, ttl_seconds
		FROM admin_sessions
		WHERE token = $1
	`, token).Scan(&sess.Token, &sess.UserID, &sess.PrincipalType, &sess.PrincipalID, &sess.CreatedAt, &sess.ExpiresAt, &ttlSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = s.Delete(ctx, token)
		return nil, ErrSessionInvalid
	}

	// Sliding renewal: the TTL is an IDLE window, not an absolute
	// deadline — a session in active use keeps living; only one idle
	// past its window dies. Renew when less than half the window
	// remains so the UPDATE fires at most ~once per half-window, not
	// on every request. Legacy rows (NULL ttl_seconds) derive the
	// window from mint-time bounds and backfill it on first renewal.
	// Renewal failure is non-fatal — the session is still valid now.
	ttl := time.Duration(ttlSeconds.Int64) * time.Second
	if !ttlSeconds.Valid || ttl <= 0 {
		ttl = sess.ExpiresAt.Sub(sess.CreatedAt)
	}
	if ttl > 0 && time.Until(sess.ExpiresAt) < ttl/2 {
		newExpires := time.Now().Add(ttl)
		if _, err := s.pool.ExecContext(ctx, `
			UPDATE admin_sessions
			   SET expires_at = $2,
			       ttl_seconds = COALESCE(ttl_seconds, $3)
			 WHERE token = $1
		`, token, newExpires, int(ttl.Seconds())); err == nil {
			sess.ExpiresAt = newExpires
		}
	}
	return &sess, nil
}

// Delete removes a session (logout).
func (s *SessionStore) Delete(ctx context.Context, token string) error {
	_, err := s.pool.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token = $1`, token)
	return err
}

// DeleteByUser revokes every active session owned by userID. Called
// when an admin flips a user to a non-approved status — without this
// hook a disabled user's existing session cookies stay valid until
// their natural TTL elapses (default 2 hours), letting them keep
// using surfaces like /api/chat that only consult Validate's TTL
// check. Idempotent: returns nil when the user had no sessions.
//
// Includes shard sessions owned by the user — when a user is
// disabled their shard kiosks should also stop authenticating.
func (s *SessionStore) DeleteByUser(ctx context.Context, userID string) error {
	if userID == "" {
		return nil
	}
	_, err := s.pool.ExecContext(ctx, `DELETE FROM admin_sessions WHERE user_id = $1`, userID)
	return err
}

// Cleanup drops every expired session in one pass. Safe to run on a
// timer — bounded by the size of the table.
func (s *SessionStore) Cleanup(ctx context.Context) error {
	_, err := s.pool.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < NOW()`)
	return err
}

// StartGC runs Cleanup on a ticker until ctx is cancelled. Without it,
// admin_sessions grows forever — every login and sliding-window renewal
// leaves an eventually-expired row behind that nothing else removes.
// One sweep at start clears the backlog a long-running deploy has
// already accumulated. Returns immediately; the goroutine exits on
// ctx.Done().
func (s *SessionStore) StartGC(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		if err := s.Cleanup(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[admin] session GC sweep: %v", err)
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.Cleanup(ctx); err != nil && ctx.Err() == nil {
					log.Printf("[admin] session GC sweep: %v", err)
				}
			}
		}
	}()
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
