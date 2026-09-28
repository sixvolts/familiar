package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/engine"
	"github.com/familiar/gateway/internal/memevents"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	pb "github.com/familiar/gateway/proto/engine"
)

// extractOnlyRoutes routes the extract task (and nothing else) to one
// endpoint, satisfying both sidecar resolver interfaces so a test can drive a
// real ExtractFacts roundtrip without standing up the whole role table.
type extractOnlyRoutes struct{ endpoint string }

const testExtractModelID = "test/extractor"

func (e extractOnlyRoutes) Resolve(role string) (string, int, bool) {
	if role == sidecar.TaskExtract {
		return testExtractModelID, 0, true
	}
	return "", 0, false
}
func (e extractOnlyRoutes) Status(string) string { return "online" }
func (e extractOnlyRoutes) Chain(role string) []string {
	if role == sidecar.TaskExtract {
		return []string{testExtractModelID}
	}
	return nil
}
func (e extractOnlyRoutes) EndpointForModel(id string) string {
	if id == testExtractModelID {
		return e.endpoint
	}
	return ""
}

// When the post-turn extractor's cheap dedup gate fires — a candidate is
// essentially identical to its nearest live neighbour — the survivor must be
// reinforced (last_accessed/access_count bumped on that row) rather than the
// restatement being silently dropped with no trace. This pins the wiring from
// the gate through to MemoryStore.ReinforceFacts.
func TestPostTurnExtract_ReinforcesCheapGateDuplicate(t *testing.T) {
	// Extractor returns exactly one candidate fact.
	extractSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant",
					"content": `{"facts":[{"content":"gpu-host has 64GB RAM","category":"configuration"}],"relationships":[]}`}},
			},
		})
	}))
	defer extractSrv.Close()

	// The nearest live fact is an all-but-identical match (0.99 > the 0.92
	// cheap-gate floor), so the candidate is skipped and its target reinforced.
	store := &recordingStore{
		nearestLive: []memory.NearestFact{{ID: "dup-target-id", Content: "gpu-host has 64GB RAM", Similarity: 0.99}},
	}
	embed := func(context.Context, string) ([]float32, error) { return []float32{0.1, 0.2, 0.3}, nil }

	rtr := router.NewRouter(config.RouterConfig{Enabled: true}, router.NewRegistry(nil))
	pl := New(Deps{
		Engine:      &mockEngine{},
		Router:      rtr,
		Sessions:    session.NewManager(),
		AgentID:     "test-agent",
		MemoryStore: store,
		Embedder:    embed,
	})
	routes := extractOnlyRoutes{endpoint: extractSrv.URL}
	pl.sidecarClient = sidecar.NewClient(config.SidecarConfig{Enabled: true}, routes, routes)

	sess := pl.sessions.GetOrCreate("cli", "user1")
	pl.runPostTurnExtract(sess, "bump gpu-host to 64GB", "done", nil, nil, nil)

	if len(store.reinforced) != 1 || store.reinforced[0] != "dup-target-id" {
		t.Fatalf("cheap-gate duplicate must reinforce its survivor: got reinforced=%v, want [dup-target-id]", store.reinforced)
	}
}

// restatingEngine reports every committed fact as landing on an
// existing row, the way CommitFacts does for a restated fact.
type restatingEngine struct{ mockEngine }

func (e *restatingEngine) CommitFacts(ctx context.Context, sessionID string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error) {
	resp, err := e.mockEngine.CommitFacts(ctx, sessionID, facts)
	if err == nil {
		for _, f := range facts {
			f.Id = "stored-row"
		}
	}
	return resp, err
}

