package db

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
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
	"pending_embeds",
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

	// The memories CHECK is added once, not dropped and re-added (a
	// full-table scan under an exclusive lock) on every boot. It must
	// still exist and still bite.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO memories (agent_id, scope, content, content_hash, source_type, user_id)
		VALUES ('t', 'session', 'x', 'hash-empty-user', 'test', '')`); err == nil {
		t.Error("memories accepted an empty user_id: memories_user_id_nonempty is missing")
	}

	// The two supersede repairs are GATED one-shots too. Each ran on the
	// first Migrate above; a row that later matches a repair's pattern
	// must survive the next boot.
	for _, fix := range []string{"repair_inverted_supersedes", "isolated_supersede_repair"} {
		var n int
		if err := pool.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM applied_data_fixes WHERE name = $1`, fix).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: applied_data_fixes marker count %d (err %v), want 1", fix, n, err)
		}
	}
	var older, newer string
	if err := pool.QueryRowContext(ctx, `
		INSERT INTO memories (agent_id, scope, content, content_hash, source_type, user_id, created_at)
		VALUES ('t', 'session', 'older', 'hash-older', 'test', 'tester', NOW() - INTERVAL '1 day')
		RETURNING id::text`).Scan(&older); err != nil {
		t.Fatalf("seed older: %v", err)
	}
	if err := pool.QueryRowContext(ctx, `
		INSERT INTO memories (agent_id, scope, content, content_hash, source_type, user_id)
		VALUES ('t', 'session', 'newer', 'hash-newer', 'test', 'tester')
		RETURNING id::text`).Scan(&newer); err != nil {
		t.Fatalf("seed newer: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`UPDATE memories SET supersedes = $1::uuid WHERE id = $2::uuid`, newer, older); err != nil {
		t.Fatalf("seed inverted pointer: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("re-migrate with an inverted pointer: %v", err)
	}
	var kept sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT supersedes::text FROM memories WHERE id = $1::uuid`, older).Scan(&kept); err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	if !kept.Valid {
		t.Error("repair_inverted_supersedes re-ran on a later boot — it is not gated")
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

// scratchPools returns a pool on dsn and one on a freshly migrated
// scratch schema of the same database, which no other package's tests
// touch. The schema is dropped when the test ends.
func scratchPools(t *testing.T, schema string) (admin, pool *Pool) {
	t.Helper()
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
	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Fatalf("drop stale schema: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err = Open(dsn + sep + "options=" + url.QueryEscape("-csearch_path="+schema+",public"))
	if err != nil {
		t.Fatalf("db.Open (scoped): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate on fresh schema: %v", err)
	}
	return admin, pool
}

// awaitLockWait returns once some session is waiting for a lock on
// relation (schema-qualified). Migrate queues behind every other
// package's run for the advisory lock first, so under go test ./...
// this can take a while.
func awaitLockWait(t *testing.T, admin *Pool, relation string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting bool
		if err := admin.QueryRowContext(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_locks
			                WHERE relation = to_regclass($1) AND NOT granted)`,
			relation).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing ever waited for a lock on %s", relation)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The deadlock go test ./... hit. A shard delete locks shards, then
// its ON DELETE CASCADE locks shard_passkeys; shard_auth_phase1 locks
// shard_passkeys (CREATE INDEX IF NOT EXISTS), then alters shards.
// Holding admin_sessions, which the migration alters in between, lets
// the delete start waiting first, so Postgres picked the delete as the
// victim. The migration must yield instead.
func TestMigrateYieldsToAShardDelete(t *testing.T) {
	const schema = "migrate_deadlock_test"
	admin, pool := scratchPools(t, schema)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role) VALUES ('owner', 'Owner', 'approved', 'user')`); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO shards (id, owner_id, name, persistence, visibility, scope_tag, system_prompt)
		VALUES ('doomed', 'owner', 'doomed', 'persistent', 'isolated', 'shard:doomed', 'p')`); err != nil {
		t.Fatalf("seed shard: %v", err)
	}

	sessions, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = sessions.Rollback() }()
	if _, err := sessions.ExecContext(ctx, `LOCK TABLE admin_sessions IN ACCESS SHARE MODE`); err != nil {
		t.Fatalf("hold admin_sessions: %v", err)
	}
	del, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = del.Rollback() }()
	if _, err := del.ExecContext(ctx, `SELECT 1 FROM shards WHERE id = 'doomed' FOR UPDATE`); err != nil {
		t.Fatalf("lock the shard row: %v", err)
	}

	migCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	migErr := make(chan error, 1)
	go func() { migErr <- Migrate(migCtx, pool) }()
	awaitLockWait(t, admin, schema+".admin_sessions")

	delErr := make(chan error, 1)
	go func() {
		_, err := del.ExecContext(ctx, `DELETE FROM shards WHERE id = 'doomed'`)
		delErr <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := sessions.Commit(); err != nil {
		t.Fatalf("release admin_sessions: %v", err)
	}
	if err := <-delErr; err != nil {
		t.Fatalf("shard delete during Migrate: %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("commit the delete: %v", err)
	}
	if err := <-migErr; err != nil {
		t.Fatalf("Migrate during a shard delete: %v", err)
	}
}

// A migration that can't get a table lock must give it up rather than
// wait in the lock queue, where an ACCESS EXCLUSIVE request blocks
// every later reader of the table until whoever holds it is done.
func TestMigrateYieldsTableLocks(t *testing.T) {
	const schema = "migrate_lock_test"
	admin, pool := scratchPools(t, schema)
	ctx := context.Background()

	// Another session reads shards in a transaction it keeps open.
	holder, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `LOCK TABLE shards IN ACCESS SHARE MODE`); err != nil {
		t.Fatalf("hold shards: %v", err)
	}

	migCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	migErr := make(chan error, 1)
	go func() { migErr <- Migrate(migCtx, pool) }()
	awaitLockWait(t, admin, schema+".shards")

	// A reader arriving now waits at most one lock timeout behind it.
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	start := time.Now()
	if _, err := pool.ExecContext(readCtx, `SELECT 1 FROM shards LIMIT 1`); err != nil {
		t.Fatalf("reader stuck behind Migrate's lock request: %v (after %s)", err, time.Since(start))
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("reader waited %s behind Migrate's lock request", waited)
	}

	// Once the holder is done, Migrate finishes.
	if err := holder.Commit(); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	if err := <-migErr; err != nil {
		t.Fatalf("Migrate after the lock was released: %v", err)
	}

	// Migrate's session goes back to the pool; its timeout must not.
	pool.SetMaxOpenConns(1)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate on one connection: %v", err)
	}
	var lockTimeout string
	if err := pool.QueryRowContext(ctx, `SHOW lock_timeout`).Scan(&lockTimeout); err != nil {
		t.Fatalf("show lock_timeout: %v", err)
	}
	if lockTimeout != "0" {
		t.Errorf("pooled session kept lock_timeout = %s after Migrate", lockTimeout)
	}
}

