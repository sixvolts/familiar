package pipeline

// Integration-style tests that drive the full Pipeline against a
// MockLLM speaking the OpenAI chat-completions wire format. Unlike
// pipeline_test.go (which uses a simpler fakeOpenAIServer), these
// tests script exact LLM replies — including tool_calls — so we can
// assert multi-turn behaviour: a trivial answer, a tool-call round
// trip, and (skipped until Phase C) per-user memory isolation.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/llm"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
)

// ── helpers ──────────────────────────────────────────────────

// recordingStore is a memory.MemoryStore fake used only by the
// multi-user isolation test. It records every (userID, vector) pair
// Search was called with, and returns a canned result bound to the
// userID argument so the test can assert that the pipeline never
// surfaces one user's results to another.
type recordingStore struct {
	mu     sync.Mutex
	byUser map[string][]memory.MemoryResult
	calls  []recordingStoreCall
}

type recordingStoreCall struct {
	userID string
}

func newRecordingStore(seed map[string][]memory.MemoryResult) *recordingStore {
	return &recordingStore{byUser: seed}
}

func (r *recordingStore) Search(_ context.Context, _ []float32, _ int, _ float64, userID string) ([]memory.MemoryResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordingStoreCall{userID: userID})
	// Emulate the real SQL predicate: global rows (stored under "")
	// plus the caller's own rows.
	var out []memory.MemoryResult
	out = append(out, r.byUser[""]...)
	if userID != "" {
		out = append(out, r.byUser[userID]...)
	}
	return out, nil
}

// HybridSearch delegates to Search — the recording mock doesn't model
// FTS, so the two paths return the same user-scoped rows. The
// pipeline calls HybridSearch on the live retrieval path.
func (r *recordingStore) HybridSearch(ctx context.Context, _ string, vec []float32, limit int, threshold float64, userID string) ([]memory.MemoryResult, error) {
	return r.Search(ctx, vec, limit, threshold, userID)
}

func (r *recordingStore) NearestSimilarity(_ context.Context, _ []float32, _ string, _ string) (float64, bool, error) {
	return 0, false, nil
}

func (r *recordingStore) NearestLiveFact(_ context.Context, _ []float32, _ string) (memory.NearestFact, bool, error) {
	return memory.NearestFact{}, false, nil
}

func (r *recordingStore) Close() error { return nil }

// makePipelineWithMockLLM wires a Pipeline against an OpenAI-compatible
// MockLLM. The model is registered under the "llama-server" provider so
// the registry builds an OpenAIProvider pointed at mock.URL().
func makePipelineWithMockLLM(eng *mockEngine, mock *testutil.MockLLM, reg *skills.Registry) *Pipeline {
	models := []config.ModelConfig{
		{
			ID:           "mock-model",
			Provider:     "llama-server",
			Endpoint:     mock.URL(),
			Capabilities: []string{"tools"},
		},
	}
	rr := router.NewRegistry(models)
	rr.SetStatusForTest("mock-model", "online")
	rtr := router.NewRouter(config.RouterConfig{
		Enabled: true,
	}, rr)

	return New(Deps{
		Engine:        eng,
		Router:        rtr,
		Sessions:      session.NewManager(),
		AgentID:       "test-agent",
		SkillRegistry: reg,
	})
}

// stubSkill is a one-tool skill whose Execute returns a canned string.
// It exists so the tool-call round-trip test can verify that the
// pipeline forwards tool results back to the model on the next turn.
type stubSkill struct {
	toolName   string
	reply      string
	execCalls  int
	lastParams json.RawMessage
}

func (s *stubSkill) Name() string        { return "stub" }
func (s *stubSkill) Description() string { return "test skill" }
func (s *stubSkill) Version() string     { return "0.0.1" }
func (s *stubSkill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{{
		Name:        s.toolName,
		Description: "stub tool for tests",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}}
}
func (s *stubSkill) Init(_ json.RawMessage) error { return nil }
func (s *stubSkill) Close() error                 { return nil }
func (s *stubSkill) Execute(_ context.Context, _ string, params json.RawMessage) (skills.ToolResult, error) {
	s.execCalls++
	s.lastParams = params
	return skills.ToolResult{Content: s.reply}, nil
}

