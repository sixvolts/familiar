package identity

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/testutil"
)

// Email lookups ignore case, and refuse to pick between two accounts
// whose addresses differ only in case (book invites resolve by email).
func TestGetByEmail_CaseInsensitiveAndUnambiguous(t *testing.T) {
	pool := testutil.PgTestPool(t)
	ctx := context.Background()
	r, err := NewResolver(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	n := time.Now().UnixNano()
	one := fmt.Sprintf("email-one-%d", n)
	addr := fmt.Sprintf("Bob-%d@Corp.example", n)
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO users (id, display_name, status, role, email) VALUES ($1, $1, 'approved', 'user', $2)`, one, addr); err != nil {
		t.Fatal(err)
	}
	u, err := r.GetByEmail(ctx, fmt.Sprintf("bob-%d@corp.example", n))
	if err != nil || u == nil || u.ID != one {
		t.Fatalf("lowercase lookup: %v, %v", u, err)
	}
	two := fmt.Sprintf("email-two-%d", n)
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO users (id, display_name, status, role, email) VALUES ($1, $1, 'approved', 'user', $2)`,
		two, fmt.Sprintf("bob-%d@corp.example", n)); err != nil {
		t.Fatal(err)
	}
	if u, err := r.GetByEmail(ctx, addr); err == nil {
		t.Errorf("an address two accounts share (by case) resolved to %v", u)
	}
}
