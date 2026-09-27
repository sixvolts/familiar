package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeSlackAPI serves conversations.members (two pages) and records
// chat.postMessage channels.
type fakeSlackAPI struct {
	mu          sync.Mutex
	posted      []string
	membersErr  string // e.g. "missing_scope"
	membersPage [][]string
}

func (f *fakeSlackAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/conversations.members", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if f.membersErr != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": f.membersErr})
			return
		}
		page := 0
		if r.Form.Get("cursor") == "p2" {
			page = 1
		}
		next := ""
		if page == 0 && len(f.membersPage) > 1 {
			next = "p2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "members": f.membersPage[page],
			"response_metadata": map[string]string{"next_cursor": next},
		})
	})
	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.posted = append(f.posted, r.Form.Get("channel"))
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": r.Form.Get("channel"), "ts": "1.2"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A scheduled action's channel post goes only where its owner may post.
// Any approved user could make the bot post anything to any channel or
// person, private channels included.
func TestPostForOwner(t *testing.T) {
	member := ChannelOwner{SlackUserID: "U_MEMBER"}
	outsider := ChannelOwner{SlackUserID: "U_OUTSIDER"}
	for _, tc := range []struct {
		name     string
		channel  string
		owner    ChannelOwner
		allowed  []string
		apiErr   string
		wantPost bool
	}{
		{"member of the channel", "C_TEAM", member, nil, "", true},
		{"not a member", "C_ADMINS", outsider, nil, "", false},
		{"a person's id", "U_CFO", member, nil, "", false},
		{"a DM id", "D_12345", member, nil, "", false},
		{"outside the bot's channel list", "C_TEAM", member, []string{"C_OTHER"}, "", false},
		{"in the bot's channel list", "C_TEAM", member, []string{"C_TEAM"}, "", true},
		{"no linked Slack identity", "C_TEAM", ChannelOwner{}, nil, "", false},
		{"membership can't be checked", "C_TEAM", member, nil, "missing_scope", false},
		{"admin, not a member", "C_ADMINS", ChannelOwner{IsAdmin: true}, nil, "", true},
		{"admin, a person's id", "U_CFO", ChannelOwner{IsAdmin: true}, nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeSlackAPI{membersErr: tc.apiErr,
				membersPage: [][]string{{"U_A", "U_B"}, {"U_MEMBER"}}} // on the second page
			srv := api.server(t)
			s, err := NewSender("xoxb-test", srv.URL+"/api/")
			if err != nil {
				t.Fatal(err)
			}
			err = s.PostForOwner(context.Background(), tc.channel, tc.owner, tc.allowed, "report")
			posted := len(api.posted) == 1 && api.posted[0] == tc.channel
			if posted != tc.wantPost {
				t.Errorf("posted=%v (err %v), want %v", posted, err, tc.wantPost)
			}
			if !tc.wantPost && err == nil {
				t.Error("a refused post returned no error; the run must record why")
			}
		})
	}
}
