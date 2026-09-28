package admin

// EnsureExternalConversation integration test (FAMILIAR_TEST_DSN-gated).
// This is the load-bearing primitive for SLACK-CONTEXT: a Slack DM and
// the scheduled-action slack_dm deliverer must resolve to the SAME
// conversation row through one external key, so a digest the action
// posts is hydrated by the user's next reply. The invariant worth
// pinning is idempotency (same key → same id) and that the hydration
// read (LoadRecentTurns) replays what was appended.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/testutil"
)

func seedUser(t *testing.T, s *ConversationStore, id string) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO users (id, display_name, status, role)
		VALUES ($1, $1, 'approved', 'user')
		ON CONFLICT (id) DO NOTHING`, id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

// The workspace chat list (List) must omit external (Slack) conversations
// so they don't bleed into the chat tab, while native chats still show.
func TestList_ExcludesExternalConversations(t *testing.T) {
	pool := testutil.PgTestPool(t)
	s := NewConversationStore(pool)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "listext-user-" + suffix
	seedUser(t, s, userID)

	native, err := s.Create(ctx, userID, "Native chat", "familiar")
	if err != nil {
		t.Fatalf("create native: %v", err)
	}
	ext, err := s.EnsureExternalConversation(ctx, userID, "slack:dm:"+userID, "Slack DM")
	if err != nil {
		t.Fatalf("ensure external: %v", err)
	}

	list, err := s.List(ctx, userID, false, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	foundNative, foundExt := false, false
	for _, c := range list {
		if c.ID == native.ID {
			foundNative = true
		}
		if c.ID == ext.ID {
			foundExt = true
		}
	}
	if !foundNative {
		t.Error("native conversation should appear in the workspace list")
	}
	if foundExt {
		t.Error("external (Slack) conversation must be excluded from the workspace list")
	}
}

func TestEnsureExternalConversation_IdempotentAndHydrates(t *testing.T) {
	pool := testutil.PgTestPool(t)
	s := NewConversationStore(pool)
	ctx := context.Background()

	// Unique per run — external_key is globally UNIQUE and the test DB
	// outlives a single run.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "slackctx-user-" + suffix
	seedUser(t, s, userID)
	key := "slack:dm:" + userID

	first, err := s.EnsureExternalConversation(ctx, userID, key, "Slack DM")
	if err != nil {
		t.Fatalf("ensure (first): %v", err)
	}
	if first.UserID != userID {
		t.Errorf("owner = %q, want %q", first.UserID, userID)
	}

	// Same key resolves to the SAME row — this is what makes the
	// adapter's inbound path and the deliverer's outbound path agree.
	second, err := s.EnsureExternalConversation(ctx, userID, key, "Different Title")
	if err != nil {
		t.Fatalf("ensure (second): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("same external key produced two conversations: %s vs %s", first.ID, second.ID)
	}

	// Simulate the scheduled deliverer writing a digest, then the
	// user's reply — the hydration read must replay both in order.
	if _, err := s.AppendMessage(ctx, &Message{ConversationID: first.ID, Role: "assistant", Content: "your daily digest", Model: "scheduled:Daily"}); err != nil {
		t.Fatalf("append digest: %v", err)
	}
	if _, err := s.AppendMessage(ctx, &Message{ConversationID: first.ID, Role: "user", Content: "same as yesterday?"}); err != nil {
		t.Fatalf("append reply: %v", err)
	}

	var got []string
	err = s.LoadRecentTurns(ctx, first.ID, userID, 10, 0, func(role, content string, _ []byte, _ string) {
		got = append(got, role+":"+content)
	})
	if err != nil {
		t.Fatalf("LoadRecentTurns: %v", err)
	}
	if len(got) != 2 || got[0] != "assistant:your daily digest" || got[1] != "user:same as yesterday?" {
		t.Fatalf("hydration replay = %v, want [assistant:digest, user:reply]", got)
	}

	// skip leaves out the messages a rolling summary already covers.
	got = nil
	err = s.LoadRecentTurns(ctx, first.ID, userID, 10, 1, func(role, content string, _ []byte, _ string) {
		got = append(got, role+":"+content)
	})
	if err != nil || len(got) != 1 || got[0] != "user:same as yesterday?" {
		t.Fatalf("replay after skipping 1 = %v (%v), want [user:reply]", got, err)
	}

	// A different key is a different conversation.
	other, err := s.EnsureExternalConversation(ctx, userID, "slack:thread:C1:171.5"+suffix, "Slack thread")
	if err != nil {
		t.Fatalf("ensure (thread): %v", err)
	}
	if other.ID == first.ID {
		t.Errorf("distinct external keys collapsed to one conversation")
	}
}

// getConversation loads the whole thread via MessagesAll on open/refresh.
// The paginated Messages() defaults a zero limit to 100, so a conversation
// past 100 rows used to lose its recent turns from the UI on reload (the
// server session kept the context, so the chat still "carried forward").
// MessagesAll must return every row, oldest-first, including the newest.
func TestMessagesAll_ReturnsWholeThreadPast100(t *testing.T) {
	pool := testutil.PgTestPool(t)
	s := NewConversationStore(pool)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "msgall-user-" + suffix
	seedUser(t, s, userID)

	conv, err := s.Create(ctx, userID, "Long chat", "familiar")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const total = 250 // well past the paginated 100 default
	for i := 0; i < total; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if _, err := s.AppendMessage(ctx, &Message{
			ConversationID: conv.ID, Role: role, Content: fmt.Sprintf("msg-%03d", i),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// The buggy path (what getConversation used to call) caps at 100.
	capped, err := s.Messages(ctx, conv.ID, 0, 0)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(capped) != 100 {
		t.Fatalf("Messages(0,0) = %d rows, expected the 100 cap (guards the regression premise)", len(capped))
	}

	// The fix returns the whole thread, oldest-first, INCLUDING the newest
	// message — the one the capped read dropped.
	all, err := s.MessagesAll(ctx, conv.ID)
	if err != nil {
		t.Fatalf("messages all: %v", err)
	}
	if len(all) != total {
		t.Fatalf("MessagesAll = %d rows, want all %d", len(all), total)
	}
	if all[0].Content != "msg-000" {
		t.Errorf("first message = %q, want msg-000 (oldest first)", all[0].Content)
	}
	if last := all[len(all)-1].Content; last != fmt.Sprintf("msg-%03d", total-1) {
		t.Errorf("last message = %q, want the newest msg-%03d (the turn the 100-cap dropped)", last, total-1)
	}
}

// The session id doubles as the conversation id, and several adapters
// derive session ids a client can influence. Hydration and intermediate
// writes must therefore check the conversation belongs to the session's
// user, or a colliding id reads and writes someone else's thread.
func TestConversationStore_TurnsAreOwnerScoped(t *testing.T) {
	pool := testutil.PgTestPool(t)
	s := NewConversationStore(pool)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, other := "own-owner-"+suffix, "own-other-"+suffix
	seedUser(t, s, owner)
	seedUser(t, s, other)
	c, err := s.Create(ctx, owner, "Private", "familiar")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AppendMessage(ctx, &Message{ConversationID: c.ID, Role: "user", Content: "my private question"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	var loaded []string
	visit := func(_, content string, _ []byte, _ string) { loaded = append(loaded, content) }
	if err := s.LoadRecentTurns(ctx, c.ID, other, 10, 0, visit); err != nil {
		t.Fatalf("load as other: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("another user's session hydrated %d turn(s) of this conversation", len(loaded))
	}
	if err := s.LoadRecentTurns(ctx, c.ID, owner, 10, 0, visit); err != nil || len(loaded) != 1 {
		t.Fatalf("owner hydrate: %d turn(s), err=%v; want 1", len(loaded), err)
	}

	err = s.AppendIntermediateMessages(ctx, c.ID, other, []pipeline.IntermediateMessage{{Role: "tool", Content: "injected", ToolCallID: "t1"}})
	if err == nil {
		t.Fatal("another user's session wrote into this conversation")
	}
	msgs, err := s.MessagesAll(ctx, c.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	for _, m := range msgs {
		if m.Content == "injected" {
			t.Fatal("the refused write landed anyway")
		}
	}
}

// An external key already bound to another user's conversation is
// refused rather than handed back as a shared conversation.
func TestEnsureExternalConversation_RefusesAnotherUsersKey(t *testing.T) {
	pool := testutil.PgTestPool(t)
	s := NewConversationStore(pool)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	alice, bob := "ext-alice-"+suffix, "ext-bob-"+suffix
	seedUser(t, s, alice)
	seedUser(t, s, bob)
	key := "slack:thread:C1:171.5:" + suffix
	if _, err := s.EnsureExternalConversation(ctx, alice, key, "Slack thread"); err != nil {
		t.Fatalf("alice ensure: %v", err)
	}
	if _, err := s.EnsureExternalConversation(ctx, bob, key, "Slack thread"); !errors.Is(err, ErrExternalConversationOwner) {
		t.Fatalf("bob got alice's conversation (err=%v), want ErrExternalConversationOwner", err)
	}
	if c, err := s.EnsureExternalConversation(ctx, alice, key, "Slack thread"); err != nil || c.UserID != alice {
		t.Fatalf("alice re-ensure: %+v, %v", c, err)
	}
}

// The gateway now writes each turn's final reply. A client from before
// that still POSTs the same reply after the stream; it must get the
// gateway's row back rather than store a second copy.
func TestAppendMessage_OldClientRepeatOfServerReplyIsNotStoredTwice(t *testing.T) {
	pool := testutil.PgTestPool(t)
	s := NewConversationStore(pool)
	ctx := context.Background()
	owner := fmt.Sprintf("dedupe-owner-%d", time.Now().UnixNano())
	seedUser(t, s, owner)
	c, err := s.Create(ctx, owner, "t", "familiar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, &Message{ConversationID: c.ID, Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendIntermediateMessages(ctx, c.ID, owner, []pipeline.IntermediateMessage{
		{Role: "assistant", Content: "hello there", Model: "big", ReasoningContent: "Generating response...\n"},
	}); err != nil {
		t.Fatalf("server-side persist: %v", err)
	}

	h := &Handler{conversations: s}
	post := func(body string) int {
		req := httptest.NewRequest("POST", "/console/api/conversations/"+c.ID+"/messages", strings.NewReader(body))
		req.SetPathValue("id", c.ID)
		req = req.WithContext(ctxWithAuth(req.Context(), AuthUser{UserID: owner, Role: "user", PrincipalType: PrincipalTypeUser, PrincipalID: owner}))
		w := httptest.NewRecorder()
		h.appendConversationMessage(w, req)
		return w.Code
	}
	if code := post(`{"role":"assistant","content":"hello there","model":"big"}`); code != http.StatusCreated {
		t.Fatalf("repeat POST status %d", code)
	}
	msgs, err := s.MessagesAll(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (the repeat must not be stored)", len(msgs))
	}
	if last := msgs[len(msgs)-1]; last.Model != "big" || last.ReasoningContent == "" {
		t.Errorf("server row lost its model or reasoning: %+v", last)
	}
	// A different reply is a real new message and is stored.
	if code := post(`{"role":"assistant","content":"something else"}`); code != http.StatusCreated {
		t.Fatalf("new POST status %d", code)
	}
	if msgs, _ = s.MessagesAll(ctx, c.ID); len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
}
