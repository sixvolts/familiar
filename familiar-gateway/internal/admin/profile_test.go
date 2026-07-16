package admin

// Profile endpoint tests.
//
//   GET   /console/api/profile  — current user's personality prompt
//   PATCH /console/api/profile  — replace the personality prompt
//   ?user_id=<id> admin override — non-admin's value is ignored
//
// Coverage:
//   - GET returns an empty string when no row exists (not 404)
//   - GET scopes to session user for non-admin
//   - GET honors ?user_id= for admin
//   - PATCH replaces the user_prompt
//   - PATCH on non-admin ignores ?user_id= spoof — writes hit session user
//   - PATCH with malformed body returns 400
//   - 503 when profile store isn't wired

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeProfileStore is a map-backed stand-in for *userprofile.Store.
// Records every Set so tests can assert on the userID + value the
// handler forwarded.
type fakeProfileStore struct {
	mu              sync.Mutex
	rows            map[string]string
	lastWriteUserID string
	lastWriteValue  string
}

func newFakeProfileStore() *fakeProfileStore {
	return &fakeProfileStore{rows: map[string]string{}}
}

func (f *fakeProfileStore) Get(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[userID], nil
}

func (f *fakeProfileStore) Set(_ context.Context, userID, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastWriteUserID = userID
	f.lastWriteValue = prompt
	f.rows[userID] = prompt
	return nil
}

// ──────────────────────────────────────────────────────────────────
// GET
// ──────────────────────────────────────────────────────────────────

func TestProfile_GetEmptyReturnsEmptyString(t *testing.T) {
	store := newFakeProfileStore()
	h := &Handler{}
	h.AttachProfileStore(store)

	req := httptest.NewRequest("GET", "/console/api/profile", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.getProfile(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (missing row should not 404)", rec.Code)
	}
	var body struct {
		UserID     string `json:"user_id"`
		UserPrompt string `json:"user_prompt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.UserID != "alison" {
		t.Errorf("user_id = %q, want alison", body.UserID)
	}
	if body.UserPrompt != "" {
		t.Errorf("user_prompt = %q, want empty", body.UserPrompt)
	}
}

func TestProfile_GetReturnsExistingRow(t *testing.T) {
	store := newFakeProfileStore()
	store.rows["alison"] = "Be concise and direct."
	h := &Handler{}
	h.AttachProfileStore(store)

	req := httptest.NewRequest("GET", "/console/api/profile", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.getProfile(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		UserPrompt string `json:"user_prompt"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.UserPrompt != "Be concise and direct." {
		t.Errorf("round-trip lost prompt: %q", body.UserPrompt)
	}
}

func TestProfile_GetAdminOverrideHonored(t *testing.T) {
	store := newFakeProfileStore()
	store.rows["alison"] = "alison's prompt"
	h := &Handler{}
	h.AttachProfileStore(store)

	// Admin asks for alison's profile via ?user_id=
	req := httptest.NewRequest("GET", "/console/api/profile?user_id=alison", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.getProfile(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		UserID     string `json:"user_id"`
		UserPrompt string `json:"user_prompt"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.UserID != "alison" {
		t.Errorf("user_id = %q, want alison (admin's ?user_id= should be honored)", body.UserID)
	}
	if body.UserPrompt != "alison's prompt" {
		t.Errorf("admin saw wrong profile data: %q", body.UserPrompt)
	}
}

func TestProfile_GetNonAdminOverrideIgnored(t *testing.T) {
	store := newFakeProfileStore()
	store.rows["operator"] = "operator's secret prompt"
	h := &Handler{}
	h.AttachProfileStore(store)

	// Non-admin tries to spoof ?user_id=operator.
	req := httptest.NewRequest("GET", "/console/api/profile?user_id=operator", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.getProfile(rec, req)

	var body struct {
		UserID     string `json:"user_id"`
		UserPrompt string `json:"user_prompt"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.UserID != "alison" {
		t.Errorf("non-admin's spoofed user_id reached the store: got %q, want alison", body.UserID)
	}
	if body.UserPrompt != "" {
		t.Errorf("non-admin saw another user's prompt: %q", body.UserPrompt)
	}
}

// ──────────────────────────────────────────────────────────────────
// PATCH
// ──────────────────────────────────────────────────────────────────

func TestProfile_PatchReplacesUserPrompt(t *testing.T) {
	store := newFakeProfileStore()
	store.rows["alison"] = "old prompt"
	h := &Handler{}
	h.AttachProfileStore(store)

	body := []byte(`{"user_prompt":"new prompt"}`)
	req := httptest.NewRequest("PATCH", "/console/api/profile", bytes.NewReader(body)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.patchProfile(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if store.rows["alison"] != "new prompt" {
		t.Errorf("replacement contents wrong: %q", store.rows["alison"])
	}
}

func TestProfile_PatchNonAdminCannotTargetAnother(t *testing.T) {
	store := newFakeProfileStore()
	store.rows["operator"] = "operator's prompt"
	h := &Handler{}
	h.AttachProfileStore(store)

	body := []byte(`{"user_prompt":"hacked"}`)
	req := httptest.NewRequest("PATCH", "/console/api/profile?user_id=operator", bytes.NewReader(body)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.patchProfile(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	// Alison's row should have been written; Operator's must be untouched.
	if store.rows["operator"] != "operator's prompt" {
		t.Errorf("non-admin PATCH leaked into another user's profile: %q", store.rows["operator"])
	}
	if store.rows["alison"] != "hacked" {
		t.Errorf("non-admin PATCH did not write to their own profile: %q", store.rows["alison"])
	}
	if store.lastWriteUserID != "alison" {
		t.Errorf("Set called with userID = %q, want alison", store.lastWriteUserID)
	}
}

func TestProfile_PatchAdminCanTargetAnother(t *testing.T) {
	store := newFakeProfileStore()
	h := &Handler{}
	h.AttachProfileStore(store)

	body := []byte(`{"user_prompt":"reset by admin"}`)
	req := httptest.NewRequest("PATCH", "/console/api/profile?user_id=alison", bytes.NewReader(body)).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.patchProfile(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if store.lastWriteUserID != "alison" {
		t.Errorf("admin-override Set targeted %q, want alison", store.lastWriteUserID)
	}
}

func TestProfile_PatchMalformedReturns400(t *testing.T) {
	store := newFakeProfileStore()
	h := &Handler{}
	h.AttachProfileStore(store)

	req := httptest.NewRequest("PATCH", "/console/api/profile",
		bytes.NewReader([]byte(`{not valid json`))).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.patchProfile(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestProfile_UnwiredReturns503(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest("GET", "/console/api/profile", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.getProfile(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
