package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/identity"
)

// One link, one passkey. The token used to be consumed after the
// credential was stored, with the failure ignored, so two overlapping
// ceremonies on one link could each add a passkey.
func TestStoreEnrolledCredential_OneLinkOnePasskey(t *testing.T) {
	pool := privateAuthPool(t)
	ctx := context.Background()
	h := &Handler{credentials: &CredentialStore{pool: pool}, enrollTokens: NewEnrollmentTokenStore(pool)}
	seedAuthUser(t, pool, "bob", "user")
	tok, err := h.enrollTokens.Issue(ctx, "bob", "localhost", "boss")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.storeEnrolledCredential(ctx, tok, "first", testCredential("k-first")); err != nil {
		t.Fatal(err)
	}
	if err := h.storeEnrolledCredential(ctx, tok, "second", testCredential("k-second")); !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Errorf("a spent link stored another passkey (err %v)", err)
	}
	creds, err := h.credentials.ListByUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Errorf("bob has %d passkeys from one link, want 1", len(creds))
	}
}

// An admin can end a user's unused links, and disabling the user ends
// them too. Before, a link sent to the wrong place stayed usable for 48h.
func TestEnrollmentTokens_RevokedByAdminAndByDisable(t *testing.T) {
	pool := privateAuthPool(t)
	ctx := context.Background()
	store := NewEnrollmentTokenStore(pool)
	issue := func(user string) string {
		tok, err := store.Issue(ctx, user, "localhost", "boss")
		if err != nil {
			t.Fatal(err)
		}
		return tok.Token
	}
	bob1, bob2, alice := issue("bob"), issue("bob"), issue("alice")

	h := &Handler{enrollTokens: store}
	req := httptest.NewRequest(http.MethodDelete, "/console/api/users/bob/enrollment-tokens", nil)
	req.SetPathValue("id", "bob")
	rec := httptest.NewRecorder()
	h.revokeEnrollmentTokens(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revoked":2`) {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	for _, tok := range []string{bob1, bob2} {
		if _, err := store.Get(ctx, tok); !errors.Is(err, ErrEnrollmentTokenInvalid) {
			t.Errorf("a revoked link still resolves (err %v)", err)
		}
	}
	if _, err := store.Get(ctx, alice); err != nil {
		t.Errorf("another user's link was revoked: %v", err)
	}

	// Disabling alice ends hers.
	h.users = &fakeUserManager{users: map[string]*identity.User{
		"boss":  {ID: "boss", Role: "admin", Status: identity.StatusApproved},
		"alice": {ID: "alice", Role: "user", Status: identity.StatusApproved},
	}, adminCount: -1}
	req = httptest.NewRequest(http.MethodPost, "/console/api/users/alice/status", strings.NewReader(`{"status":"disabled"}`))
	req.SetPathValue("id", "alice")
	rec = httptest.NewRecorder()
	h.setUserStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if _, err := store.Get(ctx, alice); !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Errorf("a disabled user's link still resolves (err %v)", err)
	}
}

// Links are only issued for approved users that exist. A typo used to
// mint a link for a nonexistent id, whose passkey a later account with
// that id would inherit.
func TestCreateEnrollmentToken_OnlyForApprovedUsers(t *testing.T) {
	pool := privateAuthPool(t)
	h := &Handler{
		cfg:          config.AdminConfig{RPID: "localhost", RPOrigins: []string{"http://localhost"}},
		enrollTokens: NewEnrollmentTokenStore(pool),
		users: &fakeUserManager{users: map[string]*identity.User{
			"boss":  {ID: "boss", Role: "admin", Status: identity.StatusApproved},
			"bob":   {ID: "bob", Role: "user", Status: identity.StatusApproved},
			"carol": {ID: "carol", Role: "user", Status: identity.StatusDisabled},
		}, adminCount: -1},
	}
	for id, want := range map[string]int{"bob": http.StatusOK, "carol": http.StatusBadRequest, "bbo": http.StatusBadRequest} {
		req := httptest.NewRequest(http.MethodPost, "/console/api/auth/enrollment-token",
			strings.NewReader(`{"canonical_id":"`+id+`","target_rp_id":"localhost"}`))
		req = req.WithContext(context.WithValue(req.Context(), ctxAuthUserKey, AuthUser{UserID: "boss", Role: "admin"}))
		rec := httptest.NewRecorder()
		h.createEnrollmentToken(rec, req)
		if rec.Code != want {
			t.Errorf("link for %s: status %d (%s), want %d", id, rec.Code, strings.TrimSpace(rec.Body.String()), want)
		}
	}
}

// A link stops working once its user is no longer approved, at both
// steps of the ceremony (before, only the token's own state counted).
func TestEnrollCeremony_RefusesUnapprovedUser(t *testing.T) {
	pool := privateAuthPool(t)
	h, err := New(config.AdminConfig{Enabled: true, RPID: "localhost", RPOrigins: []string{"http://localhost"}, FirstUserID: "boss"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	fm := &fakeUserManager{users: map[string]*identity.User{
		"bob": {ID: "bob", Role: "user", Status: identity.StatusApproved},
	}, adminCount: -1}
	h.users = fm
	tok, err := h.enrollTokens.Issue(context.Background(), "bob", "localhost", "boss")
	if err != nil {
		t.Fatal(err)
	}
	post := func(fn http.HandlerFunc, path string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"token":"`+tok.Token+`","attestation":{}}`))
		req.Host = "localhost"
		rec := httptest.NewRecorder()
		fn(rec, req)
		return rec.Code
	}
	if code := post(h.enrollBegin, "/console/api/auth/enroll/begin"); code != http.StatusOK {
		t.Fatalf("begin for an approved user: %d", code)
	}
	fm.users["bob"].Status = identity.StatusDisabled
	if code := post(h.enrollBegin, "/console/api/auth/enroll/begin"); code != http.StatusForbidden {
		t.Errorf("begin for a disabled user: %d, want 403", code)
	}
	if code := post(h.enrollFinish, "/console/api/auth/enroll/finish"); code != http.StatusForbidden {
		t.Errorf("finish for a disabled user: %d, want 403", code)
	}
}
