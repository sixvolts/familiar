package pipeline

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
)

// recordingConversations captures what the pipeline persists.
type recordingConversations struct {
	mu    sync.Mutex
	calls []recordedAppend
}

type recordedAppend struct {
	convID, owner string
	msgs          []IntermediateMessage
}

func (r *recordingConversations) LoadRecentTurns(context.Context, string, string, int, int, func(string, string, []byte, string)) error {
	return nil
}

func (r *recordingConversations) AppendIntermediateMessages(_ context.Context, convID, owner string, msgs []IntermediateMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedAppend{convID, owner, append([]IntermediateMessage(nil), msgs...)})
	return nil
}

// The gateway, not the browser, persists a turn's final reply, after the
// tool loop's rows and in the same write, carrying the model and the
// thinking panel's text. It used to be written only by the client after
// the stream's done event, so a dropped stream lost the answer.
func TestCommit_PersistsFinalReplyAfterToolRows(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(
		testutil.ScriptedResponse{ToolCalls: []testutil.ScriptedToolCall{{
			ID: "call_1", Name: "stub_lookup", Arguments: map[string]any{"q": "weather"},
		}}},
		testutil.ScriptedResponse{Content: "it is sunny"},
	)
	reg := skills.NewRegistry()
	if err := reg.Register(&stubSkill{toolName: "stub_lookup", reply: "sunny, 72F"}); err != nil {
		t.Fatal(err)
	}
	pl := makePipelineWithMockLLM(&mockEngine{}, mock, reg)
	convs := &recordingConversations{}
	pl.conversations = convs

	const conv = "3f2c9a1e-7b4d-4e8a-9c1f-2a6b8d0e4f13"
	sess := pl.sessions.GetOrCreateWithID(conv, "workspace", "alice")
	sess.ClaimIdentity("workspace", "alice")

	var shownThinking strings.Builder
	record := func(s string) { shownThinking.WriteString(s) }
	resp, _, err := pl.HandleStream(context.Background(), sess, "what's the weather?", nil,
		func(string) {}, record, record)
	if err != nil {
		t.Fatalf("HandleStream: %v", err)
	}
	if resp != "it is sunny" {
		t.Fatalf("reply = %q", resp)
	}

	convs.mu.Lock()
	defer convs.mu.Unlock()
	if len(convs.calls) != 1 {
		t.Fatalf("persist calls = %d, want 1 (tool rows and the reply together)", len(convs.calls))
	}
	c := convs.calls[0]
	if c.convID != conv || c.owner != "alice" {
		t.Errorf("persisted to (%q, %q), want the session's conversation and user", c.convID, c.owner)
	}
	var roles []string
	for _, m := range c.msgs {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "assistant,tool,assistant" {
		t.Fatalf("persisted roles = %s, want tool call, tool result, then the reply", got)
	}
	final := c.msgs[len(c.msgs)-1]
	if final.Content != "it is sunny" || len(final.ToolCalls) != 0 {
		t.Errorf("final row = %+v, want the plain reply", final)
	}
	if final.Model == "" {
		t.Error("final row has no model")
	}
	if final.ReasoningContent != shownThinking.String() || !strings.Contains(final.ReasoningContent, "Calling tools") {
		t.Errorf("persisted thinking %q, want what was streamed %q", final.ReasoningContent, shownThinking.String())
	}
}

func TestPersistedReply_AddsResearchLinkAndPostHocReasoning(t *testing.T) {
	info := &RouteInfo{
		ModelID:          "big",
		ReasoningContent: "post-hoc",
		ResearchNote:     ResearchNoteRef{BookSlug: "personal:alice", PageSlug: "research-optane", Title: "Research: Optane [draft]"},
	}
	info.thinking.add("Searching memories...\n")
	m := persistedReply("Here is the summary.", info)
	if !strings.Contains(m.Content, "(#note/personal%3Aalice/research-optane)") || strings.Contains(m.Content, "[draft]") {
		t.Errorf("research link = %q", m.Content)
	}
	if m.ReasoningContent != "Searching memories...\n\npost-hoc" {
		t.Errorf("reasoning = %q", m.ReasoningContent)
	}
	if again := persistedReply("See #note/x/y", info); strings.Count(again.Content, "#note/") != 1 {
		t.Errorf("link appended to a reply that already had one: %q", again.Content)
	}
}