// A Stop that lands mid-completion may salvage a partial that still
// carries a tool call the model had begun (often one the user never even
// saw — providers buffer the whole call). Dispatching it would run the
// exact side effect the user pressed Stop to cancel, so runToolLoop must
// drop the calls and return the text only.
func TestRunToolLoop_StopDoesNotDispatchSalvagedToolCall(t *testing.T) {
	stub := &stubSkill{toolName: "stub_lookup", reply: "SIDE EFFECT — must not run"}
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatalf("register: %v", err)
	}
	pl := makePipelineWithIters(&mockEngine{}, testutil.NewMockLLM(t), reg, 4)

	// Model the real timing: the turn is live when the loop starts, then
	// Stop lands DURING the completion — the completion cancels the turn
	// context (as StopTurn would) and salvages a partial that still
	// carries a tool call.
	ctx, cancel := context.WithCancelCause(context.Background())
	complete := func(_ context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
		cancel(errUserStopped)
		return &llm.CompletionResponse{
			Content:      "let me look that up",
			FinishReason: "stopped",
			ToolCalls: []llm.ToolCall{{
				ID:        "c1",
				Name:      "stub_lookup",
				Arguments: json.RawMessage(`{"q":"x"}`),
			}},
		}, nil
	}
	baseReq := llm.CompletionRequest{
		Model: "mock-model",
		Tools: []llm.ToolSpec{{Name: "stub_lookup"}},
	}

	resp, _, err := pl.runToolLoop(ctx, baseReq, "standard", classifier.SearchNone, 0, complete, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatalf("runToolLoop errored on a stopped turn instead of salvaging: %v", err)
	}
	if stub.execCalls != 0 {
		t.Errorf("tool dispatched %d times after Stop — must be 0 (the side effect the user cancelled)", stub.execCalls)
	}
	if resp.Content != "let me look that up" {
		t.Errorf("salvaged content = %q, want the partial text", resp.Content)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("salvaged tool calls not dropped: %d remain", len(resp.ToolCalls))
	}
	if resp.FinishReason != "stopped" {
		t.Errorf("finish reason = %q, want stopped", resp.FinishReason)
	}
}

// makePipelineWithIters is makePipelineWithMockLLM with an explicit
// tool-loop cap, for the runaway-loop test.
func makePipelineWithIters(eng *mockEngine, mock *testutil.MockLLM, reg *skills.Registry, maxIters int) *Pipeline {
	models := []config.ModelConfig{
		{ID: "mock-model", Provider: "llama-server", Endpoint: mock.URL(), Capabilities: []string{"tools"}},
	}
	rr := router.NewRegistry(models)
	rr.SetStatusForTest("mock-model", "online")
	rtr := router.NewRouter(config.RouterConfig{Enabled: true}, rr)
	return New(Deps{
		Engine:        eng,
		Router:        rtr,
		Sessions:      session.NewManager(),
		AgentID:       "test-agent",
		SkillRegistry: reg,
		MaxToolIters:  maxIters,
	})
}

// ── tests ────────────────────────────────────────────────────

// A model that keeps asking for tools forever must not loop forever.
// The tool loop has to stop at the iteration cap and return gracefully
// (no error, no panic, no 500 to the user) — at worst the user gets a
// truncated answer, never a hung turn. This is the single most likely
// local-model failure mode, so pin that it degrades safely.
func TestIntegration_ToolLoopStopsAtCap(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	// Always return a tool call, never a final text answer.
	for i := 0; i < 40; i++ {
		mock.Enqueue(testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        "call_loop",
				Name:      "stub_lookup",
				Arguments: map[string]any{"q": "again"},
			}},
		})
	}

	stub := &stubSkill{toolName: "stub_lookup", reply: "more data"}
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatalf("register: %v", err)
	}

	eng := &mockEngine{}
	pl := makePipelineWithIters(eng, mock, reg, 4)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	resp, _, err := pl.Handle(context.Background(), sess, "do something with tools", nil)
	if err != nil {
		t.Fatalf("runaway tool loop returned an error instead of degrading gracefully: %v", err)
	}
	_ = resp // content may be empty (last turn was a tool call) — what matters is no error/hang.

	// It looped (more than once) but stopped well before draining the
	// 40 queued responses — i.e. it honored a cap rather than running
	// away. The cap can grow for the web_search budget, so assert a
	// generous upper bound rather than the exact configured value.
	if stub.execCalls < 2 {
		t.Errorf("tool executed %d times — loop didn't iterate", stub.execCalls)
	}
	if stub.execCalls > 20 {
		t.Errorf("tool executed %d times — loop did not honor an iteration cap", stub.execCalls)
	}
}

