package slack

// Socket Mode integration test against a FAKE Slack — no network, no
// real workspace. An httptest server emulates the Slack Web API
// (auth.test, apps.connections.open, chat.postMessage) and upgrades
// one route to a WebSocket that speaks the Socket Mode envelope
// protocol. The real SlackAdapter.Run drives it end to end: dial →
// hello → events_api(message) → pipeline → reply posted back. The
// reply is asserted to be mrkdwn-converted, so the inbound socket
// path and the formatting fix are both covered without Slack.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/gorilla/websocket"
)

// fakeResponder returns a canned reply, standing in for the pipeline.
type fakeResponder struct {
	reply   string
	gotText chan string
}

func (f *fakeResponder) Handle(_ context.Context, _ *session.Session, userMsg string, _ *sidecar.ConversationContext) (string, *pipeline.RouteInfo, error) {
	select {
	case f.gotText <- userMsg:
	default:
	}
	return f.reply, &pipeline.RouteInfo{ModelID: "test/fake"}, nil
}

func TestSlackAdapter_SocketModeRoundTrip(t *testing.T) {
	posted := make(chan string, 4)
	var wsURL string
	upgrader := websocket.Upgrader{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "user": "familiar", "user_id": "UBOT"})
	})
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "url": wsURL})
	})
	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		posted <- r.FormValue("text")
		writeJSON(w, map[string]any{"ok": true, "ts": "1.2", "channel": r.FormValue("channel")})
	})
	// Socket Mode WebSocket: on connect, greet then push one message
	// event; drain the adapter's acks so its inline writer never blocks.
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})

		payload := json.RawMessage(`{
			"type":"event_callback","team_id":"T1","api_app_id":"A1",
			"event":{"type":"message","user":"U1","channel":"D1",
			         "text":"hello there","ts":"100.1","channel_type":"im"}
		}`)
		env, _ := json.Marshal(map[string]any{
			"envelope_id": "e1", "type": "events_api", "payload": payload,
		})
		_ = conn.WriteMessage(websocket.TextMessage, env)

		// Hold the connection open + drain acks until the test ends.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL = "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/ws"

	resp := &fakeResponder{
		// Markdown the pipeline would emit — must arrive mrkdwn-ified.
		reply:   "## Report\n\n**bold** and a [link](http://x)\n\n- item\n\n---",
		gotText: make(chan string, 1),
	}
	adapter := New(resp, session.NewManager(), nil, config.SlackConfig{
		BotToken:   "xoxb-test",
		AppToken:   "xapp-test",
		APIBaseURL: srv.URL + "/api/",
	}, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = adapter.Run(ctx) }()

	// The inbound text reached the pipeline…
	select {
	case got := <-resp.gotText:
		if got != "hello there" {
			t.Fatalf("pipeline got %q, want %q", got, "hello there")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pipeline never received the socket message")
	}

	// …and the reply posted back is Slack mrkdwn, not raw CommonMark.
	select {
	case text := <-posted:
		for _, want := range []string{"*Report*", "*bold*", "<http://x|link>", "•  item", mrkdwnRule} {
			if !strings.Contains(text, want) {
				t.Errorf("posted reply missing %q\n--- got ---\n%s", want, text)
			}
		}
		if strings.Contains(text, "## Report") || strings.Contains(text, "**bold**") {
			t.Errorf("posted reply still has raw markdown:\n%s", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no reply posted back to Slack")
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// A model turn that yields no usable content (e.g. only a scrubbed
// protocol tag) must NOT post a blank Slack message.
func TestSlackAdapter_EmptyReplyNotPosted(t *testing.T) {
	posted := make(chan string, 4)
	var wsURL string
	upgrader := websocket.Upgrader{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "user": "familiar", "user_id": "UBOT"})
	})
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "url": wsURL})
	})
	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		posted <- r.FormValue("text")
		writeJSON(w, map[string]any{"ok": true, "ts": "1.2"})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		payload := json.RawMessage(`{"type":"event_callback","event":{"type":"message","user":"U1","channel":"D1","text":"hi","ts":"1.1","channel_type":"im"}}`)
		env, _ := json.Marshal(map[string]any{"envelope_id": "e1", "type": "events_api", "payload": payload})
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

	resp := &fakeResponder{reply: "   ", gotText: make(chan string, 1)}
	adapter := New(resp, session.NewManager(), nil, config.SlackConfig{
		BotToken: "xoxb", AppToken: "xapp", APIBaseURL: srv.URL + "/api/",
	}, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = adapter.Run(ctx) }()

	select {
	case <-resp.gotText: // pipeline ran
	case <-time.After(10 * time.Second):
		t.Fatal("pipeline never received the message")
	}
	select {
	case text := <-posted:
		t.Fatalf("posted a blank/whitespace message: %q", text)
	case <-time.After(1500 * time.Millisecond):
		// Good — nothing posted.
	}
}
