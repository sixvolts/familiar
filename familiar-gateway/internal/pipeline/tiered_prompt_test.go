package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/ctxbuild"
	"github.com/familiar/gateway/internal/memory"
)

// recordingMemStore is a memory.MemoryStore that records every Search call
// and returns a canned result list. It's deliberately dumb: it does not
// filter on the query vector, so the "partial-context trap" has to be
// simulated by callers that set up results with gaps in them.
type recordingMemStore struct {
	mu       sync.Mutex
	results  []memory.MemoryResult
	searches []recordedSearch
}

type recordedSearch struct {
	limit     int
	threshold float64
	userID    string
}

func (m *recordingMemStore) Search(ctx context.Context, vector []float32, limit int, threshold float64, userID string) ([]memory.MemoryResult, error) {
	m.mu.Lock()
	m.searches = append(m.searches, recordedSearch{limit: limit, threshold: threshold, userID: userID})
	out := append([]memory.MemoryResult(nil), m.results...)
	m.mu.Unlock()
	return out, nil
}

// HybridSearch delegates to Search — the recording mock doesn't model
// FTS; the live pipeline retrieval path calls HybridSearch.
func (m *recordingMemStore) HybridSearch(ctx context.Context, _ string, vector []float32, limit int, threshold float64, userID string) ([]memory.MemoryResult, error) {
	return m.Search(ctx, vector, limit, threshold, userID)
}

func (m *recordingMemStore) NearestSimilarity(ctx context.Context, vector []float32, scope string, userID string) (float64, bool, error) {
	return 0, false, nil
}

func (m *recordingMemStore) NearestLiveFact(ctx context.Context, vector []float32, userID string) (memory.NearestFact, bool, error) {
	return memory.NearestFact{}, false, nil
}

func (m *recordingMemStore) Close() error { return nil }

// capturedRequest holds the system prompt the tier-prompt assertions
// inspect. In the OpenAI chat shape the system prompt is the
// role:"system" message (flattenAssembled emits a single one), so the
// recording server pulls it out of the messages array.
type capturedRequest struct {
	System string
}

// fakeOpenAIRecordingServer is like fakeOpenAIServer but records the
// system prompt from the last request for assertions.
func fakeOpenAIRecordingServer(responseText string) (*httptest.Server, *capturedRequest) {
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			var body struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, m := range body.Messages {
				if m.Role == "system" {
					captured.System = m.Content
					break
				}
			}
			resp := map[string]interface{}{
				"id":     "chatcmpl_test",
				"object": "chat.completion",
				"model":  "test-model",
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"message":       map[string]string{"role": "assistant", "content": responseText},
						"finish_reason": "stop",
					},
				},
				"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return srv, captured
}

// writeTierPromptFixtures creates a minimal ~/.familiar/prompts-shaped dir
// with distinctive strings in each file so assertions can tell which
// overlay was selected. Returns the directory path.
func writeTierPromptFixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"base.md":           "BASE_IDENTITY_MARKER",
		"tier_trivial.md":   "TRIVIAL_OVERLAY_MARKER",
		"tier_knowledge.md": "KNOWLEDGE_OVERLAY_MARKER incomplete sample memory_search",
		"tier_reasoning.md": "REASONING_OVERLAY_MARKER",
		"tier_deep.md":      "DEEP_OVERLAY_MARKER",
		"tool_policy.md":    "TOOL_POLICY_MARKER",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// TestTierDeepInjectsOverlayAndToolPolicy locks in the tier wiring: when
