package slack

// Slack durable-conversation integration test (FAMILIAR_TEST_DSN-gated).
// Drives the real SlackAdapter end to end through the fake Socket Mode
// harness, with a DB-backed identity resolver (so an inbound DM maps to
// an approved user) and a recording Conversations fake. It pins the
// SLACK-CONTEXT contract: when an approved user DMs the bot, the turn
//
//   1. resolves the durable conversation by the per-user DM key,
//   2. persists the user prompt BEFORE the pipeline runs,
//   3. runs the pipeline under the conversation's id as the session id
//      (so hydration replays that conversation), and
//   4. persists the assistant reply AFTER.
//
// The resolver needs Postgres (its cache loads from users + identity_map),
// so this is DB-gated; the ordering/wiring it proves is the part the
// unit tests can't reach.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/familiar/gateway/internal/testutil"
	"github.com/gorilla/websocket"
)

// recorder is a shared ordered event log written by both the
// Conversations fake and the responder so the test can assert the
// persist→handle→persist sequence.
type recorder struct {
	mu     sync.Mutex
	log    []string
	sessID string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
}

func (r *recorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

type fakeConvs struct {
	rec     *recorder
	convID  string
	gotKeys []string
}

func (f *fakeConvs) EnsureExternalConversation(_ context.Context, _, externalKey, _ string) (string, error) {
	f.rec.add("ensure:" + externalKey)
	f.gotKeys = append(f.gotKeys, externalKey)
	return f.convID, nil
}

func (f *fakeConvs) AppendMessage(_ context.Context, convID, role, content, _ string) error {
	f.rec.add(fmt.Sprintf("append:%s:%s:%s", convID, role, content))
	return nil
}

type recordingResponder struct {
	rec   *recorder
	reply string
}

func (r *recordingResponder) Handle(_ context.Context, sess *session.Session, _ string, _ *sidecar.ConversationContext) (string, *pipeline.RouteInfo, error) {
	r.rec.sessID = sess.ID
	r.rec.add("handle:" + sess.UserID())
	return r.reply, &pipeline.RouteInfo{ModelID: "test/fake"}, nil
}

func TestSlackAdapter_PersistsDurableConversation(t *testing.T) {
	pool := testutil.PgTestPool(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	canonical := "slackpersist-" + suffix
	slackUser := "U" + suffix
	channel := "D" + suffix // leading D → DM

	// Seed an approved user + their Slack identity link, then build the
	// resolver so its in-memory cache loads them.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role)
		VALUES ($1, $1, 'approved', 'user') ON CONFLICT (id) DO NOTHING`, canonical); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO identity_map (platform, platform_id, canonical_id, display_name)
		VALUES ('slack', $1, $2, 'Tester') ON CONFLICT (platform, platform_id) DO NOTHING`, slackUser, canonical); err != nil {
		t.Fatalf("seed identity_map: %v", err)
	}
	resolver, err := identity.NewResolver(ctx, pool)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	rec := &recorder{}
	convs := &fakeConvs{rec: rec, convID: "conv-" + suffix}
	resp := &recordingResponder{rec: rec, reply: "here is your answer"}

	posted := make(chan string, 4)
	var wsURL string
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "user": "familiar", "user_id": "UBOT"})
	})
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "url": wsURL})
	})
	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		posted <- r.FormValue("text")
		writeJSON(w, map[string]any{"ok": true, "ts": "1.2", "channel": r.FormValue("channel")})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		payload, _ := json.Marshal(map[string]any{
			"type": "event_callback",
			"event": map[string]any{
				"type": "message", "user": slackUser, "channel": channel,
				"text": "what did you send me", "ts": "100.1", "channel_type": "im",
			},
		})
		env, _ := json.Marshal(map[string]any{"envelope_id": "e1", "type": "events_api", "payload": json.RawMessage(payload)})
		_ = conn.WriteMessage(websocket.TextMessage, env)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL = "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/ws"

	adapter := New(resp, session.NewManager(), nil, config.SlackConfig{
		BotToken: "xoxb-test", AppToken: "xapp-test", APIBaseURL: srv.URL + "/api/",
	}, false)
	adapter.SetResolver(resolver)
	adapter.SetConversations(convs)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = adapter.Run(runCtx) }()

	select {
	case <-posted: // the assistant reply is persisted before postMessage, so this gates the assertion
	case <-time.After(10 * time.Second):
		t.Fatal("no reply posted back to Slack")
	}

	// The pipeline ran under the conversation id as the session id, so
	// hydration will replay this conversation on the next turn.
	if rec.sessID != convs.convID {
		t.Errorf("session id = %q, want the conversation id %q", rec.sessID, convs.convID)
	}

	// Exact sequence: ensure the DM conversation, persist the user
	// prompt, run the turn, persist the reply.
	want := []string{
		"ensure:" + DMExternalKey(canonical),
		"append:" + convs.convID + ":user:what did you send me",
		"handle:" + canonical,
		"append:" + convs.convID + ":assistant:here is your answer",
	}
	got := rec.events()
	if len(got) != len(want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q\nfull: %v", i, got[i], want[i], got)
		}
	}
}

// Slack auto-provisioning was removed: an unknown DM must get a brief
// notice and create NO account (no identity_map row). This guards
// against the auto-register-to-approved behavior creeping back.
func TestSlackAdapter_UnknownUserGetsNoticeNotAccount(t *testing.T) {
	pool := testutil.PgTestPool(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	slackUser := "Uunknown" + suffix
	channel := "D" + suffix // DM

	// Resolver with NO link for this user — they're unknown.
	resolver, err := identity.NewResolver(ctx, pool)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	posted := make(chan string, 4)
	var wsURL string
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "user": "familiar", "user_id": "UBOT"})
	})
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "url": wsURL})
	})
	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		posted <- r.FormValue("text")
		writeJSON(w, map[string]any{"ok": true, "ts": "1.2", "channel": r.FormValue("channel")})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		payload, _ := json.Marshal(map[string]any{
			"type": "event_callback",
			"event": map[string]any{
				"type": "message", "user": slackUser, "channel": channel,
				"text": "hello, let me in", "ts": "100.1", "channel_type": "im",
			},
		})
		env, _ := json.Marshal(map[string]any{"envelope_id": "e1", "type": "events_api", "payload": json.RawMessage(payload)})
		_ = conn.WriteMessage(websocket.TextMessage, env)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL = "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/ws"

	// A responder that fails the test if it's ever reached — an unknown
	// user must never get as far as the pipeline.
	resp := &recordingResponder{rec: &recorder{}, reply: "should not happen"}
	adapter := New(resp, session.NewManager(), nil, config.SlackConfig{
		BotToken: "xoxb-test", AppToken: "xapp-test", APIBaseURL: srv.URL + "/api/",
	}, false)
	adapter.SetResolver(resolver)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = adapter.Run(runCtx) }()

	select {
	case text := <-posted:
		if !strings.Contains(text, "don't recognize") {
			t.Errorf("unknown DM reply = %q, want the unrecognized-account notice", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notice posted to the unknown user")
	}

	// No account was created.
	var n int
	if err := pool.QueryRowContext(ctx,
		`SELECT count(*) FROM identity_map WHERE platform='slack' AND platform_id=$1`, slackUser,
	).Scan(&n); err != nil {
		t.Fatalf("count identity_map: %v", err)
	}
	if n != 0 {
		t.Errorf("unknown Slack DM provisioned an account (identity_map rows=%d) — auto-register regressed", n)
	}
	if rec := resp.rec; len(rec.events()) != 0 {
		t.Errorf("pipeline was reached for an unknown user: %v", rec.events())
	}
}
