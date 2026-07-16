// Package userprofile stores the per-user "assistant personality"
// prompt: a user-authored block of behavioral instructions that the
// pipeline surfaces as its own labeled section right after the admin
// system prompt.
//
// This is all that survives of the old Layer-1 "working context"
// blob. That blob mixed three different things — a prompt, durable
// facts, and conversation state — under one JSONB column. The facts
// belong in the memory store and conversation state belongs in the
// session; only the personality prompt is genuinely a prompt, so it
// gets a typed column of its own and the blob is gone.
package userprofile

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/familiar/gateway/internal/db"
)

// Store reads and writes the user_prompt column of user_profiles. It
// borrows the shared connection pool from internal/db; Close is a
// no-op because pool lifetime is owned by main(). Schema is managed
// by db.Migrate.
type Store struct {
	db *db.Pool
}

// NewStore returns a user-profile store backed by the shared pool.
// The user_profiles table must already exist — run db.Migrate first.
func NewStore(pool *db.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("userprofile: nil pool")
	}
	return &Store{db: pool}, nil
}

// Get returns the personality prompt for a user. A missing row (or a
// blank user ID) returns "" with a nil error — callers treat "no
// profile" the same as "empty prompt".
func (s *Store) Get(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}
	var prompt string
	err := s.db.QueryRowContext(ctx,
		`SELECT user_prompt FROM user_profiles WHERE user_id = $1`, userID,
	).Scan(&prompt)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("userprofile: get %s: %w", userID, err)
	}
	return strings.TrimSpace(prompt), nil
}

// Set writes the personality prompt for a user, creating the row if
// it doesn't exist. The value is trimmed; an empty string is a valid
// "no personality" state and clears any prior prompt.
func (s *Store) Set(ctx context.Context, userID, prompt string) error {
	if userID == "" {
		return fmt.Errorf("userprofile: empty user_id")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_profiles (user_id, user_prompt, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE
		SET user_prompt = EXCLUDED.user_prompt,
		    updated_at  = NOW()
	`, userID, strings.TrimSpace(prompt))
	if err != nil {
		return fmt.Errorf("userprofile: set %s: %w", userID, err)
	}
	return nil
}

// Close is a no-op: the Store borrows its pool from main().
func (s *Store) Close() error { return nil }
