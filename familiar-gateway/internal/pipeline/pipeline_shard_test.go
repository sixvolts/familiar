package pipeline

// Tests for the shard-invocation path: HandleShard runs the pipeline
// inside a ShardOverrides envelope. These tests drive the full pipeline
// end-to-end against MockLLM and assert that the seams land correctly:
// system prompt, memory skipping, tool allowlist, blocked dispatch,
// model override, max_tokens / temperature, and ephemeral-skips-commit.
//
// The shared makePipelineWithMockLLM / stubSkill / mockEngine helpers
// come from pipeline_mockllm_test.go and pipeline_test.go; this file
// adds only shard-specific fixtures.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/shards"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
)

// TestOverridesForShard_CopiesBookAccess guards the canonical
// translation: book_access on the stored shard must reach ShardOverrides
// so the scheduled-actions runner and the /v1 invoke adapter both carry
// the confinement. The file comment on OverridesForShard warns that a
// new shard field can silently miss one entry point — this is the guard.
func TestOverridesForShard_CopiesBookAccess(t *testing.T) {
	sh := &shards.Shard{
		ID:            "scoped",
		Persistence:   shards.PersistencePersistent,
		Visibility:    shards.VisibilityIsolated,
		ToolAllowlist: []string{"read_page", "update_page"},
		BookAccess:    []string{"book-familywiki"},
	}
	ov := OverridesForShard(sh)
	if len(ov.BookAccess) != 1 || ov.BookAccess[0] != "book-familywiki" {
		t.Errorf("BookAccess = %v, want [book-familywiki]", ov.BookAccess)
	}
	// Empty book_access (the common case) translates to no restriction.
	sh.BookAccess = nil
	if got := OverridesForShard(sh).BookAccess; len(got) != 0 {
		t.Errorf("empty book_access should yield empty BookAccess, got %v", got)
	}
}

// assertMessagesContainSystem returns the content of the system message
// in the recorded request, or the empty string if none is present.
func recordedSystemMsg(msgs []testutil.RecordedMessage) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// ptrFloat32 is a tiny helper so the tests can set Temperature
// ergonomically without declaring a named var at every call site.
func ptrFloat32(v float32) *float32 { return &v }

// TestShard_BasicInvocation is the happy-path test: a minimal
// ShardOverrides runs end-to-end. The shard's system prompt is the
// only non-user content in the request; nothing else from the trusted
// pipeline (tiered overlay, working context, memories) appears.
func TestShard_BasicInvocation(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{Content: `{"name":"Electrify America Fresno"}`})

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, nil)
	sess := pl.sessions.GetOrCreate("shards", "cannonball")

	overrides := &ShardOverrides{
		ShardID:              "charger-extractor",
		SystemPrompt:         "You extract charger metadata. Return JSON only.",
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ModelOverride:        "mock-model",
		MaxTokens:            512,
		Temperature:          ptrFloat32(0.1),
	}

	resp, info, err := pl.HandleShard(context.Background(), sess, `{"url":"..."}`, overrides)
	if err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	if resp != `{"name":"Electrify America Fresno"}` {
		t.Errorf("response = %q", resp)
	}
	if info.ModelID != "mock-model" {
		t.Errorf("ModelID = %q, want mock-model", info.ModelID)
	}

	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(calls))
	}
	sys := recordedSystemMsg(calls[0].Messages)
	if sys != overrides.SystemPrompt {
		t.Errorf("system prompt = %q, want %q", sys, overrides.SystemPrompt)
	}
	// Make sure no tiered-prompt fragments leaked in.
	for _, fragment := range []string{"Relevant context", "conversation_summary", "Entity Knowledge Graph"} {
		if strings.Contains(sys, fragment) {
			t.Errorf("system prompt leaked trusted-path fragment %q", fragment)
		}
	}

	// Verify MaxTokens and Temperature landed in the wire request.
	var wire struct {
		MaxTokens   int     `json:"max_tokens"`
		Temperature float32 `json:"temperature"`
	}
	if err := json.Unmarshal(calls[0].RawBody, &wire); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	// MaxTokens is the ANSWER budget (512); the classifier's thinking
	// budget now stacks on top so a reasoning model can't spend the
	// whole allowance inside <think> and starve the reply
	// (EXTERNAL-READINESS-REVIEW.md). A shard with no forced complexity
	// resolves to medium thinking.
	wantMax := 512 + pl.effort.ThinkingFor(classifier.ThinkingMedium).TokenBudget
	if wire.MaxTokens != wantMax {
		t.Errorf("max_tokens = %d, want %d (512 answer + thinking budget)", wire.MaxTokens, wantMax)
	}
	if wire.Temperature != 0.1 {
		t.Errorf("temperature = %v, want 0.1", wire.Temperature)
	}

	mock.AssertAllConsumed()
}

