package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// fakeStore is an in-memory SubscriptionStore for Sender tests.
type fakeStore struct {
	subs    []Subscription
	deleted []string
}

func (f *fakeStore) ListForUser(_ context.Context, _ string) ([]Subscription, error) {
	return f.subs, nil
}
func (f *fakeStore) DeleteByEndpoint(_ context.Context, _, endpoint string) error {
	f.deleted = append(f.deleted, endpoint)
	return nil
}

// validSubKeys produces a real P-256 public key (p256dh) + 16-byte auth
// secret, base64url-encoded, so the webpush library's payload encryption
// actually succeeds (it needs a valid client public key). We don't keep
// the private key — the fake push server never decrypts.
func validSubKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ecdh: %v", err)
	}
	authBytes := make([]byte, 16)
	if _, err := rand.Read(authBytes); err != nil {
		t.Fatalf("gen auth: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(authBytes)
}

func newSender(t *testing.T, store SubscriptionStore) *Sender {
	t.Helper()
	priv, pub, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("vapid: %v", err)
	}
	return NewSender(store, pub, priv, "mailto:test@example.com")
}

func TestSender_DeliversToSubscriptions(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// A real push service requires the VAPID Authorization header.
		if r.Header.Get("Authorization") == "" {
			t.Errorf("missing VAPID Authorization header")
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p256dh, auth := validSubKeys(t)
	store := &fakeStore{subs: []Subscription{{Endpoint: srv.URL, P256dh: p256dh, Auth: auth}}}
	s := newSender(t, store)

	n, err := s.Send(context.Background(), "user-1", Payload{
		Title: "Morning digest", Body: "Ready", URL: "/#chat/abc", Tag: "action:x",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n != 1 {
		t.Errorf("delivered = %d, want 1", n)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("push endpoint hit %d times, want 1", hits)
	}
	if len(store.deleted) != 0 {
		t.Errorf("nothing should be pruned on success, pruned %v", store.deleted)
	}
}

func TestSender_PrunesGoneEndpoints(t *testing.T) {
	// A push service replies 410 Gone for an expired/uninstalled sub.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	p256dh, auth := validSubKeys(t)
	store := &fakeStore{subs: []Subscription{{Endpoint: srv.URL, P256dh: p256dh, Auth: auth}}}
	s := newSender(t, store)

	n, err := s.Send(context.Background(), "user-1", Payload{Title: "x", URL: "/"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n != 0 {
		t.Errorf("delivered = %d, want 0 (endpoint was gone)", n)
	}
	if len(store.deleted) != 1 || store.deleted[0] != srv.URL {
		t.Errorf("dead endpoint not pruned: %v", store.deleted)
	}
}

func TestSender_NoSubscriptionsIsNoop(t *testing.T) {
	s := newSender(t, &fakeStore{})
	n, err := s.Send(context.Background(), "user-1", Payload{Title: "x"})
	if err != nil || n != 0 {
		t.Errorf("empty send: n=%d err=%v, want 0/nil", n, err)
	}
}
