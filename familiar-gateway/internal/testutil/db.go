// Package testutil provides shared helpers for gateway tests.
//
// Tests that need a real PostgreSQL instance call PgTestDB, which reads
// FAMILIAR_TEST_DSN and skips the test when unset so the default
// `go test ./...` run stays GPU- and database-free. Set the env var
// (e.g. `postgres://localhost/familiar_test?sslmode=disable`) in your
// shell or via `make test-integration` to opt in.
package testutil

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/db"
	_ "github.com/lib/pq"
)

// EnvDSN is the environment variable consulted by PgTestDB.
const EnvDSN = "FAMILIAR_TEST_DSN"

// PgTestDB opens a connection to the test Postgres instance and returns
// a *sql.DB. It skips the test when FAMILIAR_TEST_DSN is unset so
// unit-only runs stay hermetic. A ping with a short deadline verifies
// the server is reachable before the test runs. Close is registered via
// t.Cleanup so callers don't have to unwind manually.
func PgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s not set", EnvDSN)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping %s: %v", EnvDSN, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// PgTestPool opens a *db.Pool against FAMILIAR_TEST_DSN and runs the
// gateway's migrations so every test sees the same schema. Skips when
// the env var is unset.
func PgTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s not set", EnvDSN)
	}
	pool, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, pool); err != nil {
		_ = pool.Close()
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// TruncateTables wipes the named tables inside a single statement so
// tests start from a known state. Accepts either *sql.DB or *db.Pool
// via the Execer interface.
func TruncateTables(t *testing.T, e Execer, tables ...string) {
	t.Helper()
	if len(tables) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stmt := "TRUNCATE "
	for i, tbl := range tables {
		if i > 0 {
			stmt += ", "
		}
		stmt += tbl
	}
	if _, err := e.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("truncate %v: %v", tables, err)
	}
}

// Execer is the minimal interface TruncateTables needs. Both *sql.DB
// and *db.Pool satisfy it.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
