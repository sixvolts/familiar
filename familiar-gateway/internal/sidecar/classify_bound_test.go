package sidecar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/modelrole"
)

// classifyClient wires a Client whose classify role points at one endpoint.
func classifyClient(t *testing.T, endpoint string, cfg config.SidecarConfig) *Client {
	t.Helper()
	res := modelrole.New(
		map[string][]string{TaskClassify: {"m/clf"}},
		func(string) string { return modelrole.StatusOnline },
	)
	return NewClient(cfg, config.RouterConfig{},
		fakeEndpoints{models: map[string]string{"m/clf": endpoint}}, res)
}

// A hung-but-listening classifier must not hold the turn for the shared
// 10s http.Client ceiling. This is the case the missing deadline actually
// cost us — connection-refused always failed fast.
func TestClassifyHonoursTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test lets go
	}))
	defer srv.Close()
	defer close(release)

	c := classifyClient(t, srv.URL, config.SidecarConfig{Enabled: true, RequestTimeoutMs: 150})

	start := time.Now()
	out, st := c.ClassifyWithStats(context.Background(), nil, "hello")
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("classify took %s — the deadline did not bite", elapsed)
	}
	if st.Err == nil {
		t.Error("expected an error recorded in stats")
	}
	// A timeout means we learned nothing → cheap default, not max effort.
	if out.Source != classifier.SourceStatic {
		t.Errorf("Source = %q, want %q on timeout", out.Source, classifier.SourceStatic)
	}
	if out.Thinking == classifier.ThinkingHigh && out.MemoryDepth == classifier.MemoryDeep {
		t.Error("a timeout must not resolve to the most expensive configuration")
	}
	if st.Duration <= 0 {
		t.Error("stats must record a duration")
	}
}

// A model that answers but emits junk keeps the conservative fallback —
// that is the one case where erring expensive is defensible.
func TestClassifyUnparsedKeepsConservative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid envelope, unusable levels.
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"thinking\":\"YES PLEASE\",\"memory_depth\":\"none\",\"search_depth\":\"none\"}"}}]}`))
	}))
	defer srv.Close()

	c := classifyClient(t, srv.URL, config.SidecarConfig{Enabled: true})
	out, st := c.ClassifyWithStats(context.Background(), nil, "hi")
	if out.Source != classifier.SourceUnparsed {
		t.Errorf("Source = %q, want %q", out.Source, classifier.SourceUnparsed)
	}
	if out.Thinking != classifier.ThinkingHigh {
		t.Errorf("an answering-but-confused classifier should keep the conservative verdict, got %+v", out)
	}
	if st.Err == nil {
		t.Error("expected the invalid-levels reason in stats")
	}
}

// A good verdict is stamped SourceModel and carries the server's usage.
func TestClassifySuccessRecordsStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"thinking\":\"low\",\"memory_depth\":\"shallow\",\"search_depth\":\"none\"}"}}],"usage":{"prompt_tokens":123,"completion_tokens":9}}`))
	}))
	defer srv.Close()

	c := classifyClient(t, srv.URL, config.SidecarConfig{Enabled: true})
	out, st := c.ClassifyWithStats(context.Background(), nil, "what is 2+2")
	if !out.FromModel() {
		t.Fatalf("expected a model verdict, got source %q", out.Source)
	}
	if out.Thinking != classifier.ThinkingLow {
		t.Errorf("Thinking = %q, want low", out.Thinking)
	}
	if st.InputTokens != 123 || st.OutputTokens != 9 {
		t.Errorf("usage = in %d/out %d, want 123/9 (the server already reports this)", st.InputTokens, st.OutputTokens)
	}
	if st.ModelID != "m/clf" {
		t.Errorf("ModelID = %q, want m/clf", st.ModelID)
	}
}

