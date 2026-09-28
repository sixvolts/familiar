package admin

// SessionStore integration tests (FAMILIAR_TEST_DSN-gated, like the
// other DB-backed suites). Sessions are the load-bearing auth
// primitive — every console request and /api/chat turn goes through
// Validate — so the TTL/revocation semantics deserve direct pins,
// not just the E2E suite's behavioral shadows:
//
//   - expired tokens are rejected AND reaped on touch
//   - DeleteByUser revokes exactly that user's sessions (the
//     disable-user hook)
//   - Cleanup reaps expired rows only
//   - principal columns round-trip for both user and shard sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/testutil"
)

func sessionStoreForTest(t *testing.T) *SessionStore {
	t.Helper()
	pool := testutil.PgTestPool(t)
	return NewSessionStore(pool)
}

func TestSession_CreateValidateRoundTrip(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()

	token, err := s.Create(ctx, "sess-user-1", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(token) < 40 {
		t.Errorf("token suspiciously short: %d chars", len(token))
	}

	sess, err := s.Validate(ctx, token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sess.UserID != "sess-user-1" {
		t.Errorf("UserID = %q", sess.UserID)
	}
	if sess.PrincipalType != PrincipalTypeUser || sess.PrincipalID != "sess-user-1" {
		t.Errorf("principal = %q/%q, want user/sess-user-1", sess.PrincipalType, sess.PrincipalID)
	}
	if !sess.ExpiresAt.After(time.Now()) {
		t.Errorf("ExpiresAt %v not in the future", sess.ExpiresAt)
	}
}

func TestSession_ValidateRejectsUnknownAndEmpty(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()
	for _, token := range []string{"", "definitely-not-a-token"} {
		if _, err := s.Validate(ctx, token); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("Validate(%q) err = %v, want ErrSessionInvalid", token, err)
		}
	}
}

func TestSession_ExpiredTokenIsRejectedAndReaped(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()

	token, err := s.Create(ctx, "sess-user-exp", -time.Minute) // born expired
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Validate(ctx, token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expired Validate err = %v, want ErrSessionInvalid", err)
	}

	// Validate-on-expired deletes the row — prove it's gone rather
	// than merely still-expired.
	var n int
	if err := s.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admin_sessions WHERE token = $1`, token).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expired row survived Validate, count = %d", n)
	}
}

func TestSession_DeleteByUserIsScoped(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()

	doomedA, _ := s.Create(ctx, "sess-doomed", time.Hour)
	doomedB, _ := s.Create(ctx, "sess-doomed", time.Hour)
	survivor, _ := s.Create(ctx, "sess-bystander", time.Hour)

	if err := s.DeleteByUser(ctx, "sess-doomed"); err != nil {
		t.Fatalf("DeleteByUser: %v", err)
	}
	for _, token := range []string{doomedA, doomedB} {
		if _, err := s.Validate(ctx, token); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("doomed session still validates")
		}
	}
	if _, err := s.Validate(ctx, survivor); err != nil {
		t.Errorf("bystander session was revoked: %v", err)
	}

	// Idempotent on a user with nothing left (and on empty user id).
	if err := s.DeleteByUser(ctx, "sess-doomed"); err != nil {
		t.Errorf("second DeleteByUser: %v", err)
	}
	if err := s.DeleteByUser(ctx, ""); err != nil {
		t.Errorf("DeleteByUser(\"\"): %v", err)
	}
}

func TestSession_CleanupReapsExpiredOnly(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()

	dead, _ := s.Create(ctx, "sess-cleanup", -time.Minute)
	alive, _ := s.Create(ctx, "sess-cleanup", time.Hour)

	if err := s.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	var n int
	if err := s.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admin_sessions WHERE token = $1`, dead).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Error("Cleanup left the expired row")
	}
	if _, err := s.Validate(ctx, alive); err != nil {
		t.Errorf("Cleanup reaped a live session: %v", err)
	}
}