// a request routes to a non-trivial tier, the assembled system prompt
// must include the base identity, that tier's overlay, and the tool
// policy, and the pgvector search must honor the tier's memory config.
//
// The test pipeline has no sidecar, so classifyRequest falls back to
// classifier.ConservativeFallback() — thinking=high — which resolves to
// the deep tier. (Pre-fix this silently became "knowledge": complexity
// "deep" missed the tiers-table key "deep_reasoning" and TierFor fell
// back. The classifier→tier mapping now emits real keys.)
func TestTierDeepInjectsOverlayAndToolPolicy(t *testing.T) {
	dir := writeTierPromptFixtures(t)
	store, err := ctxbuild.NewPromptStore(dir, "FALLBACK_SHOULD_NOT_APPEAR")
	if err != nil {
		t.Fatalf("NewPromptStore: %v", err)
	}
	if !store.Loaded() {
		t.Fatal("prompt store should be loaded from fixture dir")
	}

	srv, captured := fakeOpenAIRecordingServer("ok")
	defer srv.Close()

	eng := &mockEngine{}
	pl := makePipeline(eng, srv)
	pl.promptStore = store

	// Seed a recording memstore and point the pipeline at it. Give it a
	// fake embedder so the pgvector search path actually runs.
	rec := &recordingMemStore{
		results: []memory.MemoryResult{
			{Content: "gpu-host runs 6x GPUs", Similarity: 0.81},
			{Content: "gpu-host has an Optane 905P scratch drive", Similarity: 0.72},
		},
	}
	pl.memStore = rec
	pl.memoryCfg = config.MemoryConfig{MaxInjected: 5, RelevanceThreshold: 0.55}
	pl.embedder = func(ctx context.Context, text string) ([]float32, error) {
		return []float32{0.1, 0.2, 0.3}, nil
	}

	sess := pl.sessions.GetOrCreate("cli", "user1")
	_, info, err := pl.Handle(context.Background(), sess, "What is gpu-host and what is its IP address?", nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// No sidecar → ConservativeFallback (thinking=high) → deep tier.
	// If the classifier→tier mapping regresses, this catches it before
	// the rest of the test runs.
	if info.Tier.Name != "deep" {
		t.Fatalf("expected deep tier, got %q", info.Tier.Name)
	}

	// System prompt must contain base + the deep overlay + tool policy.
	for _, want := range []string{"BASE_IDENTITY_MARKER", "DEEP_OVERLAY_MARKER", "TOOL_POLICY_MARKER"} {
		if !strings.Contains(captured.System, want) {
			t.Errorf("system prompt missing %q; got: %q", want, captured.System)
		}
	}
	// Fallback must NOT leak through when the store loaded successfully.
	if strings.Contains(captured.System, "FALLBACK_SHOULD_NOT_APPEAR") {
		t.Errorf("fallback leaked into system prompt: %q", captured.System)
	}

	// Memory store was called at least once with a vector (embedder
	// supplies one). With CHAT-REARCH the effort resolver is now the
	// authority on memory budgets — the test pipeline has no sidecar,
	// so classifyRequest returns classifier.ConservativeFallback() which
	// pins MemoryDepth=deep, resolving to TopK=20 / threshold=0.40 from
	// the spec defaults.
	if len(rec.searches) == 0 {
		t.Fatal("expected pgvector Search to be called")
	}
	first := rec.searches[0]
	if first.limit != 20 {
		t.Errorf("limit = %d, want 20 (effort.memory.deep default)", first.limit)
	}
	if first.threshold != 0.40 {
		t.Errorf("threshold = %v, want 0.40 (effort.memory.deep default)", first.threshold)
	}
}

// TestTierReasoningOverridesMemoryConfig verifies that the reasoning tier
// widens the memory retrieval net per its TierMemoryConfig (threshold
// 0.45, max_results 10), overriding the global MemoryConfig values.
func TestTierReasoningOverridesMemoryConfig(t *testing.T) {
	// Drive this via searchPgVector directly so we don't have to force the
	// router into a non-fallback classification. That's the unit under
	// test anyway — Handle is covered by the knowledge-tier case above.
	srv, _ := fakeOpenAIRecordingServer("ok")
	defer srv.Close()

	pl := makePipeline(&mockEngine{}, srv)
	rec := &recordingMemStore{}
	pl.memStore = rec
	pl.memoryCfg = config.MemoryConfig{MaxInjected: 5, RelevanceThreshold: 0.55}

	reasoningTier := ctxbuild.TierFor("analytical")
	limit := pl.memoryCfg.MaxInjected
	if reasoningTier.MemoryConfig.MaxResults > 0 {
		limit = reasoningTier.MemoryConfig.MaxResults
	}
	threshold := pl.memoryCfg.RelevanceThreshold
	if reasoningTier.MemoryConfig.Threshold > 0 {
		threshold = reasoningTier.MemoryConfig.Threshold
	}

	_ = pl.searchPgVector(
		context.Background(),
		"test-user",
		"why does thing X happen",
		[]float32{0.1, 0.2, 0.3},
		reasoningTier,
		limit,
		threshold,
		nil,
	)

	if len(rec.searches) == 0 {
		t.Fatal("expected Search to be called")
	}
	if rec.searches[0].limit != 10 {
		t.Errorf("limit = %d, want 10 (reasoning override)", rec.searches[0].limit)
	}
	if rec.searches[0].threshold != 0.45 {
		t.Errorf("threshold = %v, want 0.45 (reasoning override)", rec.searches[0].threshold)
	}
}