// The classifier answers a four-field multiple-choice question; it must
// not be made to prefill a pasted document to do it.
//
// Asserts on the DECODED messages, not on a single Body.Read — net/http
// serves that from a 4096-byte buffer, so a length check on it passes
// whether or not the caps exist (this test previously did exactly that
// and could not fail).
func TestClassifyCapsInput(t *testing.T) {
	type gotMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var got []gotMsg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var req struct {
			Messages []gotMsg `json:"messages"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		got = req.Messages
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"thinking\":\"low\",\"memory_depth\":\"none\",\"search_depth\":\"none\"}"}}]}`))
	}))
	defer srv.Close()

	pasted := strings.Repeat("x", 50000)
	// A realistic paste-then-ask turn: the REQUEST is at the end.
	userMsg := pasted + " so what is causing this? look up the latest fix"
	c := classifyClient(t, srv.URL, config.SidecarConfig{Enabled: true})
	c.ClassifyWithStats(context.Background(), []Turn{{Role: "user", Content: pasted}}, userMsg)

	if len(got) < 3 {
		t.Fatalf("expected system + history + user messages, got %d", len(got))
	}
	var history, current *gotMsg
	for i := range got {
		if got[i].Role == "user" {
			if history == nil {
				history = &got[i]
			} else {
				current = &got[i]
			}
		}
	}
	if history == nil || current == nil {
		t.Fatalf("expected two user messages (history + current), got %d", len(got))
	}

	// Each is capped, in runes, at its own budget (plus the marker).
	if n := len([]rune(history.Content)); n > maxClassifyTurnChars+8 {
		t.Errorf("history turn is %d runes, want <= %d — the history cap is not applied",
			n, maxClassifyTurnChars)
	}
	if n := len([]rune(current.Content)); n > maxClassifyUserChars+8 {
		t.Errorf("current message is %d runes, want <= %d — the user cap is not applied",
			n, maxClassifyUserChars)
	}
	if !strings.Contains(current.Content, "[…]") {
		t.Error("expected truncated text to be marked")
	}

	// The regression that matters: head+tail must preserve the trailing
	// request. Head-only truncation would drop it, and because the
	// resulting verdict parses cleanly it would be stamped SourceModel —
	// a silent hard veto on web search that no src=static grep can find.
	if !strings.Contains(current.Content, "look up the latest fix") {
		t.Errorf("trailing request was truncated away; classifier cannot see the intent.\ngot tail: %q",
			lastRunes(current.Content, 80))
	}
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// No classifier configured at all must not mean max effort forever.
func TestClassifyNoModelUsesStaticDefault(t *testing.T) {
	c := NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{}, nil, nil)
	out, st := c.ClassifyWithStats(context.Background(), nil, "hi")
	if out.Source != classifier.SourceStatic {
		t.Errorf("Source = %q, want static", out.Source)
	}
	if st.Err == nil {
		t.Error("expected a recorded reason")
	}
	var nilClient *Client
	if got, _ := nilClient.ClassifyWithStats(context.Background(), nil, "hi"); got.Source != classifier.SourceStatic {
		t.Errorf("nil client Source = %q, want static", got.Source)
	}
}

// The general sidecar request timeout can TIGHTEN the classify deadline
// but must not lift it. The shipped example config sets
// request_timeout_ms = 10000, which is exactly the HTTPRouter's own
// http.Client ceiling — honouring that verbatim would leave the deadline
// buying nothing for anyone who copied the example.
func TestClassifyTimeoutIsClamped(t *testing.T) {
	cases := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{"unset falls to the ceiling", 0, maxClassifyTimeout},
		{"example config is clamped", 10000, maxClassifyTimeout},
		{"absurd value is clamped", 600000, maxClassifyTimeout},
		{"tighter value wins", 1500, 1500 * time.Millisecond},
		{"default 5s wins (under the ceiling)", 5000, 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := &Client{cfg: config.SidecarConfig{RequestTimeoutMs: c.ms}}
			if got := cl.classifyTimeout(); got != c.want {
				t.Errorf("classifyTimeout() = %v, want %v", got, c.want)
			}
		})
	}
	// Must never exceed the router's own client ceiling, or the deadline
	// is decorative.
	if maxClassifyTimeout >= 10*time.Second {
		t.Errorf("maxClassifyTimeout %v must be below the HTTPRouter client ceiling", maxClassifyTimeout)
	}
	var nilClient *Client
	if got := nilClient.classifyTimeout(); got != maxClassifyTimeout {
		t.Errorf("nil client classifyTimeout() = %v, want %v", got, maxClassifyTimeout)
	}
}
