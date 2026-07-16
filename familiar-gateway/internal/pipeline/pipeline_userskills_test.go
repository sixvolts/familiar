package pipeline

// USER-SKILLS-SPEC Phase B — the trusted-path grant. When
// UserSkillsAugment returns a block, the turn's system prompt carries
// it and the (otherwise shard-only) skillpacks tools are advertised
// AND dispatchable; without a grant the historical ban holds even
// against a hallucinated call. Runs the full pipeline against MockLLM.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
)

// useSkillStub stands in for the skillpacks skill: one tool named
// use_skill that records the SessionContext identity it dispatched
// with.
type useSkillStub struct {
	execCalls  int
	lastUserID string
	lastShard  string
}

func (s *useSkillStub) Name() string        { return "skillpacks-stub" }
func (s *useSkillStub) Description() string { return "records ctx" }
func (s *useSkillStub) Version() string     { return "0.0.1" }
func (s *useSkillStub) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{{
		Name:        "use_skill",
		Description: "stub",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
	}}
}
func (s *useSkillStub) Init(_ json.RawMessage) error { return nil }
func (s *useSkillStub) Close() error                 { return nil }
func (s *useSkillStub) Execute(ctx context.Context, _ string, _ json.RawMessage) (skills.ToolResult, error) {
	s.execCalls++
	if sc, ok := skills.ContextFrom(ctx); ok {
		s.lastUserID = sc.UserID
		s.lastShard = sc.ShardID
	}
	return skills.ToolResult{Content: "# Skill: grocery\n\nRULESWORD"}, nil
}

func makeUserSkillsPipeline(eng *mockEngine, mock *testutil.MockLLM, reg *skills.Registry, augment func(context.Context, string) string) *Pipeline {
	models := []config.ModelConfig{{
		ID:           "mock-model",
		Provider:     "llama-server",
		Endpoint:     mock.URL(),
		Capabilities: []string{"tools"},
	}}
	rr := router.NewRegistry(models)
	rr.SetStatusForTest("mock-model", "online")
	rtr := router.NewRouter(config.RouterConfig{Enabled: true}, rr)
	return New(Deps{
		Engine:            eng,
		Router:            rtr,
		Sessions:          session.NewManager(),
		AgentID:           "test-agent",
		SkillRegistry:     reg,
		ShardOnlyTools:    []string{"use_skill", "read_skill_file"},
		UserSkillsAugment: augment,
	})
}

func TestUserSkills_GrantInjectsBlockAndUnlocksDispatch(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        "call_us",
				Name:      "use_skill",
				Arguments: map[string]any{"name": "grocery"},
			}},
		},
		testutil.ScriptedResponse{Content: "done per the skill"},
	)

	stub := &useSkillStub{}
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatalf("register: %v", err)
	}

	var augmentedFor string
	augment := func(_ context.Context, userID string) string {
		augmentedFor = userID
		return "## Skills\n- grocery: household grocery rules"
	}

	eng := &mockEngine{}
	pl := makeUserSkillsPipeline(eng, mock, reg, augment)
	sess := pl.sessions.GetOrCreate("cli", "alice")
	sess.SetCanonicalID("alice")

	resp, _, err := pl.Handle(context.Background(), sess, "tidy the grocery list", nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp != "done per the skill" {
		t.Errorf("resp = %q", resp)
	}
	if augmentedFor != "alice" {
		t.Errorf("augment called for %q, want alice", augmentedFor)
	}
	if stub.execCalls != 1 {
		t.Fatalf("use_skill dispatched %d times, want 1", stub.execCalls)
	}
	if stub.lastUserID != "alice" || stub.lastShard != "" {
		t.Errorf("dispatch identity = user %q shard %q, want alice / empty", stub.lastUserID, stub.lastShard)
	}

	// The first LLM request carried the block in its system message and
	// advertised the unlocked tool.
	first := mock.Calls()[0]
	var sysContent string
	for _, m := range first.Messages {
		if m.Role == "system" {
			sysContent = m.Content
			break
		}
	}
	if !strings.Contains(sysContent, "## Skills") || !strings.Contains(sysContent, "grocery: household grocery rules") {
		t.Errorf("system message missing skills block: %q", sysContent)
	}
	advertised := false
	for _, tool := range first.Tools {
		if tool.Function.Name == "use_skill" {
			advertised = true
		}
	}
	if !advertised {
		t.Errorf("use_skill not advertised on granted turn: %v", first.Tools)
	}
}

func TestUserSkills_NoGrantKeepsShardOnlyBan(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{
			ToolCalls: []testutil.ScriptedToolCall{{
				ID:        "call_banned",
				Name:      "use_skill",
				Arguments: map[string]any{"name": "grocery"},
			}},
		},
		testutil.ScriptedResponse{Content: "ok without skills"},
	)

	stub := &useSkillStub{}
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatalf("register: %v", err)
	}
	// A second, ordinary tool keeps the tool loop alive: with zero
	// advertised tools the pipeline skips the loop entirely and the
	// hallucinated call would never reach the dispatch ban.
	if err := reg.Register(&stubSkill{toolName: "other_tool", reply: "x"}); err != nil {
		t.Fatalf("register other: %v", err)
	}

	// Augment present but grants nothing — the empty grant must leave
	// the historical shard-only posture fully intact.
	eng := &mockEngine{}
	pl := makeUserSkillsPipeline(eng, mock, reg, func(_ context.Context, _ string) string { return "" })
	sess := pl.sessions.GetOrCreate("cli", "bob")
	sess.SetCanonicalID("bob")

	if _, _, err := pl.Handle(context.Background(), sess, "hi", nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if stub.execCalls != 0 {
		t.Fatalf("banned tool dispatched %d times, want 0", stub.execCalls)
	}
	calls := mock.Calls()
	if len(calls) < 2 {
		t.Fatalf("expected the loop to continue past the refusal; got %d LLM calls", len(calls))
	}
	// Not advertised…
	for _, tool := range calls[0].Tools {
		if tool.Function.Name == "use_skill" {
			t.Error("use_skill advertised without a grant")
		}
	}
	// …and the hallucinated call got the synthetic refusal.
	refused := false
	for _, m := range calls[1].Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "only available inside a shard") {
			refused = true
		}
	}
	if !refused {
		t.Error("hallucinated use_skill was not refused with the synthetic tool error")
	}
}
