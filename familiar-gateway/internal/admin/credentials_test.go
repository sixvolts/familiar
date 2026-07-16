package admin

import (
	"context"
	"testing"

	"github.com/familiar/gateway/internal/testutil"
	"github.com/go-webauthn/webauthn/webauthn"
)

// CountsByUser backs the admin Users panel's "enrolled vs invited-but-
// no-passkey" flag. Pin that it counts per canonical user and omits
// users with no credential entirely (so a 0 read = needs enrollment).
// DB-gated like the other admin integration suites.
func TestCredentialStore_CountsByUser(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "webauthn_credentials")
	s := NewCredentialStore(pool)
	ctx := context.Background()

	ins := func(userID, credID string) {
		t.Helper()
		if err := s.Insert(ctx, userID, "k", &webauthn.Credential{ID: []byte(credID)}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins("alice", "cred-a1")
	ins("alice", "cred-a2")
	ins("bob", "cred-b1")

	counts, err := s.CountsByUser(ctx)
	if err != nil {
		t.Fatalf("CountsByUser: %v", err)
	}
	if counts["alice"] != 2 {
		t.Errorf("alice = %d, want 2", counts["alice"])
	}
	if counts["bob"] != 1 {
		t.Errorf("bob = %d, want 1", counts["bob"])
	}
	// A user with no passkey isn't in the map → reads as 0 (needs enroll).
	if _, ok := counts["carol"]; ok {
		t.Errorf("carol should be absent from the counts map")
	}
	if counts["carol"] != 0 {
		t.Errorf("absent user must read as 0, got %d", counts["carol"])
	}
}
