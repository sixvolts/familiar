package sidecar

import (
	"context"
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
func TestClassifyCapsInput(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<20)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"thinking\":\"low\",\"memory_depth\":\"none\",\"search_depth\":\"none\"}"}}]}`))
	}))
	defer srv.Close()

	huge := strings.Repeat("x", 50000)
	c := classifyClient(t, srv.URL, config.SidecarConfig{Enabled: true})
	c.ClassifyWithStats(context.Background(), []Turn{{Role: "user", Content: huge}}, huge)

	if len(gotBody) > 20000 {
		t.Errorf("classify request body is %d bytes for a 100KB input — caps are not applied", len(gotBody))
	}
	if !strings.Contains(gotBody, "[…]") {
		t.Error("expected truncated text to be marked")
	}
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