// TestShard_PanicsOnNilOverrides documents the intentional fail-loud
// behavior of HandleShard: callers must supply overrides. Silently
// falling back to Handle would bypass the shard envelope the caller
// thought they were opting into.
func TestShard_PanicsOnNilOverrides(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected HandleShard(nil) to panic")
		}
	}()
	mock := testutil.NewMockLLM(t)
	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, nil)
	sess := pl.sessions.GetOrCreate("shards", "x")
	_, _, _ = pl.HandleShard(context.Background(), sess, "hi", nil)
}

// TestShard_EphemeralSkipsCommit asserts that commitAndExtract does
// not fire on an ephemeral shard: the mock engine records no commits
// and the session's turn buffer stays empty across an invocation.
func TestShard_EphemeralSkipsCommit(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{Content: "ok"})

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, nil)
	sess := pl.sessions.GetOrCreate("shards", "ephemeral")

	overrides := &ShardOverrides{
		ShardID:              "eph",
		SystemPrompt:         "test",
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ModelOverride:        "mock-model",
	}
	if _, _, err := pl.HandleShard(context.Background(), sess, "one", overrides); err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	if eng.commitCalled {
		t.Error("engine.CommitFacts should not fire for ephemeral shard")
	}
	if len(sess.RecentTurns(10)) != 0 {
		t.Errorf("session turns = %d, want 0 for ephemeral shard", len(sess.RecentTurns(10)))
	}
}

// TestShard_PersistentCommitsAndAppendsTurns asserts the counterpart:
// a persistent shard (SkipCommit=false) goes through the commit path,
// so the engine sees the fact and the session buffer grows.
func TestShard_PersistentCommitsAndAppendsTurns(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{Content: "persisted"})

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, nil)
	sess := pl.sessions.GetOrCreate("shards", "persistent")

	overrides := &ShardOverrides{
		ShardID:             "pers",
		SystemPrompt:        "test",
		SkipMemoryRetrieval: true,
		ModelOverride:       "mock-model",
		ScopeTag:            "shard:pers",
	}
	if _, _, err := pl.HandleShard(context.Background(), sess, "keep me", overrides); err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	if !eng.commitCalled {
		t.Error("engine.CommitFacts should fire for persistent shard")
	}
	// Committed FactProto carries the shard's ScopeTag, which the previous
	// engine persists to memories.scope_tag. That column is what the
	// retrieval filter in internal/memory/pgvector.go consults to hide
	// isolated-shard rows from top-level views.
	if len(eng.commitFacts) != 1 {
		t.Fatalf("commitFacts = %d, want 1", len(eng.commitFacts))
	}
	if eng.commitFacts[0].ScopeTag != "shard:pers" {
		t.Errorf("FactProto.ScopeTag = %q, want shard:pers", eng.commitFacts[0].ScopeTag)
	}
	turns := sess.RecentTurns(10)
	if len(turns) != 2 {
		t.Fatalf("session turns = %d, want 2 (user + assistant)", len(turns))
	}
	if turns[0].Role != "user" || !strings.Contains(turns[0].Content, "keep me") {
		t.Errorf("turn[0] = %+v, want user/keep me", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Content != "persisted" {
		t.Errorf("turn[1] = %+v, want assistant/persisted", turns[1])
	}
}

// TestShard_TrustedPathLeavesFactProtoScopeTagEmpty is the counterpart
// to the persistent-commit test: Handle (no overrides) must not stamp a
// ScopeTag on the committed fact. A non-empty tag on a trusted write
// would get that fact filtered out of the owner's own retrieval as
// soon as an isolated shard with a matching tag existed.
func TestShard_TrustedPathLeavesFactProtoScopeTagEmpty(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{Content: "answer"})

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, nil)
	sess := pl.sessions.GetOrCreate("cli", "trusted")

	if _, _, err := pl.Handle(context.Background(), sess, "question", nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(eng.commitFacts) != 1 {
		t.Fatalf("commitFacts = %d, want 1", len(eng.commitFacts))
	}
	if eng.commitFacts[0].ScopeTag != "" {
		t.Errorf("trusted-path ScopeTag = %q, want empty", eng.commitFacts[0].ScopeTag)
	}
}

