package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/testutil"
)

// Deleting a conversation removes its rolling summary (the sessions row
// keyed by its id, which has no foreign key) and its in-memory session
// (up to 100 verbatim turns, listed on the sessions panel). Both stayed.
func TestDeleteConversation_TakesItsSummaryAndSession(t *testing.T) {
	pool := testutil.PgTestPool(t)
	s := NewConversationStore(pool)
	ctx := context.Background()
	user := fmt.Sprintf("del-user-%d", time.Now().UnixNano())
	seedUser(t, s, user)
	conv, err := s.Create(ctx, user, "Sensitive", "familiar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO sessions (session_key, running_summary, summarized_count, updated_at) VALUES ($1, 'they discussed X', 8, NOW())`,
		conv.ID); err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager()
	mgr.GetOrCreateWithID(conv.ID, "workspace", user)
	other := mgr.GetOrCreateWithID("someone-elses", "workspace", "bob")

	h := &Handler{}
	h.AttachConversationStore(s)
	h.AttachChatSessionLister(mgr)
	req := httptest.NewRequest("DELETE", "/console/api/conversations/"+conv.ID, nil).
		WithContext(ctxWithAuth(ctx, AuthUser{UserID: user, Role: "user"}))
	req.SetPathValue("id", conv.ID)
	w := httptest.NewRecorder()
	h.deleteConversation(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var n int
	_ = pool.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE session_key = $1`, conv.ID).Scan(&n)
	if n != 0 {
		t.Error("the conversation's summary row survived the delete")
	}
	if _, ok := mgr.Get(conv.ID); ok {
		t.Error("the conversation's in-memory session survived the delete")
	}
	if _, ok := mgr.Get(other.ID); !ok {
		t.Error("another session was dropped")
	}

	// Someone else's conversation id leaves the session alone.
	mgr.GetOrCreateWithID("3f2c9a1e-7b4d-4e8a-9c1f-2a6b8d0e4f13", "workspace", "bob")
	req = httptest.NewRequest("DELETE", "/", nil).WithContext(ctxWithAuth(ctx, AuthUser{UserID: user, Role: "user"}))
	req.SetPathValue("id", "3f2c9a1e-7b4d-4e8a-9c1f-2a6b8d0e4f13")
	w = httptest.NewRecorder()
	h.deleteConversation(w, req)
	if _, ok := mgr.Get("3f2c9a1e-7b4d-4e8a-9c1f-2a6b8d0e4f13"); !ok || w.Code != http.StatusNotFound {
		t.Errorf("a non-owner's delete (status %d) dropped the owner's session", w.Code)
	}
}