// TestIntegration_TrivialResponse drives Handle end-to-end with a
// single scripted text reply: no tools, no memory hits. Asserts the
// answer round-trips and that the mock saw exactly one request in the
// expected shape.
func TestIntegration_TrivialResponse(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{Content: "hello back"})

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, nil)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	resp, info, err := pl.Handle(context.Background(), sess, "hello there", nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp != "hello back" {
		t.Errorf("response = %q, want %q", resp, "hello back")
	}
	if info.ModelID != "mock-model" {
		t.Errorf("ModelID = %q", info.ModelID)
	}
	if mock.CallCount() != 1 {
		t.Errorf("mock.CallCount = %d, want 1", mock.CallCount())
	}

	calls := mock.Calls()
	// Last message should be the user's request.
	msgs := calls[0].Messages
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" ||
		!strings.Contains(msgs[len(msgs)-1].Content, "hello there") {
		t.Errorf("last message not the user turn: %+v", msgs)
	}
	mock.AssertAllConsumed()
}

// TestIntegration_ToolCallRoundTrip scripts a tool_calls response
// followed by a text response. Verifies the pipeline dispatches the
// tool through the skill registry and feeds the result back on the
// second call.
func TestIntegration_ToolCallRoundTrip(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        "call_1",
				Name:      "stub_lookup",
				Arguments: map[string]any{"q": "weather"},
			}},
		},
		testutil.ScriptedResponse{Content: "it is sunny"},
	)

	stub := &stubSkill{toolName: "stub_lookup", reply: "sunny, 72F"}
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatalf("register: %v", err)
	}

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, reg)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	resp, _, err := pl.Handle(context.Background(), sess, "what's the weather?", nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp != "it is sunny" {
		t.Errorf("final response = %q", resp)
	}
	if stub.execCalls != 1 {
		t.Errorf("stub executed %d times, want 1", stub.execCalls)
	}
	if !strings.Contains(string(stub.lastParams), "weather") {
		t.Errorf("stub params did not preserve args: %s", stub.lastParams)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("mock.CallCount = %d, want 2 (tool + followup)", mock.CallCount())
	}

	// Second call's messages should include the assistant's tool_calls
	// turn followed by a tool-role result carrying the stub's reply.
	second := mock.Calls()[1].Messages
	var sawToolResult bool
	for _, m := range second {
		if m.Role == "tool" && m.ToolCallID == "call_1" &&
			strings.Contains(m.Content, "sunny, 72F") {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Errorf("tool result not fed back to model; messages=%+v", second)
	}

	// First call should have advertised the stub tool in the tools array.
	firstTools := mock.Calls()[0].Tools
	var sawStub bool
	for _, tl := range firstTools {
		if tl.Function.Name == "stub_lookup" {
			sawStub = true
		}
	}
	if !sawStub {
		t.Errorf("stub tool not advertised on first call: %+v", firstTools)
	}

	mock.AssertAllConsumed()
}

// TestIntegration_StreamingRoundTrip exercises HandleStream through
// the same MockLLM. The mock emits a single streamed text chunk; we
// collect it via the onChunk callback and assert it matches what the
// pipeline also returned as the final response.
func TestIntegration_StreamingRoundTrip(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{Content: "streamed hello"})

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, nil)
	sess := pl.sessions.GetOrCreate("cli", "user1")

	var chunks strings.Builder
	onChunk := func(s string) { chunks.WriteString(s) }
	resp, _, err := pl.HandleStream(context.Background(), sess, "hi", nil, onChunk, nil, nil)
	if err != nil {
		t.Fatalf("HandleStream: %v", err)
	}
	if resp != "streamed hello" {
		t.Errorf("final = %q", resp)
	}
	if chunks.String() != "streamed hello" {
		t.Errorf("chunks = %q", chunks.String())
	}
	if mock.CallCount() != 1 {
		t.Errorf("CallCount = %d", mock.CallCount())
	}
	// Verify the request actually used streaming.
	if !mock.Calls()[0].Stream {
		t.Error("expected Stream=true on recorded request")
	}
	mock.AssertAllConsumed()
}