// The extraction events name the row a fact was stored in, and only go
// out once it was stored: they used to carry the generated id (which a
// restatement never creates) and fire before the commit, even one that
// then failed.
func TestPostTurnExtract_EventsNameStoredRow(t *testing.T) {
	extractSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant",
					"content": `{"facts":[{"content":"User lives in Portland","category":"personal"}],"relationships":[]}`}},
			},
		})
	}))
	defer extractSrv.Close()

	run := func(t *testing.T, eng engine.Service) []memevents.Event {
		t.Helper()
		bus := memevents.NewBus(64, nil)
		pl := New(Deps{
			Engine:      eng,
			Router:      router.NewRouter(config.RouterConfig{Enabled: true}, router.NewRegistry(nil)),
			Sessions:    session.NewManager(),
			AgentID:     "test-agent",
			MemoryStore: &recordingStore{},
			Embedder:    func(context.Context, string) ([]float32, error) { return []float32{0.1, 0.2, 0.3}, nil },
			Events:      bus,
		})
		routes := extractOnlyRoutes{endpoint: extractSrv.URL}
		pl.sidecarClient = sidecar.NewClient(config.SidecarConfig{Enabled: true}, routes, routes)
		sess := pl.sessions.GetOrCreate("cli", "user1")
		pl.runPostTurnExtract(sess, "I moved back to Portland", "welcome back", nil, nil, nil)
		var extracted []memevents.Event
		for _, ev := range bus.Replay(sess.ID, 0) {
			if ev.Kind == memevents.KindFactExtracted {
				extracted = append(extracted, ev)
			}
		}
		return extracted
	}

	t.Run("stored id", func(t *testing.T) {
		evs := run(t, &restatingEngine{})
		if len(evs) != 1 {
			t.Fatalf("want 1 fact_extracted event, got %d", len(evs))
		}
		var p memevents.FactExtractedPayload
		if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.FactID != "stored-row" {
			t.Errorf("event names fact %q, want the row it was stored in", p.FactID)
		}
	})
	t.Run("failed commit", func(t *testing.T) {
		if evs := run(t, &mockEngine{commitErr: errors.New("db down")}); len(evs) != 0 {
			t.Errorf("announced %d extracted fact(s) that were never stored", len(evs))
		}
	})
}

// classifyRoutes resolves the classify task to one model on endpoint.
type classifyRoutes struct{ endpoint string }

func (c classifyRoutes) Resolve(role string) (string, int, bool) {
	if role == sidecar.TaskClassify {
		return "sidecar/fast-model", 0, true
	}
	return "", 0, false
}
func (c classifyRoutes) Status(string) string           { return "online" }
func (c classifyRoutes) Chain(string) []string          { return []string{"sidecar/fast-model"} }
func (c classifyRoutes) EndpointForModel(string) string { return c.endpoint }

// The preamble follows the classify role (so its failover) and names
// the model it reaches. It used to post to the endpoint captured at boot
// with the model "sidecar", whatever was serving.
func TestPreamble_UsesClassifyRoleTarget(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"On it.\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	pl := New(Deps{
		Engine:          &mockEngine{},
		Router:          router.NewRouter(config.RouterConfig{Enabled: true}, router.NewRegistry(nil)),
		Sessions:        session.NewManager(),
		AgentID:         "test-agent",
		SidecarEndpoint: "http://127.0.0.1:1", // captured at boot; gone since
	})
	routes := classifyRoutes{endpoint: srv.URL}
	pl.sidecarClient = sidecar.NewClient(config.SidecarConfig{Enabled: true}, routes, routes)

	got := pl.generatePreamble(context.Background(), "summarize my notes", "standard", func(string) {})
	if !strings.Contains(got, "On it.") {
		t.Fatalf("preamble = %q; it didn't reach the classify model", got)
	}
	if gotModel != "fast-model" {
		t.Errorf("preamble model = %q, want the classify model's request name", gotModel)
	}
}

// offlineClassifyRoutes is classifyRoutes with the model offline.
type offlineClassifyRoutes struct{ classifyRoutes }

func (offlineClassifyRoutes) Status(string) string { return "offline" }

// With the classify chain offline there's no preamble. It fell back to
// the endpoint captured at boot, stalling each thinking=high turn up to
// 15s on a host that was down.
func TestPreamble_SkippedWhenClassifyIsOffline(t *testing.T) {
	hits := 0
	boot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer boot.Close()
	pl := New(Deps{
		Engine:          &mockEngine{},
		Router:          router.NewRouter(config.RouterConfig{Enabled: true}, router.NewRegistry(nil)),
		Sessions:        session.NewManager(),
		AgentID:         "test-agent",
		SidecarEndpoint: boot.URL,
	})
	routes := offlineClassifyRoutes{classifyRoutes{endpoint: boot.URL}}
	pl.sidecarClient = sidecar.NewClient(config.SidecarConfig{Enabled: true}, routes, routes)
	if got := pl.generatePreamble(context.Background(), "q", "standard", func(string) {}); got != "" || hits != 0 {
		t.Errorf("preamble %q with %d request(s) while classify is offline, want none", got, hits)
	}
}
