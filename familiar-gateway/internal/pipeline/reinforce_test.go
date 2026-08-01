package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
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
func (e extractOnlyRoutes) EndpointForRole(string) string { return "" }
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
	pl.sidecarClient = sidecar.NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{}, routes, routes)

	sess := pl.sessions.GetOrCreate("cli", "user1")
	pl.runPostTurnExtract(sess, "bump gpu-host to 64GB", "done", nil, nil)

	if len(store.reinforced) != 1 || store.reinforced[0] != "dup-target-id" {
		t.Fatalf("cheap-gate duplicate must reinforce its survivor: got reinforced=%v, want [dup-target-id]", store.reinforced)
	}
}