// TestIntegration_MultiUserIsolation is the Phase C acceptance test.
// Two sessions belonging to different canonical users share one
// pipeline and one memory store. The store holds:
//
//   - a "global" row visible to everyone
//   - a private row owned by alice
//   - a private row owned by bob
//
// Each user takes one turn; the test then asserts that Search was
// invoked with each user's own canonical ID (never the other's) and
// that no session has leaked the other user's private memory into its
// LLM prompt.
func TestIntegration_MultiUserIsolation(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{Content: "alice answer"},
		testutil.ScriptedResponse{Content: "bob answer"},
	)

	store := newRecordingStore(map[string][]memory.MemoryResult{
		"":      {{Content: "shared knowledge", Scope: "global", Similarity: 0.9}},
		"alice": {{Content: "alice private secret", Scope: "user", Similarity: 0.95}},
		"bob":   {{Content: "bob private secret", Scope: "user", Similarity: 0.95}},
	})
	embed := func(context.Context, string) ([]float32, error) {
		return []float32{0.1, 0.2, 0.3}, nil
	}

	models := []config.ModelConfig{
		{
			ID:           "mock-model",
			Provider:     "llama-server",
			Endpoint:     mock.URL(),
			Capabilities: []string{"tools"},
		},
	}
	rr := router.NewRegistry(models)
	rr.SetStatusForTest("mock-model", "online")
	rtr := router.NewRouter(config.RouterConfig{
		Enabled: true,
	}, rr)

	pl := New(Deps{
		Engine:      &mockEngine{},
		Router:      rtr,
		Sessions:    session.NewManager(),
		AgentID:     "test-agent",
		MemoryStore: store,
		Embedder:    embed,
	})

	aliceSess := pl.sessions.GetOrCreate("slack", "U_ALICE")
	aliceSess.SetCanonicalID("alice")
	bobSess := pl.sessions.GetOrCreate("slack", "U_BOB")
	bobSess.SetCanonicalID("bob")

	if _, _, err := pl.Handle(context.Background(), aliceSess, "what do I know?", nil); err != nil {
		t.Fatalf("alice Handle: %v", err)
	}
	if _, _, err := pl.Handle(context.Background(), bobSess, "what do I know?", nil); err != nil {
		t.Fatalf("bob Handle: %v", err)
	}

	// Every Search call must have carried a canonical user ID, never
	// an empty string (which would collapse to global-only) and never
	// the wrong user's ID (which would leak private memories).
	if len(store.calls) == 0 {
		t.Fatal("expected at least one Search call per user turn")
	}
	seenUsers := map[string]int{}
	for _, c := range store.calls {
		if c.userID != "alice" && c.userID != "bob" {
			t.Errorf("Search called with unexpected userID %q", c.userID)
		}
		seenUsers[c.userID]++
	}
	if seenUsers["alice"] == 0 || seenUsers["bob"] == 0 {
		t.Errorf("expected Search per user; got %v", seenUsers)
	}

	// Cross-check the LLM prompts: bob's private row must never
	// appear in alice's request, and vice-versa. The recorded request
	// body flattens every message content, so a substring scan is
	// sufficient.
	calls := mock.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(calls))
	}
	for i, c := range calls {
		owner := "alice"
		foreign := "bob private secret"
		if i == 1 {
			owner = "bob"
			foreign = "alice private secret"
		}
		body := string(c.RawBody)
		if strings.Contains(body, foreign) {
			t.Errorf("%s's LLM prompt leaked foreign memory %q", owner, foreign)
		}
		if !strings.Contains(body, "shared knowledge") {
			t.Errorf("%s's prompt missing the shared/global memory", owner)
		}
	}

	mock.AssertAllConsumed()
}
