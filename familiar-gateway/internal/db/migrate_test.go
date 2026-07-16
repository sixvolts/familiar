package db

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

// freshSchema is the scratch schema TestMigrateFreshDatabase builds in.
// Pinning search_path to it simulates a completely empty database even
// when the test DB's public schema already carries partial state from
// other tests or earlier (broken) bootstrap attempts. public stays on
// the path only so extension types (vector) resolve.
const freshSchema = "migrate_fresh_test"

// migrationTables is every relation Migrate must produce on a fresh
// database. A missing entry here means a service (auth, chat, wiki, …)
// would 500 on first boot — exactly the failure mode the memories
// bootstrap regression caused.
var migrationTables = []string{
	"sessions",
	"user_profiles",
	"identity_map",
	"memories",
	"users",
	"webauthn_credentials",
	"admin_sessions",
	"relationships",
	"memory_versions",
	"shards",
	"shard_tokens",
	"conversations",
	"messages",
	"notes",
	"books",
	"book_members",
	"wiki_pages",
	"wiki_revisions",
	"user_page_prefs",
	"wiki_page_links",
	"wiki_page_entities",
	"book_audit",
	"shard_passkeys",
	"passkey_enrollment_tokens",
	"wiki_page_shares",
	"instance_settings",
	"scheduled_actions",
	"scheduled_action_runs",
	"skill_packages",
	"shard_skills",
	"chat_folders",
}

func TestMigrateFreshDatabase(t *testing.T) {
	dsn := os.Getenv("FAMILIAR_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping: FAMILIAR_TEST_DSN not set")
	}
	ctx := context.Background()

	admin, err := Open(dsn)
	if err != nil {
		t.Fatalf("db.Open (admin): %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+freshSchema+" CASCADE"); err != nil {
		t.Fatalf("drop stale schema: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+freshSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+freshSchema+" CASCADE")
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scoped := dsn + sep + "options=" + url.QueryEscape("-csearch_path="+freshSchema+",public")
	pool, err := Open(scoped)
	if err != nil {
		t.Fatalf("db.Open (scoped): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate on fresh database: %v", err)
	}
	// Migrations run unconditionally on every gateway boot, so the
	// second pass over an already-migrated schema must be a no-op.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate re-run not idempotent: %v", err)
	}

	for _, table := range migrationTables {
		var n int
		err := pool.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = $2`,
			freshSchema, table).Scan(&n)
		if err != nil {
			t.Fatalf("lookup %s: %v", table, err)
		}
		if n == 0 {
			t.Errorf("table %q missing after fresh migrate", table)
		}
	}

	// The memengine's CommitFacts dedups via ON CONFLICT
	// (agent_id, content_hash), which only works if memories_base
	// produced a unique index on exactly that column pair. (Cross-user
	// separation is handled in the hash input — see factHash — not the
	// index; that's covered by the memengine package's own tests.)
	upsert := `
		INSERT INTO memories (agent_id, scope, content, content_hash, source_type, user_id)
		VALUES ('test-agent', 'session', 'fresh-bootstrap fact', 'hash-1', 'test', 'tester')
		ON CONFLICT (agent_id, content_hash) DO UPDATE
		    SET access_count = memories.access_count + 1`
	for i := 0; i < 2; i++ {
		if _, err := pool.ExecContext(ctx, upsert); err != nil {
			t.Fatalf("memengine-style upsert (pass %d): %v", i+1, err)
		}
	}
	var rows, accessCount int
	if err := pool.QueryRowContext(ctx, `
		SELECT COUNT(*), MAX(access_count) FROM memories
		 WHERE agent_id = 'test-agent'`).Scan(&rows, &accessCount); err != nil {
		t.Fatalf("verify upsert: %v", err)
	}
	if rows != 1 || accessCount != 1 {
		t.Errorf("upsert dedup: got %d rows / access_count %d, want 1 row / access_count 1", rows, accessCount)
	}

	// add_user_roles_and_email's owner->admin backfill is a GATED
	// one-shot. The gate was tripped by the Migrate calls above, so a
	// user that later lands the legacy 'owner' id must NOT be auto-
	// promoted to admin on the next boot.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role)
		VALUES ('owner', 'Fresh Owner', 'approved', 'user')`); err != nil {
		t.Fatalf("seed fresh owner: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("re-migrate with a fresh owner: %v", err)
	}
	var freshRole string
	if err := pool.QueryRowContext(ctx,
		`SELECT role FROM users WHERE id = 'owner'`).Scan(&freshRole); err != nil {
		t.Fatalf("read fresh owner role: %v", err)
	}
	if freshRole != "user" {
		t.Errorf("a fresh 'owner' user was auto-promoted to %q — the owner->admin backfill is not gated", freshRole)
	}

	// wiki_fix_page_slugs_from_title is a GATED one-shot: after its first
	// run (already done by the Migrate calls above), a custom page slug
	// that differs from slugify(title) must SURVIVE a re-migrate. Before
	// the marker gate, every boot reverted it. (created_by references the
	// 'owner' row seeded just above — any existing user satisfies the FK.)
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO books (id, slug, name, created_by)
		VALUES ('11111111-1111-1111-1111-111111111111', 'kb', 'KB', 'owner')`); err != nil {
		t.Fatalf("seed book: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO wiki_pages (id, book_id, slug, title, created_by, updated_by)
		VALUES ('22222222-2222-2222-2222-222222222222',
		        '11111111-1111-1111-1111-111111111111',
		        'faq', 'Frequently Asked Questions', 'owner', 'owner')`); err != nil {
		t.Fatalf("seed custom-slug page: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("re-migrate with a custom-slug page: %v", err)
	}
	var slug string
	if err := pool.QueryRowContext(ctx,
		`SELECT slug FROM wiki_pages WHERE id = '22222222-2222-2222-2222-222222222222'`).Scan(&slug); err != nil {
		t.Fatalf("read page slug: %v", err)
	}
	if slug != "faq" {
		t.Errorf("custom slug reverted to %q on re-migrate — the slug fix is not gated", slug)
	}
}

// TestMigrateNilPool pins the guard clause: a nil pool must error, not
// panic, because main.go calls Migrate before any nil-checking of its own.
func TestMigrateNilPool(t *testing.T) {
	if err := Migrate(context.Background(), nil); err == nil {
		t.Fatal("Migrate(nil) returned nil error")
	}
	if err := Migrate(context.Background(), &Pool{}); err == nil {
		t.Fatal("Migrate(&Pool{}) returned nil error")
	}
}
