package profile

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/skills"
)

// fakeEmailUpdater captures SetUserEmail calls so tests can assert
// what the skill would have written + control the returned error.
type fakeEmailUpdater struct {
	err       error
	gotUserID string
	gotEmail  string
	callCount int
}

func (f *fakeEmailUpdater) SetUserEmail(ctx context.Context, userID, email string) error {
	f.callCount++
	f.gotUserID = userID
	f.gotEmail = email
	return f.err
}

func TestSkill_TopLevelShape(t *testing.T) {
	s := New(&fakeEmailUpdater{})
	if s.Name() != "profile" {
		t.Errorf("Name = %q, want 'profile'", s.Name())
	}
	tools := s.Tools()
	if len(tools) != 1 || tools[0].Name != "update_my_email" {
		t.Errorf("expected exactly update_my_email, got %+v", tools)
	}
}

func TestExecute_UnknownToolErrors(t *testing.T) {
	s := New(&fakeEmailUpdater{})
	res, _ := s.Execute(context.Background(), "not_a_tool", nil)
	if !strings.Contains(res.Error, "unknown tool") {
		t.Errorf("want 'unknown tool', got %q", res.Error)
	}
}

func TestExecute_MissingUpdaterErrors(t *testing.T) {
	s := New(nil)
	res, _ := s.Execute(withUser(context.Background(), "owner"),
		"update_my_email", json.RawMessage(`{"email":"a@b.com"}`))
	if !strings.Contains(res.Error, "not configured") {
		t.Errorf("want 'not configured', got %q", res.Error)
	}
}

func TestExecute_MissingEmailRejected(t *testing.T) {
	s := New(&fakeEmailUpdater{})
	res, _ := s.Execute(withUser(context.Background(), "owner"),
		"update_my_email", json.RawMessage(`{}`))
	if !strings.Contains(res.Error, "email is required") {
		t.Errorf("want 'email is required', got %q", res.Error)
	}
}

func TestExecute_InvalidEmailRejected(t *testing.T) {
	s := New(&fakeEmailUpdater{})
	res, _ := s.Execute(withUser(context.Background(), "owner"),
		"update_my_email", json.RawMessage(`{"email":"not an email"}`))
	if !strings.Contains(res.Error, "not a valid email") {
		t.Errorf("want 'not a valid email', got %q", res.Error)
	}
}

func TestExecute_MissingCallerRejected(t *testing.T) {
	// No session context — the skill refuses rather than silently
	// using an empty UserID, which would hit the resolver's "missing
	// userID" branch and produce a confusing error.
	s := New(&fakeEmailUpdater{})
	res, _ := s.Execute(context.Background(),
		"update_my_email", json.RawMessage(`{"email":"a@b.com"}`))
	if !strings.Contains(res.Error, "cannot identify caller") {
		t.Errorf("want 'cannot identify caller', got %q", res.Error)
	}
}

func TestExecute_HappyPath(t *testing.T) {
	upd := &fakeEmailUpdater{}
	s := New(upd)
	res, err := s.Execute(withUser(context.Background(), "owner"),
		"update_my_email", json.RawMessage(`{"email":"admin@example.com"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	if upd.callCount != 1 {
		t.Errorf("SetUserEmail call count = %d, want 1", upd.callCount)
	}
	if upd.gotUserID != "owner" {
		t.Errorf("got userID %q, want 'owner'", upd.gotUserID)
	}
	if upd.gotEmail != "admin@example.com" {
		t.Errorf("got email %q, want 'admin@example.com'", upd.gotEmail)
	}
	if !strings.Contains(res.Content, "admin@example.com") {
		t.Errorf("content should mention the linked email: %s", res.Content)
	}
}

func TestExecute_NamedAddressNormalizedToBare(t *testing.T) {
	// net/mail accepts "Name <addr>"; the skill strips to bare
	// address so identity_map and users.email stay clean.
	upd := &fakeEmailUpdater{}
	s := New(upd)
	_, _ = s.Execute(withUser(context.Background(), "owner"),
		"update_my_email", json.RawMessage(`{"email":"Ada Lovelace <admin@example.com>"}`))
	if upd.gotEmail != "admin@example.com" {
		t.Errorf("expected bare address, got %q", upd.gotEmail)
	}
}

func TestExecute_AlreadyHasEmailFriendlyMessage(t *testing.T) {
	upd := &fakeEmailUpdater{err: errors.New("identity: set email: user \"owner\" missing or already has an email")}
	s := New(upd)
	res, err := s.Execute(withUser(context.Background(), "owner"),
		"update_my_email", json.RawMessage(`{"email":"new@example.com"}`))
	if err != nil {
		t.Fatalf("error should be surfaced as friendly content, not Go error: %v", err)
	}
	if !strings.Contains(res.Content, "already on file") && !strings.Contains(res.Content, "already") {
		t.Errorf("want 'already on file' message, got %q", res.Content)
	}
}

func TestExecute_DuplicateEmailFriendlyMessage(t *testing.T) {
	// Postgres returns "duplicate key" on a unique-index conflict —
	// the skill should surface a low-information message that keeps
	// attacker email enumeration hard.
	upd := &fakeEmailUpdater{err: errors.New("pq: duplicate key value violates unique constraint \"idx_users_email_unique\"")}
	s := New(upd)
	res, err := s.Execute(withUser(context.Background(), "owner"),
		"update_my_email", json.RawMessage(`{"email":"taken@example.com"}`))
	if err != nil {
		t.Fatalf("duplicate shouldn't escape as Go error: %v", err)
	}
	if !strings.Contains(res.Content, "already associated") {
		t.Errorf("want 'already associated', got %q", res.Content)
	}
}

// withUser mirrors what the pipeline does: installs a
// skills.SessionContext so ContextFrom returns (ctx, true) with the
// right UserID. Keeps the test file self-contained without importing
// the pipeline.
func withUser(ctx context.Context, userID string) context.Context {
	return skills.WithContext(ctx, skills.SessionContext{UserID: userID})
}
