package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	pb "github.com/familiar/gateway/proto/engine"
)

// ── Mock engine ──────────────────────────────────────────────

type mockEngine struct {
	assembleResp *pb.AssembleContextResponse
	assembleErr  error
	commitResp   *pb.CommitFactsResponse
	commitErr    error

	// Capture calls
	commitCalled bool
	commitFacts  []*pb.FactProto
}

func (m *mockEngine) Ping(ctx context.Context) (*pb.PingResponse, error) {
	return &pb.PingResponse{Version: "test", UptimeSecs: 1, MemoryTier: "ram_only"}, nil
}
func (m *mockEngine) AssembleContext(ctx context.Context, sessionID, userMsg string, vis *pb.VisibilityContext, memBudget, convBudget uint32, queryVec []float32) (*pb.AssembleContextResponse, error) {
	if m.assembleErr != nil {
		return nil, m.assembleErr
	}
	if m.assembleResp != nil {
		return m.assembleResp, nil
	}
	return &pb.AssembleContextResponse{}, nil
}
func (m *mockEngine) CommitFacts(ctx context.Context, sessionID string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error) {
	m.commitCalled = true
	m.commitFacts = facts
	if m.commitErr != nil {
		return nil, m.commitErr
	}
	if m.commitResp != nil {
		return m.commitResp, nil
	}
	return &pb.CommitFactsResponse{Committed: uint32(len(facts))}, nil
}
func (m *mockEngine) QueryMemory(ctx context.Context, req *pb.MemoryQueryRequest) (*pb.MemoryQueryResponse, error) {
	return &pb.MemoryQueryResponse{}, nil
}
func (m *mockEngine) DeleteFact(ctx context.Context, sessionID, factID string, vis *pb.VisibilityContext) (*pb.DeleteFactResponse, error) {
	return &pb.DeleteFactResponse{}, nil
}
func (m *mockEngine) UpdateFact(ctx context.Context, sessionID, factID, newContent string, newEmbedding []float32, vis *pb.VisibilityContext) (*pb.UpdateFactResponse, error) {
	return &pb.UpdateFactResponse{}, nil
}
func (m *mockEngine) VaultGet(ctx context.Context, key string) (string, bool, error) {
	return "", false, nil
}
func (m *mockEngine) VaultSet(ctx context.Context, key, value string) error { return nil }
func (m *mockEngine) GetAgentIdentity(ctx context.Context) (*pb.AgentIdentityResponse, error) {
	return &pb.AgentIdentityResponse{AgentId: "test-agent"}, nil
}
func (m *mockEngine) GetBriefing(ctx context.Context) (*pb.BriefingResponse, error) {
	return &pb.BriefingResponse{}, nil
}
func (m *mockEngine) StartSleep(ctx context.Context, phases []string) (string, error) {
	return "handle-1", nil
}
func (m *mockEngine) SleepStatus(ctx context.Context, handle string) (*pb.SleepStatusResponse, error) {
	return &pb.SleepStatusResponse{}, nil
}
func (m *mockEngine) WakeSleep(ctx context.Context, handle string) error { return nil }
func (m *mockEngine) Close() error                                       { return nil }

// ── Fake OpenAI-compatible server ─────────────────────────────
//
// Local-first: the gateway talks to llama-server's OpenAI-compatible
// /v1/chat/completions endpoint, so the test LLM mimics that shape.

// fakeOpenAIServer returns an httptest.Server that mimics the
// OpenAI chat-completions API. responseText is returned in every
// response.
func fakeOpenAIServer(responseText string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
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
				"usage": map[string]int{
					"prompt_tokens": 10, "completion_tokens": 5,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		// /v1/models health check or unknown — return 200.
		w.WriteHeader(http.StatusOK)
	}))
}

// fakeOpenAIServerError returns a server that returns an HTTP error.
func fakeOpenAIServerError(statusCode int, msg string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, msg, statusCode)
	}))
}

// ── Helpers ──────────────────────────────────────────────────

func makePipeline(eng *mockEngine, fakeServer *httptest.Server) *Pipeline {
	models := []config.ModelConfig{
		{ID: "test-model", Provider: "openai", Endpoint: fakeServer.URL},
	}
	reg := router.NewRegistry(models)
	reg.SetStatusForTest("test-model", "online")
	rtr := router.NewRouter(config.RouterConfig{
		Enabled: true,
	}, reg)

	sm := session.NewManager()
	return New(Deps{
		Engine:   eng,
		Router:   rtr,
		Sessions: sm,
		AgentID:  "test-agent",
	})
}

// ── Tests ────────────────────────────────────────────────────

