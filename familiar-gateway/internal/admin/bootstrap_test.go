package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/testdsn"
	"github.com/familiar/gateway/internal/testutil"
	"github.com/go-webauthn/webauthn/webauthn"
)

// First-run bootstrap and passkey deletion, against a private schema:
// bootstrap is decided over the whole users/credentials tables, so the
// shared test schema (other packages write users concurrently) won't do.
func privateAuthPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv(testutil.EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s not set", testutil.EnvDSN)
	}
	ctx := context.Background()
	adminPool, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminPool.Close() })
	schema := "auth_bootstrap_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	if _, err := adminPool.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = adminPool.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
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

func seedAuthUser(t *testing.T, pool *db.Pool, id, role string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO users (id, display_name, status, role) VALUES ($1, $1, 'approved', $2)`, id, role); err != nil {
		t.Fatal(err)
	}
}

func testCredential(id string) *webauthn.Credential {
	return &webauthn.Credential{ID: []byte(id)}
}

// Once any user has registered a passkey, first-run registration (no
// session needed) stays closed, even if every credential is deleted. It
// used to reopen whenever the credentials table was empty.
func TestBootstrap_StaysClosedAfterCredentialsGone(t *testing.T) {
	pool := privateAuthPool(t)
	cs := &CredentialStore{pool: pool}
	ctx := context.Background()
	if open, err := cs.BootstrapOpen(ctx); err != nil || !open {
		t.Fatalf("fresh instance: open=%v err=%v, want open", open, err)
	}
	seedAuthUser(t, pool, "boss", "admin")
	if err := cs.Insert(ctx, "boss", "key", testCredential("k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `DELETE FROM webauthn_credentials`); err != nil {
		t.Fatal(err)
	}
	if open, _ := cs.BootstrapOpen(ctx); open {
		t.Error("deleting every credential reopened first-run registration")
	}
}

// Only one first-run ceremony can complete. A second one begun in the
// open window used to finish for the pending TTL after the first.
func TestBootstrap_ClaimOnce(t *testing.T) {
	pool := privateAuthPool(t)
	cs := &CredentialStore{pool: pool}
	ctx := context.Background()
	seedAuthUser(t, pool, "boss", "admin")
	if ok, err := cs.ClaimBootstrap(ctx, "boss"); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, _ := cs.ClaimBootstrap(ctx, "boss"); ok {
		t.Error("a second first-run completed after the first")
	}
	// A claim whose credential never got stored can be retried.
	if err := cs.ReleaseBootstrap(ctx, "boss"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := cs.ClaimBootstrap(ctx, "boss"); !ok {
		t.Error("a released claim couldn't be retried")
	}
}

// An instance upgraded with passkeys already registered is closed from
// its first boot on the new code, whatever happens to its credentials.
func TestBootstrap_MigrationBackfillsExistingInstances(t *testing.T) {
	pool := privateAuthPool(t)
	ctx := context.Background()
	seedAuthUser(t, pool, "boss", "admin")
	blob, _ := json.Marshal(testCredential("k1"))
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO webauthn_credentials (id, credential_blob, user_id, webauthn_user_handle, display_name)
		VALUES ('k1', $1, 'boss', 'boss', 'legacy')`, blob); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `DELETE FROM webauthn_credentials`); err != nil {
		t.Fatal(err)
	}
	if open, _ := (&CredentialStore{pool: pool}).BootstrapOpen(ctx); open {
		t.Error("an upgraded instance's bootstrap reopened")
	}
}

// The unauthenticated status says only whether setup is open, not how
// many passkeys exist.
func TestRegisterStatus_ReportsOnlyOpenOrClosed(t *testing.T) {
	pool := privateAuthPool(t)
	h := &Handler{credentials: &CredentialStore{pool: pool}}
	status := func() map[string]any {
		w := httptest.NewRecorder()
		h.registerStatus(w, httptest.NewRequest("GET", "/", nil))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if got := status(); got["credentials_registered"] != float64(0) || got["requires_auth"] != false {
		t.Errorf("fresh instance status = %v", got)
	}
	seedAuthUser(t, pool, "boss", "admin")
	ctx := context.Background()
	for _, k := range []string{"k1", "k2", "k3"} {
		_ = h.credentials.Insert(ctx, "boss", k, testCredential(k))
	}
	if got := status(); got["credentials_registered"] != float64(1) || got["requires_auth"] != true {
		t.Errorf("bootstrapped status = %v (the count must not leak)", got)
	}
}

// The last admin passkey can't be deleted: with bootstrap closed for
// good, nobody could administer the instance again.
func TestDeleteUserPasskey_KeepsLastAdminKey(t *testing.T) {
	pool := privateAuthPool(t)
	ctx := context.Background()
	res, err := identity.NewResolver(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{credentials: &CredentialStore{pool: pool}, users: res, sessions: NewSessionStore(pool)}
	seedAuthUser(t, pool, "boss", "admin")
	if err := h.credentials.Insert(ctx, "boss", "only", testCredential("only-key")); err != nil {
		t.Fatal(err)
	}
	del := func(credID string) int {
		r := httptest.NewRequest("DELETE", "/", nil)
		r.SetPathValue("credential_id", base64.RawURLEncoding.EncodeToString([]byte(credID)))
		r = r.WithContext(context.WithValue(r.Context(), ctxAuthUserKey, AuthUser{UserID: "boss", Role: "admin"}))
		w := httptest.NewRecorder()
		h.deleteUserPasskey(w, r)
		return w.Code
	}
	if code := del("only-key"); code != http.StatusConflict {
		t.Errorf("deleting the last admin passkey: %d, want 409", code)
	}
	if err := h.credentials.Insert(ctx, "boss", "second", testCredential("second-key")); err != nil {
		t.Fatal(err)
	}
	if code := del("only-key"); code != http.StatusOK {
		t.Errorf("deleting one of two admin passkeys: %d, want 200", code)
	}
}

// A deleted passkey's sessions end, including the ones minted before
// sessions recorded their passkey (they might be this key's), except
// the caller's own.
func TestDeleteUserPasskey_EndsUnboundSessions(t *testing.T) {
	pool := privateAuthPool(t)
	ctx := context.Background()
	res, err := identity.NewResolver(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{credentials: &CredentialStore{pool: pool}, users: res, sessions: NewSessionStore(pool)}
	seedAuthUser(t, pool, "sam", "user")
	for _, k := range []string{"lost-key", "home-key"} {
		if err := h.credentials.Insert(ctx, "sam", k, testCredential(k)); err != nil {
			t.Fatal(err)
		}
	}
	legacy, _ := h.sessions.Create(ctx, "sam", time.Hour) // pre-upgrade: no passkey recorded
	mine, _ := h.sessions.Create(ctx, "sam", time.Hour)   // the caller's own (also unbound)
	r := httptest.NewRequest("DELETE", "/", nil)
	r.SetPathValue("credential_id", base64.RawURLEncoding.EncodeToString([]byte("lost-key")))
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: mine})
	r = r.WithContext(context.WithValue(r.Context(), ctxAuthUserKey, AuthUser{UserID: "sam", Role: "user"}))
	w := httptest.NewRecorder()
	h.deleteUserPasskey(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := h.sessions.Validate(ctx, legacy); err == nil {
		t.Error("an unbound session survived deleting the passkey")
	}
	if _, err := h.sessions.Validate(ctx, mine); err != nil {
		t.Error("the caller's own session was ended")
	}
}
