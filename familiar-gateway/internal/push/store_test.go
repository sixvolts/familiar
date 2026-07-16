package push

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/testutil"
)

// DB-gated (FAMILIAR_TEST_DSN). Pins the subscription store contract the
// subscribe endpoints + sender depend on: upsert-by-endpoint (re-home on
// re-subscribe), per-user listing, owner-scoped vs unscoped delete.
func TestStore_SubscriptionLifecycle(t *testing.T) {
	pool := testutil.PgTestPool(t)
	s := NewStore(pool)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userA := "push-a-" + suffix
	userB := "push-b-" + suffix
	for _, u := range []string{userA, userB} {
		if _, err := pool.ExecContext(ctx,
			`INSERT INTO users (id, display_name, status, role) VALUES ($1,$1,'approved','user') ON CONFLICT (id) DO NOTHING`, u); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	ep := "https://push.example.com/ep-" + suffix

	// Insert.
	if err := s.Upsert(ctx, userA, Subscription{Endpoint: ep, P256dh: "k1", Auth: "a1"}, "UA/1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	has, _ := s.HasAny(ctx, userA)
	if !has {
		t.Error("HasAny(userA) = false after upsert")
	}
	subs, err := s.ListForUser(ctx, userA)
	if err != nil || len(subs) != 1 || subs[0].Endpoint != ep || subs[0].P256dh != "k1" {
		t.Fatalf("list = %+v, err=%v", subs, err)
	}

	// Re-subscribe (same endpoint) updates keys + re-homes to userB.
	if err := s.Upsert(ctx, userB, Subscription{Endpoint: ep, P256dh: "k2", Auth: "a2"}, "UA/2"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if a, _ := s.ListForUser(ctx, userA); len(a) != 0 {
		t.Errorf("userA should no longer own the re-homed endpoint, got %d", len(a))
	}
	b, _ := s.ListForUser(ctx, userB)
	if len(b) != 1 || b[0].P256dh != "k2" {
		t.Errorf("userB list after re-home = %+v, want one with k2", b)
	}

	// Owner-scoped delete refuses a non-owner; unscoped (sender prune) wins.
	if err := s.DeleteByEndpoint(ctx, userA, ep); err != nil {
		t.Fatalf("scoped delete (wrong owner): %v", err)
	}
	if b, _ := s.ListForUser(ctx, userB); len(b) != 1 {
		t.Errorf("wrong-owner delete should not remove the row, got %d", len(b))
	}
	if err := s.DeleteByEndpoint(ctx, "", ep); err != nil {
		t.Fatalf("unscoped delete: %v", err)
	}
	if has, _ := s.HasAny(ctx, userB); has {
		t.Error("endpoint should be gone after unscoped delete")
	}
}