func TestPipelineHandleBasic(t *testing.T) {
	srv := fakeOpenAIServer("hello back")
	defer srv.Close()

	eng := &mockEngine{}
	pl := makePipeline(eng, srv)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	resp, info, err := pl.Handle(context.Background(), sess, "hello", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "hello back" {
		t.Fatalf("expected 'hello back', got %q", resp)
	}
	if info.ModelID != "test-model" {
		t.Fatalf("expected test-model, got %q", info.ModelID)
	}
}

func TestPipelineHandleWithMemory(t *testing.T) {
	srv := fakeOpenAIServer("I know you like brisket")
	defer srv.Close()

	eng := &mockEngine{
		assembleResp: &pb.AssembleContextResponse{
			MemoryContext: []*pb.MemoryResultProto{
				{
					Fact:      &pb.FactProto{Content: "user likes brisket"},
					Staleness: "fresh",
				},
			},
		},
	}
	pl := makePipeline(eng, srv)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	_, info, err := pl.Handle(context.Background(), sess, "what food do I like?", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.MemHits != 1 {
		t.Fatalf("expected 1 memory hit, got %d", info.MemHits)
	}
}

func TestPipelineHandleProviderError(t *testing.T) {
	srv := fakeOpenAIServerError(http.StatusTooManyRequests, "rate limited")
	defer srv.Close()

	eng := &mockEngine{}
	pl := makePipeline(eng, srv)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	_, _, err := pl.Handle(context.Background(), sess, "hello", nil)
	if err == nil {
		t.Fatal("expected error from provider")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 error, got: %v", err)
	}
}

func TestPipelineHandleCommitFacts(t *testing.T) {
	srv := fakeOpenAIServer("response")
	defer srv.Close()

	eng := &mockEngine{}
	pl := makePipeline(eng, srv)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	_, _, err := pl.Handle(context.Background(), sess, "remember this", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !eng.commitCalled {
		t.Fatal("expected CommitFacts to be called")
	}
	if len(eng.commitFacts) != 1 {
		t.Fatalf("expected 1 fact committed, got %d", len(eng.commitFacts))
	}
	if !strings.Contains(eng.commitFacts[0].Content, "remember this") {
		t.Fatal("committed fact should contain user message")
	}
}

func TestPipelineSessionTurnsUpdated(t *testing.T) {
	srv := fakeOpenAIServer("hi there")
	defer srv.Close()

	eng := &mockEngine{}
	pl := makePipeline(eng, srv)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	_, _, err := pl.Handle(context.Background(), sess, "hello", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	turns := sess.RecentTurns(0)
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns (user+assistant), got %d", len(turns))
	}
	if turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("unexpected turn roles: %s, %s", turns[0].Role, turns[1].Role)
	}
}

func TestPipelineHandleAssembleError(t *testing.T) {
	srv := fakeOpenAIServer("still works")
	defer srv.Close()

	eng := &mockEngine{
		assembleErr: fmt.Errorf("engine down"),
	}
	pl := makePipeline(eng, srv)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	// Pipeline should continue even when AssembleContext fails
	resp, _, err := pl.Handle(context.Background(), sess, "hello", nil)
	if err != nil {
		t.Fatalf("expected no error (graceful degradation), got: %v", err)
	}
	if resp != "still works" {
		t.Fatalf("expected 'still works', got %q", resp)
	}
}

func TestModelIDToProviderModel(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"llama-server/qwen3-30b", "qwen3-30b"},
		{"openai/gpt-4", "gpt-4"},
		{"local-model", "local-model"},
		{"a/b/c", "b/c"},
	}
	for _, tc := range cases {
		got := modelIDToProviderModel(tc.input)
		if got != tc.want {
			t.Errorf("modelIDToProviderModel(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestPipelineEmptyResponseNotCommitted is a regression test for Bug 1:
// the gateway used to persist empty assistant turns into the session
// buffer AND the memories table. The next request in the same session
// would then send `{"role":"assistant"}` (no content, no tool_calls) to
// llama-server, which correctly rejected it with HTTP 400, which the
// gateway surfaced as a 500 to the caller. One transient empty response
// would poison the session permanently until the gateway was restarted.
//
// commitAndExtract now early-returns on empty responseText: no
// CommitFacts call, no sess.AddTurn, no maybeSummarize. The bad turn
// vanishes instead of poisoning future requests.
func TestPipelineEmptyResponseNotCommitted(t *testing.T) {
	// Fake provider returns empty content, simulating Qwen exhausting
	// max_tokens on reasoning or returning `{"role":"assistant"}`.
	srv := fakeOpenAIServer("")
	defer srv.Close()

	eng := &mockEngine{}
	pl := makePipeline(eng, srv)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	resp, _, err := pl.Handle(context.Background(), sess, "extract this URL", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Caller still gets the empty response back — that's honest about
	// what happened. It's the PERSISTENCE we block, not the return.
	if resp != "" {
		t.Fatalf("expected empty response returned to caller, got %q", resp)
	}
	// The critical invariants: nothing written to memories, nothing
	// added to the session turn buffer.
	if eng.commitCalled {
		t.Error("CommitFacts must NOT be called for empty responses (Bug 1 regression)")
	}
	if len(sess.RecentTurns(0)) != 0 {
		t.Errorf("session buffer must stay empty after empty response, got %d turns (Bug 1 regression)",
			len(sess.RecentTurns(0)))
	}
}

// An unresolved identity must NOT reach the post-turn extract path:
// pgvector + the relationships table treat an empty user_id as
// globally visible, so extracting facts/edges for an identity-less
// session would leak across tenants. The guard returns before the
// sidecar extractor runs — with a nil sidecarClient here, the absence
// of a panic AND of any CommitFacts proves the early return (removing
// the guard makes this test panic on the nil sidecar deref).
func TestPostTurnExtract_SkipsWhenNoIdentity(t *testing.T) {
	srv := fakeOpenAIServer("ok")
	defer srv.Close()
	eng := &mockEngine{}
	pl := makePipeline(eng, srv)

	// SenderID "" and no canonical id → UserID() == "".
	sess := pl.sessions.GetOrCreate("cli", "")
	if sess.UserID() != "" {
		t.Fatalf("precondition: expected empty UserID, got %q", sess.UserID())
	}

	pl.runPostTurnExtract(sess, "remember my api key is sk-123", "noted", nil, nil)

	if eng.commitCalled {
		t.Error("extracted/committed facts for an unresolved-identity session (tenant-leak guard removed?)")
	}
}

// Tier-prefix override regression tests (TestRouteRequest_Tier3*,
// _Tier4*) were removed in CHAT-REARCH S1.3 along with the
// route_override session-meta path. Behavior they pinned —
// "thinking off for tier 3, on for tier 4" — moves to the new
// classifier's effort-level output in a later sprint.
