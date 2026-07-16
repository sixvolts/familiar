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