// TestShard_ToolAllowlistFiltersAdvertised verifies that only the
// shard's allowlisted tools are advertised to the model. A registered
// skill not in the allowlist must not appear in the request's
// tools[] array.
func TestShard_ToolAllowlistFiltersAdvertised(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{Content: "done"})

	allowed := &stubSkill{toolName: "allowed_tool", reply: "ok"}
	disallowed := &stubSkill{toolName: "disallowed_tool", reply: "ok"}
	reg := skills.NewRegistry()
	// stubSkill.Name() returns "stub" for both, so they collide on
	// Register. Wrap one in a renamed shim.
	if err := reg.Register(allowed); err != nil {
		t.Fatalf("register allowed: %v", err)
	}
	if err := reg.Register(&renamedStubSkill{stubSkill: disallowed, name: "stub2"}); err != nil {
		t.Fatalf("register disallowed: %v", err)
	}

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, reg)
	sess := pl.sessions.GetOrCreate("shards", "t")

	overrides := &ShardOverrides{
		ShardID:              "filtered",
		SystemPrompt:         "use tools sparingly",
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ModelOverride:        "mock-model",
		ToolAllowlist:        []string{"allowed_tool"},
	}
	if _, _, err := pl.HandleShard(context.Background(), sess, "go", overrides); err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	tools := mock.Calls()[0].Tools
	if len(tools) != 1 {
		t.Fatalf("advertised tools = %d, want 1 (only allowed_tool)", len(tools))
	}
	if tools[0].Function.Name != "allowed_tool" {
		t.Errorf("advertised tool = %q, want allowed_tool", tools[0].Function.Name)
	}
}

// TestShard_BlockedToolCallReturnsSyntheticError scripts the model
// trying to invoke a tool outside the shard's allowlist. The pipeline
// must reject the call (not dispatch it to the registry) and feed a
// synthetic error back so the next model turn can adapt.
func TestShard_BlockedToolCallReturnsSyntheticError(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        "call_bad",
				Name:      "forbidden_tool",
				Arguments: map[string]any{},
			}},
		},
		testutil.ScriptedResponse{Content: "I cannot do that."},
	)

	// Both tools are registered; only `allowed_tool` is in the shard's
	// allowlist. The scripted model call names `forbidden_tool`, which
	// would be dispatched to the registry on the trusted path but must
	// be blocked here. req.Tools is non-empty (carries the allowed
	// spec), so runCompletion goes through the tool loop and hits the
	// allowlist check.
	allowed := &stubSkill{toolName: "allowed_tool", reply: "allowed ran"}
	forbidden := &stubSkill{toolName: "forbidden_tool", reply: "should not be called"}
	reg := skills.NewRegistry()
	if err := reg.Register(allowed); err != nil {
		t.Fatalf("register allowed: %v", err)
	}
	if err := reg.Register(&renamedStubSkill{stubSkill: forbidden, name: "stub2"}); err != nil {
		t.Fatalf("register forbidden: %v", err)
	}

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, reg)
	sess := pl.sessions.GetOrCreate("shards", "b")

	overrides := &ShardOverrides{
		ShardID:              "locked",
		SystemPrompt:         "no tools for you",
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ModelOverride:        "mock-model",
		ToolAllowlist:        []string{"allowed_tool"},
	}
	if _, _, err := pl.HandleShard(context.Background(), sess, "go", overrides); err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	if forbidden.execCalls != 0 {
		t.Errorf("forbidden skill was dispatched %d times, want 0 (blocked by allowlist)", forbidden.execCalls)
	}
	if allowed.execCalls != 0 {
		t.Errorf("allowed skill executed unexpectedly (%d); the scripted call was for forbidden_tool", allowed.execCalls)
	}
	// The second call should carry a tool-role message with the
	// synthetic error text.
	if len(mock.Calls()) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(mock.Calls()))
	}
	second := mock.Calls()[1].Messages
	var sawBlock bool
	for _, m := range second {
		if m.Role == "tool" && m.ToolCallID == "call_bad" &&
			strings.Contains(m.Content, "not available in this shard's allowlist") {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Errorf("synthetic block error not fed back to model; messages=%+v", second)
	}
}

