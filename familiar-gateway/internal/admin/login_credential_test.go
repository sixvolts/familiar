package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
)

// Login identity comes only from the credential the assertion names. It
// used to start from the instance's first credential (normally the
// bootstrap admin) and switch only when the rawId lookup hit, so a miss
// verified against every user's credentials and could mint an admin
// session.
func TestLoginCredential_OnlyTheNamedCredentialsOwner(t *testing.T) {
	pool := privateAuthPool(t)
	h := &Handler{credentials: &CredentialStore{pool: pool}}
	ctx := context.Background()
	seedAuthUser(t, pool, "boss", "admin")
	seedAuthUser(t, pool, "bob", "user")
	for _, c := range []struct{ user, id string }{{"boss", "k-boss"}, {"bob", "k-bob1"}, {"bob", "k-bob2"}} {
		if err := h.credentials.Insert(ctx, c.user, "key", testCredential(c.id)); err != nil {
			t.Fatal(err)
		}
	}

	for _, raw := range [][]byte{nil, []byte("k-revoked")} {
		if owner, _, err := h.loginCredential(ctx, raw); !errors.Is(err, ErrNotFound) {
			t.Errorf("rawId %q: owner %+v err %v, want ErrNotFound", raw, owner, err)
		}
	}

	owner, own, err := h.loginCredential(ctx, []byte("k-bob1"))
	if err != nil {
		t.Fatal(err)
	}
	if owner.UserID != "bob" {
		t.Errorf("owner = %q, want bob", owner.UserID)
	}
	if len(own) != 2 {
		t.Fatalf("verifying against %d credentials, want bob's 2", len(own))
	}
	for _, c := range own {
		if c.UserID != "bob" {
			t.Errorf("verifying against %s's credential %s", c.UserID, c.ID)
		}
	}
}

// The rawId is read with the library's decoder, which takes padded
// base64. A strict decode here once missed on a padded id that
// FinishLogin still accepted.
func TestAssertionRawID_AcceptsPadding(t *testing.T) {
	id := []byte("sixteen-byte-id!")
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding} {
		body := []byte(`{"rawId":"` + enc.EncodeToString(id) + `"}`)
		if got := assertionRawID(body); !bytes.Equal(got, id) {
			t.Errorf("%s: got %q, want %q", body, got, id)
		}
	}
	if got := assertionRawID([]byte(`{}`)); got != nil {
		t.Errorf("no rawId: got %q", got)
	}
}

// The credential handed to FinishLogin carries the latest sign counter.
// The blob keeps the registration-time counter, and the column (updated
// every login) was never read back, so clone detection compared against
// the registration value forever.
func TestScanCredential_LoadsTheCurrentSignCount(t *testing.T) {
	pool := privateAuthPool(t)
	cs := &CredentialStore{pool: pool}
	ctx := context.Background()
	seedAuthUser(t, pool, "bob", "user")
	if err := cs.Insert(ctx, "bob", "key", testCredential("k-count")); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateSignCount(ctx, []byte("k-count"), 7); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetByRawID(ctx, []byte("k-count"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Credential.Authenticator.SignCount != 7 {
		t.Errorf("credential counter = %d, want the stored 7", got.Credential.Authenticator.SignCount)
	}
}

// Shard passkeys carry their latest counter too (same fix as users').
func TestScanShardPasskey_LoadsTheCurrentSignCount(t *testing.T) {
	pool := privateAuthPool(t)
	ctx := context.Background()
	seedAuthUser(t, pool, "owner", "user")
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO shards (id, owner_id, name, scope_tag, persistence, visibility, system_prompt) VALUES ('kiosk', 'owner', 'Kiosk', 'shard:kiosk', 'persistent', 'isolated', 'x')`); err != nil {
		t.Fatal(err)
	}
	st := NewShardPasskeyStore(pool)
	if _, err := st.Insert(ctx, "kiosk", "key", "owner", testCredential("k-shard")); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSignCount(ctx, []byte("k-shard"), 9); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetByRawID(ctx, []byte("k-shard"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Credential.Authenticator.SignCount != 9 {
		t.Errorf("credential counter = %d, want the stored 9", got.Credential.Authenticator.SignCount)
	}
}
