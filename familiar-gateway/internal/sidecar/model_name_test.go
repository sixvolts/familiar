package sidecar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/modelrole"
)

// modelRecorder is a chat-completions server that records the `model`
// each request names and answers with JSON both the classifier and the
// fact extractor accept.
type modelRecorder struct {
	mu     sync.Mutex
	models []string
}

func (m *modelRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.models = append(m.models, body.Model)
		m.mu.Unlock()
		content := `{"thinking":"low","memory_depth":"shallow","search_depth":"none","condensed_query":"q","facts":[],"relationships":[]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (m *modelRecorder) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.models...)
}

// namedEndpoints adds configured request names, as *router.Registry has.
type namedEndpoints struct {
	fakeEndpoints
	names map[string]string
}

func (n namedEndpoints) RequestModelFor(id string) string {
	if name, ok := n.names[id]; ok {
		return name
	}
	return config.StripModelNamespace(id)
}

// A task that fails over to its backup must name the backup. Every
// sidecar request used to send "gemma-4-26b-a4b", so a failover to an
// ollama or vLLM backup asked it for a model it doesn't have, and the
// task stayed broken for the whole outage.
func TestSidecar_FailoverSendsBackupModelName(t *testing.T) {
	rec := &modelRecorder{}
	srv := rec.server(t)
	chain := []string{"sidecar/gemma-4-26b-a4b", "ollama/qwen3:8b"}
	c := newTestClient(
		map[string][]string{TaskClassify: chain, TaskExtract: chain},
		map[string]string{chain[0]: modelrole.StatusOffline, chain[1]: modelrole.StatusOnline},
		map[string]string{chain[0]: srv.URL, chain[1]: srv.URL},
	)
	if out, st := c.ClassifyWithStats(context.Background(), nil, "look up the latest Go release"); st.Err != nil {
		t.Fatalf("classify: %v (%+v)", st.Err, out)
	}
	if _, err := c.ExtractFacts(context.Background(), []Turn{{Role: "user", Content: "My dentist is Dr. Alvarez."}}); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got := rec.seen()
	if len(got) != 2 || got[0] != "qwen3:8b" || got[1] != "qwen3:8b" {
		t.Errorf("request models = %q, want the backup's name on both classify and extract", got)
	}
}

// A model's configured `model` is what its requests send.
func TestSidecar_SendsConfiguredModelName(t *testing.T) {
	rec := &modelRecorder{}
	srv := rec.server(t)
	res := modelrole.New(map[string][]string{TaskExtract: {"sidecar/gemma"}},
		func(string) string { return modelrole.StatusOnline })
	c := NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{},
		namedEndpoints{fakeEndpoints{map[string]string{"sidecar/gemma": srv.URL}},
			map[string]string{"sidecar/gemma": "gemma-4-26b-a4b"}}, res)
	if _, err := c.ExtractFacts(context.Background(), []Turn{{Role: "user", Content: "My dentist is Dr. Alvarez."}}); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := rec.seen(); len(got) != 1 || got[0] != "gemma-4-26b-a4b" {
		t.Errorf("request model = %q, want the configured name", got)
	}
}

// [sidecar].request_timeout_ms is the request ceiling for the sidecar
// tasks, as documented; it used to reach only the classifier. The
// large-document route keeps its own 5-minute ceiling.
func TestSidecar_RequestTimeoutApplies(t *testing.T) {
	chains := map[string][]string{TaskExtract: {"fast"}, TaskExtractLarge: {"big"}}
	health := map[string]string{"fast": modelrole.StatusOnline, "big": modelrole.StatusOnline}
	eps := map[string]string{"fast": "http://127.0.0.1:8400", "big": "http://127.0.0.1:8500"}

	c := NewClient(config.SidecarConfig{Enabled: true, RequestTimeoutMs: 45000}, config.RouterConfig{},
		fakeEndpoints{models: eps}, modelrole.New(chains, func(id string) string { return health[id] }))
	if got := c.routerFor(TaskExtract).client.Timeout; got != 45*time.Second {
		t.Errorf("extract timeout = %v, want request_timeout_ms (45s)", got)
	}
	if got := c.routerFor(TaskExtractLarge).client.Timeout; got != LargeExtractTimeout {
		t.Errorf("extract_large timeout = %v, want %v whatever request_timeout_ms says", got, LargeExtractTimeout)
	}
	d := newTestClient(chains, health, eps)
	if got := d.routerFor(TaskExtract).client.Timeout; got != defaultTaskTimeout {
		t.Errorf("default extract timeout = %v, want %v", got, defaultTaskTimeout)
	}
}

// A primary and backup on one server: after the primary has served,
// failing over must switch the name too (the router cache is per model,
// not just per endpoint).
func TestSidecar_FailoverOnSameServerSwitchesName(t *testing.T) {
	rec := &modelRecorder{}
	srv := rec.server(t)
	chain := []string{"sidecar/gemma", "sidecar/qwen3"}
	var mu sync.Mutex
	health := map[string]string{chain[0]: modelrole.StatusOnline, chain[1]: modelrole.StatusOnline}
	res := modelrole.New(map[string][]string{TaskExtract: chain}, func(id string) string {
		mu.Lock()
		defer mu.Unlock()
		return health[id]
	})
	c := NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{},
		fakeEndpoints{models: map[string]string{chain[0]: srv.URL, chain[1]: srv.URL}}, res)
	turn := []Turn{{Role: "user", Content: "My dentist is Dr. Alvarez."}}
	if _, err := c.ExtractFacts(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	health[chain[0]] = modelrole.StatusOffline
	mu.Unlock()
	if _, err := c.ExtractFacts(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if got := rec.seen(); len(got) != 2 || got[0] != "gemma" || got[1] != "qwen3" {
		t.Errorf("request models = %q, want gemma then qwen3", got)
	}
}