func TestSession_ShardPrincipalRoundTrip(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()

	token, err := s.CreatePrincipal(ctx, PrincipalTypeShard, "shard-kiosk", "sess-owner", time.Hour)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	sess, err := s.Validate(ctx, token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sess.PrincipalType != PrincipalTypeShard || sess.PrincipalID != "shard-kiosk" {
		t.Errorf("principal = %q/%q, want shard/shard-kiosk", sess.PrincipalType, sess.PrincipalID)
	}
	// UserID carries the OWNER so legacy user_id readers still
	// resolve to a canonical human.
	if sess.UserID != "sess-owner" {
		t.Errorf("UserID = %q, want sess-owner", sess.UserID)
	}
}

func TestSession_CreatePrincipalRejectsUnknownType(t *testing.T) {
	s := sessionStoreForTest(t)
	if _, err := s.CreatePrincipal(context.Background(), "robot", "r1", "u1", time.Hour); err == nil {
		t.Fatal("CreatePrincipal accepted an invalid principal_type")
	}
}

func TestSession_SlidingRenewal(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()

	token, err := s.Create(ctx, "sess-slide-1", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A fresh session (more than half the window left) must NOT renew
	// — otherwise every request writes a row.
	first, err := s.Validate(ctx, token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	second, err := s.Validate(ctx, token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Errorf("fresh session renewed: %v → %v", first.ExpiresAt, second.ExpiresAt)
	}

	// Push the session into the renewal half (10 min left of a 1h
	// window) — the next Validate slides it back out to a full hour.
	if _, err := s.pool.ExecContext(ctx, `
		UPDATE admin_sessions SET expires_at = NOW() + interval '10 minutes'
		 WHERE token = $1`, token); err != nil {
		t.Fatalf("age session: %v", err)
	}
	renewed, err := s.Validate(ctx, token)
	if err != nil {
		t.Fatalf("Validate (renewal): %v", err)
	}
	if remaining := time.Until(renewed.ExpiresAt); remaining < 50*time.Minute {
		t.Errorf("session not renewed: %v remaining", remaining)
	}

	// Legacy row (NULL ttl_seconds): the window derives from the
	// mint-time bounds and is backfilled on first renewal.
	legacy, err := s.Create(ctx, "sess-slide-legacy", time.Hour)
	if err != nil {
		t.Fatalf("Create legacy: %v", err)
	}
	if _, err := s.pool.ExecContext(ctx, `
		UPDATE admin_sessions
		   SET ttl_seconds = NULL,
		       created_at = NOW() - interval '50 minutes',
		       expires_at = NOW() + interval '10 minutes'
		 WHERE token = $1`, legacy); err != nil {
		t.Fatalf("age legacy session: %v", err)
	}
	lr, err := s.Validate(ctx, legacy)
	if err != nil {
		t.Fatalf("Validate legacy: %v", err)
	}
	if remaining := time.Until(lr.ExpiresAt); remaining < 50*time.Minute {
		t.Errorf("legacy session not renewed: %v remaining", remaining)
	}
	var backfilled int
	if err := s.pool.QueryRowContext(ctx,
		`SELECT ttl_seconds FROM admin_sessions WHERE token = $1`, legacy,
	).Scan(&backfilled); err != nil {
		t.Fatalf("read backfilled ttl: %v", err)
	}
	if backfilled < 3500 || backfilled > 3700 {
		t.Errorf("backfilled ttl_seconds = %d, want ~3600", backfilled)
	}
}

// Deleting or revoking a passkey must end exactly the sessions it
// minted: sliding renewal otherwise keeps a stolen session alive.
func TestSession_DeleteByCredentialEndsOnlyThatKeysSessions(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()
	stolen, err := s.CreateBound(ctx, PrincipalTypeUser, "sess-cred-user", "sess-cred-user", "cred-stolen", time.Hour, 0)
	if err != nil {
		t.Fatalf("CreateBound: %v", err)
	}
	other, err := s.CreateBound(ctx, PrincipalTypeUser, "sess-cred-user", "sess-cred-user", "cred-other", time.Hour, 0)
	if err != nil {
		t.Fatalf("CreateBound: %v", err)
	}
	if err := s.DeleteByCredential(ctx, "cred-stolen"); err != nil {
		t.Fatalf("DeleteByCredential: %v", err)
	}
	if _, err := s.Validate(ctx, stolen); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("session minted by the deleted key still validates (err=%v)", err)
	}
	if _, err := s.Validate(ctx, other); err != nil {
		t.Errorf("session from a different key was ended too: %v", err)
	}
}

// Deleting or disabling a shard ends its kiosk sessions and no others,
// so a recreated shard with the same id starts clean.
func TestSession_DeleteByShardEndsOnlyThatShard(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()
	kiosk, err := s.CreateBound(ctx, PrincipalTypeShard, "sess-kitchen", "sess-shard-owner", "cred-k", time.Hour, 0)
	if err != nil {
		t.Fatalf("CreateBound: %v", err)
	}
	garage, err := s.CreateBound(ctx, PrincipalTypeShard, "sess-garage", "sess-shard-owner", "cred-g", time.Hour, 0)
	if err != nil {
		t.Fatalf("CreateBound: %v", err)
	}
	owner, err := s.Create(ctx, "sess-shard-owner", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.DeleteByShard(ctx, "sess-kitchen"); err != nil {
		t.Fatalf("DeleteByShard: %v", err)
	}
	if _, err := s.Validate(ctx, kiosk); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("deleted shard's session still validates (err=%v)", err)
	}
	for name, tok := range map[string]string{"other shard": garage, "owner": owner} {
		if _, err := s.Validate(ctx, tok); err != nil {
			t.Errorf("%s session was ended too: %v", name, err)
		}
	}
}

// A session in constant use still ends at its absolute deadline. Sliding
// renewal used to make the TTL an idle timeout only: an open tab (the
// SPA probes every 90s) or a stolen cookie in use lived forever, and a
// shard's session_max_age never forced the re-auth it documents.
func TestSession_RenewalStopsAtAbsoluteLifetime(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()
	token, err := s.CreateBound(ctx, PrincipalTypeShard, "sess-abs-kiosk", "sess-abs-owner", "", time.Hour, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var absolute time.Time
	if err := s.pool.QueryRowContext(ctx,
		`SELECT absolute_expires_at FROM admin_sessions WHERE token = $1`, token).Scan(&absolute); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Validate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ExpiresAt.After(absolute) {
		t.Errorf("a new session expires at %v, after its lifetime %v", sess.ExpiresAt, absolute)
	}

	// Due for renewal (under half the hour left): renewal stops at the deadline.
	if _, err := s.pool.ExecContext(ctx,
		`UPDATE admin_sessions SET expires_at = NOW() + interval '5 minutes' WHERE token = $1`, token); err != nil {
		t.Fatal(err)
	}
	sess, err = s.Validate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ExpiresAt.After(absolute.Add(time.Second)) {
		t.Errorf("renewal carried the session to %v, past its lifetime %v", sess.ExpiresAt, absolute)
	}

	// Past the deadline it's over, whatever expires_at says.
	if _, err := s.pool.ExecContext(ctx,
		`UPDATE admin_sessions SET absolute_expires_at = NOW() - interval '1 second', expires_at = NOW() + interval '1 hour' WHERE token = $1`, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Validate(ctx, token); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("a session past its absolute lifetime validated (err = %v)", err)
	}
}

// Sessions from before absolute_expires_at existed live at most the
// store's maximum from their creation.
func TestSession_LegacyRowsGetTheMaximumLifetime(t *testing.T) {
	s := sessionStoreForTest(t)
	ctx := context.Background()
	old, _ := s.Create(ctx, "sess-legacy", time.Hour)
	young, _ := s.Create(ctx, "sess-legacy", time.Hour)
	if _, err := s.pool.ExecContext(ctx, `
		UPDATE admin_sessions SET absolute_expires_at = NULL, expires_at = NOW() + interval '30 minutes',
		       created_at = CASE WHEN token = $1 THEN NOW() - interval '8 days' ELSE NOW() - interval '6 days' END
		 WHERE token IN ($1, $2)`, old, young); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Validate(ctx, old); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("an 8-day-old session validated (err = %v)", err)
	}
	if _, err := s.Validate(ctx, young); err != nil {
		t.Errorf("a 6-day-old session was refused: %v", err)
	}
}

// A lifetime longer than the store's maximum is cut to it.
func TestSession_LifetimeCappedAtMaximum(t *testing.T) {
	s := sessionStoreForTest(t)
	s.maxLifetime = time.Hour
	ctx := context.Background()
	token, err := s.CreateBound(ctx, PrincipalTypeUser, "sess-cap", "sess-cap", "", 2*time.Hour, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var left time.Duration
	var secs float64
	if err := s.pool.QueryRowContext(ctx,
		`SELECT EXTRACT(EPOCH FROM absolute_expires_at - NOW()) FROM admin_sessions WHERE token = $1`, token).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	left = time.Duration(secs * float64(time.Second))
	if left > time.Hour+time.Minute {
		t.Errorf("lifetime %v, want at most the store's 1h maximum", left)
	}
}
