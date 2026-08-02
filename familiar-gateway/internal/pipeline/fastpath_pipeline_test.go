package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/sidecar"
)

// A trivial turn ("thanks") must be recognized by the deterministic gate and
// skip the classifier round-trip entirely, yielding the off/none/none
// fast-path verdict; a real question must fall through and hit the model.
func TestClassifyRequest_TrivialFastPathSkipsClassifier(t *testing.T) {
	chatSrv := fakeOpenAIServer("ok")
	defer chatSrv.Close()
	pl := makePipeline(&mockEngine{}, chatSrv)

	var classifyHits int32
	classSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			atomic.AddInt32(&classifyHits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant",
					"content": `{"thinking":"high","memory_depth":"deep","search_depth":"deep","condensed_query":""}`}},
			},
		})
	}))
	defer classSrv.Close()
	routes := classifyOnlyRoutes{endpoint: classSrv.URL}
	pl.sidecarClient = sidecar.NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{}, routes, routes)

	sess := pl.sessions.GetOrCreate("cli", "user1")

	// Trivial: must NOT touch the classifier, must get the fast-path verdict.
	route, err := pl.classifyRequest(context.Background(), sess, "thanks", nil, nil)
	if err != nil {
		t.Fatalf("classifyRequest(trivial): %v", err)
	}
	if n := atomic.LoadInt32(&classifyHits); n != 0 {
		t.Errorf("trivial turn hit the classifier %d time(s); want 0 (fast-path should short-circuit)", n)
	}
	if route.classifier.Source != classifier.SourceFastPath {
		t.Errorf("source = %q, want %q", route.classifier.Source, classifier.SourceFastPath)
	}
	if route.complexityLabel() != "trivial" {
		t.Errorf("tier = %q, want trivial", route.complexityLabel())
	}
	if route.classifier.MemoryDepth != classifier.MemoryNone {
		t.Errorf("memory = %q, want none (fast-path skips retrieval)", route.classifier.MemoryDepth)
	}
	if route.classifier.SearchDepth != classifier.SearchNone {
		t.Errorf("search = %q, want none (fast-path must not permit web search)", route.classifier.SearchDepth)
	}

	// Control: a real question MUST fall through and hit the classifier once.
	if _, err := pl.classifyRequest(context.Background(), sess, "what OS does gpu-host run", nil, nil); err != nil {
		t.Fatalf("classifyRequest(control): %v", err)
	}
	if n := atomic.LoadInt32(&classifyHits); n != 1 {
		t.Errorf("non-trivial turn should hit the classifier exactly once; got %d", n)
	}
}