// TestShard_ScopeTagReachesSkillContext verifies that a skill dispatch
// inside a shard invocation sees the shard's scope_tag in its
// SessionContext. The captureSkill below is a one-tool skill that
// records the context it was invoked with, so we can assert the
// plumbing ends at the right place.
func TestShard_ScopeTagReachesSkillContext(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        "call_cap",
				Name:      "capture",
				Arguments: map[string]any{},
			}},
		},
		testutil.ScriptedResponse{Content: "captured"},
	)

	cap := &captureSkill{}
	reg := skills.NewRegistry()
	if err := reg.Register(cap); err != nil {
		t.Fatalf("register: %v", err)
	}

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, reg)
	sess := pl.sessions.GetOrCreate("shards", "s")

	overrides := &ShardOverrides{
		ShardID:              "tagged",
		SystemPrompt:         "capture the context",
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ModelOverride:        "mock-model",
		ToolAllowlist:        []string{"capture"},
		ScopeTag:             "shard:tagged",
	}
	if _, _, err := pl.HandleShard(context.Background(), sess, "go", overrides); err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	if cap.lastScope != "shard:tagged" {
		t.Errorf("skill received ScopeTag = %q, want %q", cap.lastScope, "shard:tagged")
	}
}

// TestShard_BookScopeReachesSkillContext is the BookScope counterpart to
// the ScopeTag test: the shard's book allowlist must arrive on the skill
// dispatch SessionContext so the wiki skill can confine itself. The
// console session boundary (authz.go) never runs on the scheduled /
// invoke paths, so this plumbing is the only thing carrying the
// confinement there.
func TestShard_BookScopeReachesSkillContext(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        "call_cap",
				Name:      "capture",
				Arguments: map[string]any{},
			}},
		},
		testutil.ScriptedResponse{Content: "captured"},
	)

	cap := &captureSkill{}
	reg := skills.NewRegistry()
	if err := reg.Register(cap); err != nil {
		t.Fatalf("register: %v", err)
	}

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, reg)
	sess := pl.sessions.GetOrCreate("shards", "s")

	overrides := &ShardOverrides{
		ShardID:              "scoped",
		SystemPrompt:         "capture the context",
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ModelOverride:        "mock-model",
		ToolAllowlist:        []string{"capture"},
		BookAccess:           []string{"book-familywiki"},
	}
	if _, _, err := pl.HandleShard(context.Background(), sess, "go", overrides); err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	if len(cap.lastBooks) != 1 || cap.lastBooks[0] != "book-familywiki" {
		t.Errorf("skill received BookScope = %v, want [book-familywiki]", cap.lastBooks)
	}
}

// TestShard_TrustedPathUnchanged covers the opposite invariant: a
// Handle (no overrides) call must not see any shard behavior.
// SessionContext arrives at a skill with an empty ScopeTag.
func TestShard_TrustedPathUnchanged(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        "call_cap",
				Name:      "capture",
				Arguments: map[string]any{},
			}},
		},
		testutil.ScriptedResponse{Content: "captured"},
	)

	cap := &captureSkill{}
	reg := skills.NewRegistry()
	if err := reg.Register(cap); err != nil {
		t.Fatalf("register: %v", err)
	}

	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, reg)
	sess := pl.sessions.GetOrCreate("cli", "trusted-user")
	sess.SetCanonicalID("trusted-user")

	if _, _, err := pl.Handle(context.Background(), sess, "go", nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if cap.lastScope != "" {
		t.Errorf("trusted path should carry empty ScopeTag, got %q", cap.lastScope)
	}
	if cap.lastUserID != "trusted-user" {
		t.Errorf("trusted path should pass UserID through, got %q", cap.lastUserID)
	}
}

// ── test-only skills ─────────────────────────────────────────

// renamedStubSkill wraps stubSkill to return a different Name() so two
// stubSkills can share one registry without colliding.
type renamedStubSkill struct {
	*stubSkill
	name string
}

func (r *renamedStubSkill) Name() string { return r.name }

// captureSkill records the SessionContext it was last invoked with, so
// the ScopeTag-reaches-skill test can assert on it without staring into
// goroutine internals.
type captureSkill struct {
	lastScope   string
	lastUserID  string
	lastSession string
	lastBooks   []string
}

func (c *captureSkill) Name() string        { return "capture" }
func (c *captureSkill) Description() string { return "records ctx" }
func (c *captureSkill) Version() string     { return "0.0.1" }
func (c *captureSkill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{{
		Name:        "capture",
		Description: "record ctx",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}}
}
func (c *captureSkill) Init(_ json.RawMessage) error { return nil }
func (c *captureSkill) Close() error                 { return nil }
func (c *captureSkill) Execute(ctx context.Context, _ string, _ json.RawMessage) (skills.ToolResult, error) {
	if sc, ok := skills.ContextFrom(ctx); ok {
		c.lastScope = sc.ScopeTag
		c.lastUserID = sc.UserID
		c.lastSession = sc.SessionID
		c.lastBooks = sc.BookScope
	}
	return skills.ToolResult{Content: "captured"}, nil
}

