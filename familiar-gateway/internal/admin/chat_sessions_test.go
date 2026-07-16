package admin

// Chat sessions endpoint tests.
//
//   GET /console/api/sessions
//   Non-admin sees only sessions where CanonicalID == session user.
//   Admin sees all; ?user_id=<id> narrows.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/session"
)

// fakeSessionLister is a trivial in-memory ChatSessionLister.
type fakeSessionLister struct {
	sessions []*session.Session
}

func (f *fakeSessionLister) List() []*session.Session { return f.sessions }

func makeSession(id, canonical, platform string, active time.Time) *session.Session {
	s := &session.Session{
		ID:         id,
		ChannelID:  platform + ":" + canonical,
		SenderID:   canonical,
		CreatedAt:  active.Add(-time.Hour),
		LastActive: active,
	}
	s.SetIdentity(platform, canonical)
	return s
}

func TestChatSessions_NonAdminSeesOnlyOwn(t *testing.T) {
	now := time.Now()
	lister := &fakeSessionLister{sessions: []*session.Session{
		makeSession("s-1", "operator", "openai", now.Add(-1*time.Minute)),
		makeSession("s-2", "alison", "openai", now),
		makeSession("s-3", "alison", "slack", now.Add(-2*time.Minute)),
	}}
	h := &Handler{}
	h.AttachChatSessionLister(lister)

	req := httptest.NewRequest("GET", "/console/api/sessions", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listChatSessions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []chatSessionDTO `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("len = %d, want 2 (only alison's sessions)", len(body.Items))
	}
	for _, s := range body.Items {
		if s.CanonicalID != "alison" {
			t.Errorf("non-admin saw session for %s (id=%s)", s.CanonicalID, s.ID)
		}
	}
	// Sorted by LastActive desc: alison/openai (now) before alison/slack (2m ago).
	if body.Items[0].ID != "s-2" {
		t.Errorf("first item = %q, want s-2 (most recent)", body.Items[0].ID)
	}
}

func TestChatSessions_AdminSeesAllByDefault(t *testing.T) {
	now := time.Now()
	lister := &fakeSessionLister{sessions: []*session.Session{
		makeSession("s-1", "operator", "openai", now),
		makeSession("s-2", "alison", "openai", now.Add(-1*time.Minute)),
	}}
	h := &Handler{}
	h.AttachChatSessionLister(lister)

	req := httptest.NewRequest("GET", "/console/api/sessions", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.listChatSessions(rec, req)

	var body struct {
		Items []chatSessionDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Items) != 2 {
		t.Errorf("admin default len = %d, want 2 (all sessions)", len(body.Items))
	}
}

func TestChatSessions_AdminUserFilterHonored(t *testing.T) {
	now := time.Now()
	lister := &fakeSessionLister{sessions: []*session.Session{
		makeSession("s-1", "operator", "openai", now),
		makeSession("s-2", "alison", "openai", now.Add(-1*time.Minute)),
	}}
	h := &Handler{}
	h.AttachChatSessionLister(lister)

	req := httptest.NewRequest("GET", "/console/api/sessions?user_id=alison", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.listChatSessions(rec, req)

	var body struct {
		Items []chatSessionDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Items) != 1 {
		t.Fatalf("admin-filtered len = %d, want 1", len(body.Items))
	}
	if body.Items[0].CanonicalID != "alison" {
		t.Errorf("filter leaked: %+v", body.Items[0])
	}
}

func TestChatSessions_NonAdminUserFilterIgnored(t *testing.T) {
	now := time.Now()
	lister := &fakeSessionLister{sessions: []*session.Session{
		makeSession("s-1", "operator", "openai", now),
	}}
	h := &Handler{}
	h.AttachChatSessionLister(lister)

	// Non-admin tries to spoof user_id=operator.
	req := httptest.NewRequest("GET", "/console/api/sessions?user_id=operator", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listChatSessions(rec, req)

	var body struct {
		Items []chatSessionDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	// Alison has zero sessions in the manager; the spoofed user_id
	// must be ignored, and the result must be empty (not operator's).
	if len(body.Items) != 0 {
		t.Errorf("non-admin spoof returned %d items, want 0", len(body.Items))
	}
}

func TestChatSessions_UnwiredReturns503(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest("GET", "/console/api/sessions", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listChatSessions(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
