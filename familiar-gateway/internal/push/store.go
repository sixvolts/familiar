// Package push is the Web Push (PWA notification) subsystem: a store of
// per-device subscriptions and a Sender that signs + delivers encrypted
// notifications via VAPID. It's used by the scheduled-action `push`
// delivery target to notify users who aren't on Slack, with a deep link
// back into the conversation the action's output landed in.
package push

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/familiar/gateway/internal/db"
)

// Subscription is one device's Web Push registration — the endpoint URL
// the browser's push service handed us plus the two encryption keys.
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Store persists push subscriptions in push_subscriptions.
type Store struct {
	pool *db.Pool
}

// NewStore wires a subscription store onto the shared pool.
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// Upsert stores (or refreshes) a subscription. Keyed on endpoint, so a
// re-subscribe from the same device updates the row rather than
// duplicating — and re-homes it to the current user if it changed.
func (s *Store) Upsert(ctx context.Context, userID string, sub Subscription, userAgent string) error {
	if userID == "" || sub.Endpoint == "" || sub.P256dh == "" || sub.Auth == "" {
		return fmt.Errorf("push: upsert: user_id, endpoint, and keys required")
	}
	ua := sql.NullString{String: userAgent, Valid: userAgent != ""}
	_, err := s.pool.ExecContext(ctx, `
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, user_agent)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (endpoint) DO UPDATE SET
			user_id    = EXCLUDED.user_id,
			p256dh     = EXCLUDED.p256dh,
			auth       = EXCLUDED.auth,
			user_agent = EXCLUDED.user_agent`,
		userID, sub.Endpoint, sub.P256dh, sub.Auth, ua)
	if err != nil {
		return fmt.Errorf("push: upsert: %w", err)
	}
	return nil
}

// DeleteByEndpoint removes a subscription. With a non-empty userID it
// scopes the delete to that owner (the unsubscribe endpoint); with ""
// it deletes regardless of owner (the Sender pruning a dead endpoint
// reported gone by the push service).
func (s *Store) DeleteByEndpoint(ctx context.Context, userID, endpoint string) error {
	if endpoint == "" {
		return nil
	}
	var err error
	if userID != "" {
		_, err = s.pool.ExecContext(ctx,
			`DELETE FROM push_subscriptions WHERE endpoint = $1 AND user_id = $2`, endpoint, userID)
	} else {
		_, err = s.pool.ExecContext(ctx,
			`DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	}
	if err != nil {
		return fmt.Errorf("push: delete: %w", err)
	}
	return nil
}

// ListForUser returns every device subscription for a user.
func (s *Store) ListForUser(ctx context.Context, userID string) ([]Subscription, error) {
	rows, err := s.pool.QueryContext(ctx,
		`SELECT endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("push: list: %w", err)
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth); err != nil {
			return nil, fmt.Errorf("push: list scan: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// HasAny reports whether a user has at least one subscription — lets the
// delivery path skip seeding/sending work for users who never enabled
// notifications.
func (s *Store) HasAny(ctx context.Context, userID string) (bool, error) {
	var n int
	err := s.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM push_subscriptions WHERE user_id = $1`, userID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("push: hasany: %w", err)
	}
	return n > 0, nil
}
