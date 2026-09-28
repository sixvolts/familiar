package push

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/familiar/gateway/internal/skills/fetch"
)

// Payload is the JSON the service worker receives in its `push` event.
// Keep it small — Web Push payloads are capped (~4KB after encryption),
// and the full output lives behind the deep link, not on the lock
// screen, so Body is a short preview only.
type Payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// URL is the in-app deep link to open on tap (e.g. "/#chat/<id>").
	URL string `json:"url"`
	// Tag collapses repeat notifications for the same source (e.g. one
	// scheduled action) so a daily digest doesn't stack N notifications.
	Tag string `json:"tag,omitempty"`
}

// SubscriptionStore is the slice of the store the Sender needs: list a
// user's subscriptions and prune a dead endpoint. *Store satisfies it;
// narrowing to an interface lets the Sender be tested without a DB.
type SubscriptionStore interface {
	ListForUser(ctx context.Context, userID string) ([]Subscription, error)
	DeleteByEndpoint(ctx context.Context, userID, endpoint string) error
}

// Sender signs and delivers Web Push notifications via VAPID. Construct
// it only when push is configured (both VAPID keys present); the actions
// deliverer treats a nil Sender as "push disabled".
type Sender struct {
	store      SubscriptionStore
	publicKey  string
	privateKey string
	subject    string
	// policy screens endpoints again at send time, so a subscription
	// stored before endpoints were checked is pruned, not posted to.
	policy EndpointPolicy
	// client posts to push services: no redirects (a push service
	// never redirects, and following one turned an allowed endpoint
	// into a request anywhere), a timeout, and no dialing of
	// non-public addresses. Tests replace it.
	client *http.Client
	// allowEndpoint overrides policy.Check (tests post to httptest).
	allowEndpoint func(string) error
}

// WithEndpointPolicy sets the extra allowed push hosts.
func (s *Sender) WithEndpointPolicy(p EndpointPolicy) *Sender {
	s.policy = p
	return s
}

// NewSender builds a Sender over a subscription store and the VAPID
// keypair + contact subject.
func NewSender(store SubscriptionStore, publicKey, privateKey, subject string) *Sender {
	return &Sender{
		store: store, publicKey: publicKey, privateKey: privateKey, subject: subject,
		client: &http.Client{
			Timeout:   15 * time.Second,
			Transport: fetch.SafeTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Send delivers a notification to every device the user subscribed.
// Returns the number of endpoints that accepted it. Per-endpoint
// failures are logged, not fatal; an endpoint the push service reports
// gone (404/410) is pruned so the table self-heals. The push payload is
// end-to-end encrypted to each subscription by the webpush library.
func (s *Sender) Send(ctx context.Context, userID string, p Payload) (int, error) {
	subs, err := s.store.ListForUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		// This is the failure mode that went unnoticed for two days: Apple
		// 410'd the only endpoint, it was correctly pruned, and afterwards
		// every notification silently went nowhere. A working system and a
		// completely dead one produced identical logs. Say so.
		log.Printf("[push] NO SUBSCRIPTIONS for user %s — notification dropped (%q). "+
			"Re-enable notifications in the app to register a new endpoint.", userID, p.Title)
		return 0, nil
	}
	msg, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}

	delivered := 0
	check := s.policy.Check
	if s.allowEndpoint != nil {
		check = s.allowEndpoint
	}
	for _, sub := range subs {
		if err := check(sub.Endpoint); err != nil {
			log.Printf("[push] pruning subscription with a disallowed endpoint %s: %v", shorten(sub.Endpoint), err)
			_ = s.store.DeleteByEndpoint(ctx, "", sub.Endpoint)
			continue
		}
		resp, err := webpush.SendNotificationWithContext(ctx, msg, &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
		}, &webpush.Options{
			// webpush-go auto-prepends "mailto:" to non-https subscribers,
			// so strip it from the configured subject to avoid doubling.
			Subscriber:      strings.TrimPrefix(s.subject, "mailto:"),
			VAPIDPublicKey:  s.publicKey,
			VAPIDPrivateKey: s.privateKey,
			TTL:             86400, // hold for a day if the device is offline
			HTTPClient:      s.client,
		})
		if err != nil {
			log.Printf("[push] send to %s failed: %v", shorten(sub.Endpoint), err)
			continue
		}
		// Always drain + close so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			// The subscription is dead (browser uninstalled / expired).
			if derr := s.store.DeleteByEndpoint(ctx, "", sub.Endpoint); derr != nil {
				log.Printf("[push] prune dead endpoint %s: %v", shorten(sub.Endpoint), derr)
			} else {
				log.Printf("[push] pruned dead endpoint %s (%d)", shorten(sub.Endpoint), resp.StatusCode)
			}
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			delivered++
		default:
			log.Printf("[push] endpoint %s returned %d", shorten(sub.Endpoint), resp.StatusCode)
		}
	}
	// Summarise every send. Previously ONLY failures logged, so a healthy
	// system and a totally dead one were indistinguishable in the journal —
	// which is exactly how a pruned endpoint went unnoticed for two days.
	// Push volume is a handful of notifications a day, so a line per send is
	// cheap and makes "is push working?" answerable by grep.
	if delivered == 0 {
		log.Printf("[push] delivered 0/%d for user %s (%q) — nothing reached a device",
			len(subs), userID, p.Title)
	} else {
		log.Printf("[push] delivered %d/%d for user %s (%q)",
			delivered, len(subs), userID, p.Title)
	}
	return delivered, nil
}

// GenerateVAPIDKeys returns a fresh (privateKey, publicKey) base64url
// VAPID pair to drop into the [push] config block. Exposed so the
// gateway can offer a one-shot key generator without main.go importing
// the webpush library directly.
func GenerateVAPIDKeys() (privateKey, publicKey string, err error) {
	return webpush.GenerateVAPIDKeys()
}

// shorten trims an endpoint URL for logs (they're long + contain a
// device-specific token we don't want fully in the logs).
func shorten(endpoint string) string {
	if len(endpoint) <= 48 {
		return endpoint
	}
	return endpoint[:48] + "…"
}