// Waiting for another boot's Migrate is the advisory lock doing its
// job; the lock timeout must not cut it short.
func TestMigrateWaitsOutAnotherMigrate(t *testing.T) {
	dsn := os.Getenv("FAMILIAR_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping: FAMILIAR_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := Open(dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	other, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer other.Close()
	if _, err := other.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, int64(migrateLockKey)); err != nil {
		t.Fatalf("take the migrate lock: %v", err)
	}
	migErr := make(chan error, 1)
	go func() { migErr <- Migrate(ctx, pool) }()
	time.Sleep(2 * migrateLockTimeout)
	if _, err := other.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, int64(migrateLockKey)); err != nil {
		t.Fatalf("release the migrate lock: %v", err)
	}
	if err := <-migErr; err != nil {
		t.Fatalf("Migrate behind another Migrate: %v", err)
	}
}

func TestRunMigrationRetriesLostLockRaces(t *testing.T) {
	for _, code := range []pq.ErrorCode{"55P03", "40P01"} {
		calls := 0
		exec := func(context.Context, string, ...any) (sql.Result, error) {
			calls++
			if calls < 3 {
				return nil, &pq.Error{Code: code}
			}
			return nil, nil
		}
		if err := runMigration(context.Background(), exec, migration{name: "m", ddl: "SELECT 1"}); err != nil {
			t.Errorf("%s: runMigration = %v, want nil after retries", code, err)
		}
		if calls != 3 {
			t.Errorf("%s: %d attempts, want 3", code, calls)
		}
	}
}

func TestRunMigrationReturnsOtherErrorsAtOnce(t *testing.T) {
	for _, want := range []error{&pq.Error{Code: "42P01"}, errors.New("driver: bad connection")} {
		calls := 0
		exec := func(context.Context, string, ...any) (sql.Result, error) {
			calls++
			return nil, want
		}
		if err := runMigration(context.Background(), exec, migration{name: "m", ddl: "SELECT 1"}); !errors.Is(err, want) {
			t.Errorf("runMigration = %v, want %v", err, want)
		}
		if calls != 1 {
			t.Errorf("%v: %d attempts, want 1", want, calls)
		}
	}
}

func TestRunMigrationGivesUpWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	calls := 0
	exec := func(context.Context, string, ...any) (sql.Result, error) {
		calls++
		return nil, &pq.Error{Code: "55P03"}
	}
	done := make(chan error, 1)
	go func() { done <- runMigration(ctx, exec, migration{name: "m", ddl: "SELECT 1"}) }()
	select {
	case err := <-done:
		var pqErr *pq.Error
		if !errors.As(err, &pqErr) || pqErr.Code != "55P03" {
			t.Errorf("runMigration = %v, want the last lock timeout", err)
		}
		if calls < 2 {
			t.Errorf("%d attempts before giving up, want retries", calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runMigration kept retrying after its context ended")
	}
}
