// Package db owns the single shared PostgreSQL connection pool and the
// gateway's schema migrations. Every store that persists state
// (memory/pgvector, session, userprofile, identity) talks to the same
// *db.Pool so connection lifetime and migration ordering live in one
// place instead of being re-derived inside each store's NewX constructor.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"time"

	_ "github.com/lib/pq"
)

// Pool wraps *sql.DB so callers can pass a single typed handle around
// instead of a bare *sql.DB. The embedded pointer promotes QueryContext,
// ExecContext, etc., so store code that used *sql.DB needs no rewrites
// at call sites — only the field type changes.
type Pool struct {
	*sql.DB
}

// Open dials PostgreSQL at the given DSN, configures pool sizing, and
// verifies connectivity with a bounded ping. Callers own the returned
// pool and must Close it when the gateway shuts down.
func Open(dsn string) (*Pool, error) {
	sqlDB, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open postgres: %w", err)
	}

	sqlDB.SetMaxOpenConns(5)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: ping postgres: %w", err)
	}

	log.Printf("[memory] connected to pgvector at %s", SanitizeDSN(dsn))
	return &Pool{DB: sqlDB}, nil
}

// Close releases the underlying *sql.DB. Safe to call on a nil Pool.
func (p *Pool) Close() error {
	if p == nil || p.DB == nil {
		return nil
	}
	return p.DB.Close()
}

// SanitizeDSN strips the password from a Postgres DSN for logging.
// URL form (postgres://user:pass@host/db) is rewritten to mask the
// password segment; DSNs without a `user:pass@` block pass through
// unchanged, and key-value form `password=...` is also masked.
func SanitizeDSN(dsn string) string {
	urlRe := regexp.MustCompile(`(postgres(?:ql)?://[^:@/]+):[^@]*@`)
	out := urlRe.ReplaceAllString(dsn, "${1}:***@")
	kvRe := regexp.MustCompile(`password=[^\s]+`)
	out = kvRe.ReplaceAllString(out, "password=***")
	return out
}
