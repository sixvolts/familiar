package identity

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testdsn"
	"github.com/familiar/gateway/internal/testutil"
)

// Only approved admins count: the last-admin guard used this count, and
// a disabled admin (who can't sign in) let the last usable one go.
func TestCountAdmins_ApprovedOnly(t *testing.T) {
	pool := privateIdentityPool(t, "identity_count_admins")
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role) VALUES
		  ('a1', 'a1', 'approved', 'admin'),
		  ('a2', 'a2', 'disabled', 'admin'),
		  ('a3', 'a3', 'pending',  'admin'),
		  ('u1', 'u1', 'approved', 'user')`); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.CountAdmins(ctx); err != nil || n != 1 {
		t.Errorf("CountAdmins = %d, %v; want the 1 approved admin", n, err)
	}
}

// privateIdentityPool is a migrated pool in a schema of its own, for
// tests that count or search the whole users table (other packages
// write users into the shared test schema concurrently).
func privateIdentityPool(t *testing.T, schema string) *db.Pool {
	t.Helper()
	dsn := os.Getenv(testutil.EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s not set", testutil.EnvDSN)
	}
	ctx := context.Background()
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	pool, err := db.Open(testdsn.Scoped(t, dsn, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// The lookup any signed-in user can call is a literal prefix search over
// approved users. The query went into ILIKE unescaped, so "%" (and "_")
// listed every account, disabled and denied ones included.
func TestSearchUsers_LiteralPrefixApprovedOnly(t *testing.T) {
	pool := privateIdentityPool(t, "identity_search_users")
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role, email) VALUES
		  ('bob',   'Bob',   'approved', 'user', 'bob@corp.example'),
		  ('bobby', 'Bobby', 'disabled', 'user', 'bobby@corp.example'),
		  ('a_b',   'A B',   'approved', 'user', NULL),
		  ('axb',   'Axb',   'approved', 'user', NULL)`); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(q string) []string {
		us, err := r.SearchUsers(ctx, q, 25)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range us {
			out = append(out, u.ID)
		}
		return out
	}
	for q, want := range map[string]string{
		"%":   "",
		"%%":  "",
		"_%":  "",
		"b":   "", // too short
		"bo":  "bob",
		"a_":  "a_b",
		"bob": "bob",
	} {
		if got := strings.Join(ids(q), ","); got != want {
			t.Errorf("search %q = [%s], want [%s]", q, got, want)
		}
	}
}

// Unlinking is scoped to the user it's done from, and linking never
// caches a mapping the database refused.
func TestIdentityLinks_ScopedUnlinkAndHonestLink(t *testing.T) {
	pool := privateIdentityPool(t, "identity_links")
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role) VALUES
		  ('alice', 'Alice', 'approved', 'user'),
		  ('bob',   'Bob',   'approved', 'user')`); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.LinkIdentity(ctx, "bob", "slack", "U123", "bob"); err != nil {
		t.Fatal(err)
	}
	// An admin still looking at alice (the link has since moved to bob)
	// unlinks it from her: bob keeps it.
	if err := r.UnlinkIdentity(ctx, "alice", "slack", "U123"); !errors.Is(err, ErrLinkNotFound) {
		t.Errorf("unlink from the wrong user: err %v, want ErrLinkNotFound", err)
	}
	if id, _, ok := r.ResolveWithStatus("slack", "U123"); !ok || id != "bob" {
		t.Errorf("after a stale unlink, U123 resolves to %q (ok=%v), want bob", id, ok)
	}

	// A row the cache doesn't know about (another admin, another process):
	// linking the same id elsewhere is refused and doesn't touch the cache.
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO identity_map (platform, platform_id, canonical_id) VALUES ('slack', 'U999', 'bob')`); err != nil {
		t.Fatal(err)
	}
	if err := r.LinkIdentity(ctx, "alice", "slack", "U999", "x"); !errors.Is(err, ErrDuplicateLink) {
		t.Errorf("linking an id the database already maps: err %v, want ErrDuplicateLink", err)
	}
	if id, _, ok := r.ResolveWithStatus("slack", "U999"); ok && id == "alice" {
		t.Error("the cache routes U999 to alice while the database says bob")
	}
}