// webSearchStub is a stubSkill whose tool is literally named
// "web_search" so the tool-loop's search-budget gate (which matches on
// the tool name) engages during shard tests.
type webSearchStub struct{ *stubSkill }

func (w *webSearchStub) Name() string { return "search" }

func newWebSearchStub() *webSearchStub {
	return &webSearchStub{&stubSkill{toolName: "web_search", reply: "1. Result — example.com"}}
}

// TestShard_SearchBudgetZeroKeepsWebSearchDisabled pins today's default:
// shard turns bypass the classifier and are stamped SearchNone, so a
// web_search tool call — even one the allowlist permits — is refused
// with a synthetic tool result and the skill never executes. A shard
// operator granting web_search in the allowlist alone must not be
// enough; the envelope has to carry an explicit SearchBudget.
func TestShard_SearchBudgetZeroKeepsWebSearchDisabled(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{
		ToolCalls: []testutil.ScriptedToolCall{{
			ID:        "call_ws1",
			Name:      "web_search",
			Arguments: map[string]any{"query": "anything"},
		}},
	})
	mock.Enqueue(testutil.ScriptedResponse{Content: "done without search"})

	stub := newWebSearchStub()
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatalf("register: %v", err)
	}
	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, reg)
	sess := pl.sessions.GetOrCreate("shards", "srch-off")

	overrides := &ShardOverrides{
		ShardID:              "worker-no-budget",
		SystemPrompt:         "You are a test worker.",
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ModelOverride:        "mock-model",
		ToolAllowlist:        []string{"web_search"},
	}
	if _, _, err := pl.HandleShard(context.Background(), sess, "search for anything", overrides); err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	if stub.execCalls != 0 {
		t.Errorf("web_search executed %d times, want 0 (SearchNone default)", stub.execCalls)
	}
	calls := mock.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(calls))
	}
	// The model must have been told search is unavailable, not silently
	// starved.
	var sawRefusal bool
	for _, m := range calls[1].Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "web_search is not available") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Error("second LLM call carries no synthetic web_search refusal")
	}
	mock.AssertAllConsumed()
}

// TestShard_SearchBudgetGrantsWebSearch is the Phase-2 research-worker
// seam: an envelope with SearchBudget=N lifts the shard-path SearchNone
// stamp, allows exactly N web_search executions, and converts call N+1
// into the synthetic budget-exhausted tool result.
func TestShard_SearchBudgetGrantsWebSearch(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	for _, id := range []string{"call_ws1", "call_ws2", "call_ws3"} {
		mock.Enqueue(testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        id,
				Name:      "web_search",
				Arguments: map[string]any{"query": "q"},
			}},
		})
	}
	mock.Enqueue(testutil.ScriptedResponse{Content: "synthesized"})

	stub := newWebSearchStub()
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatalf("register: %v", err)
	}
	eng := &mockEngine{}
	pl := makePipelineWithMockLLM(eng, mock, reg)
	sess := pl.sessions.GetOrCreate("shards", "srch-on")

	overrides := &ShardOverrides{
		ShardID:              "research-worker",
		SystemPrompt:         "You are a research worker.",
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ModelOverride:        "mock-model",
		ToolAllowlist:        []string{"web_search"},
		SearchBudget:         2,
	}
	resp, _, err := pl.HandleShard(context.Background(), sess, "research the thing", overrides)
	if err != nil {
		t.Fatalf("HandleShard: %v", err)
	}
	if resp != "synthesized" {
		t.Errorf("response = %q, want %q", resp, "synthesized")
	}
	if stub.execCalls != 2 {
		t.Errorf("web_search executed %d times, want exactly 2 (the budget)", stub.execCalls)
	}
	calls := mock.Calls()
	if len(calls) != 4 {
		t.Fatalf("expected 4 LLM calls, got %d", len(calls))
	}
	var sawExhausted bool
	for _, m := range calls[3].Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "budget exhausted") {
			sawExhausted = true
		}
	}
	if !sawExhausted {
		t.Error("final LLM call carries no budget-exhausted tool result for the 3rd search")
	}
	mock.AssertAllConsumed()
}
