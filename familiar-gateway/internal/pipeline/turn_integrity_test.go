package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/llm"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
)

const integrityConv = "7d1e4c2a-9b3f-4a6e-8c5d-1f2e3a4b5c6d"

// integrityPipeline is a mock-LLM pipeline with one stub tool and a
// recording conversation store, and a workspace session on a
// conversation owned by alice.
func integrityPipeline(t *testing.T, mock *testutil.MockLLM, stub *stubSkill, maxIters int) (*Pipeline, *recordingConversations, *session.Session) {
	t.Helper()
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	pl := makePipelineWithIters(&mockEngine{}, mock, reg, maxIters)
	convs := &recordingConversations{}
	pl.conversations = convs
	sess := pl.sessions.GetOrCreateWithID(integrityConv, "workspace", "alice")
	sess.ClaimIdentity("workspace", "alice")
	return pl, convs, sess
}

func persistedRoles(c recordedAppend) string {
	var roles []string
	for _, m := range c.msgs {
		roles = append(roles, m.Role)
	}
	return strings.Join(roles, ",")
}

func sessionRoles(sess *session.Session) string {
	var roles []string
	for _, t := range sess.RecentTurns(0) {
		roles = append(roles, t.Role)
	}
	return strings.Join(roles, ",")
}

// stopMidRequest is a scripted reply that presses Stop while the model is
// still thinking (no content streamed yet) and then never answers.
func stopMidRequest(pl *Pipeline, conv string) testutil.ScriptedResponse {
	return testutil.ScriptedResponse{Before: func(r *http.Request) {
		pl.StopTurn(conv)
		<-r.Context().Done()
	}}
}

var lookupCall = testutil.ScriptedResponse{ToolCalls: []testutil.ScriptedToolCall{{
	ID: "call_1", Name: "stub_lookup", Arguments: map[string]any{"q": "page"},
}}}

// Stop pressed while the model reasons in the second iteration, after
// the first iteration's tool ran. The provider has no partial to return
// (reasoning isn't content), so the completion errored and the whole
// turn failed: no user turn, tool call, result or reply reached the
// session or the conversation, although the tool had run.
func TestStop_DuringReasoningAfterToolRan_RecordsTranscript(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	stub := &stubSkill{toolName: "stub_lookup", reply: "page rewritten"}
	pl, convs, sess := integrityPipeline(t, mock, stub, 5)
	mock.Enqueue(lookupCall, stopMidRequest(pl, integrityConv))

	text, info, err := pl.HandleStream(context.Background(), sess, "rewrite the page", nil,
		func(string) {}, func(string) {}, func(string) {})
	if err != nil {
		t.Fatalf("a Stop after a tool ran failed the turn: %v", err)
	}
	if stub.execCalls != 1 {
		t.Fatalf("tool ran %d times, want 1", stub.execCalls)
	}
	if !strings.HasPrefix(text, unfinishedNotePrefix) || !strings.Contains(text, "stopped") {
		t.Errorf("reply = %q, want the unfinished-turn note naming the Stop", text)
	}
	if info == nil || !info.Stopped {
		t.Error("turn not marked stopped")
	}
	if got := sessionRoles(sess); got != "user,assistant,tool,assistant" {
		t.Errorf("session = %s, want the user turn, the tool call and result, and the note", got)
	}
	convs.mu.Lock()
	defer convs.mu.Unlock()
	if len(convs.calls) != 1 || persistedRoles(convs.calls[0]) != "assistant,tool,assistant" {
		t.Fatalf("persisted %d writes (%v), want the tool call, its result and the note", len(convs.calls), convs.calls)
	}
}

// The same Stop when the first iteration also wrote prose: that prose is
// the partial answer, committed with the turn marked stopped.
func TestStop_DuringReasoningKeepsEarlierProse(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	stub := &stubSkill{toolName: "stub_lookup", reply: "page rewritten"}
	pl, convs, sess := integrityPipeline(t, mock, stub, 5)
	withProse := lookupCall
	withProse.Content = "Writing it now."
	mock.Enqueue(withProse, stopMidRequest(pl, integrityConv))

	text, info, err := pl.HandleStream(context.Background(), sess, "rewrite the page", nil,
		func(string) {}, func(string) {}, func(string) {})
	if err != nil {
		t.Fatalf("HandleStream: %v", err)
	}
	if text != "Writing it now." {
		t.Errorf("reply = %q, want the prose the user saw", text)
	}
	if !info.Stopped {
		t.Error("turn not marked stopped")
	}
	convs.mu.Lock()
	defer convs.mu.Unlock()
	if len(convs.calls) != 1 || persistedRoles(convs.calls[0]) != "assistant,tool,assistant" {
		t.Fatalf("persisted %v, want the tool call, its result and the reply", convs.calls)
	}
}

// A provider failure after a tool ran still errors the turn, but the
// tool call and its result are recorded, with a note for the reply.
func TestProviderErrorAfterToolRan_RecordsTranscript(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	stub := &stubSkill{toolName: "stub_lookup", reply: "page rewritten"}
	pl, convs, sess := integrityPipeline(t, mock, stub, 5)
	mock.Enqueue(lookupCall, testutil.ScriptedResponse{Status: http.StatusBadRequest})

	if _, _, err := pl.Handle(context.Background(), sess, "rewrite the page", nil); err == nil {
		t.Fatal("a failed completion reported success")
	}
	if got := sessionRoles(sess); got != "user,assistant,tool,assistant" {
		t.Errorf("session = %s, want the tool exchange and a note", got)
	}
	convs.mu.Lock()
	defer convs.mu.Unlock()
	if len(convs.calls) != 1 {
		t.Fatalf("persist calls = %d, want 1", len(convs.calls))
	}
	final := convs.calls[0].msgs[len(convs.calls[0].msgs)-1]
	if !strings.HasPrefix(final.Content, unfinishedNotePrefix) || !strings.Contains(final.Content, "failed") {
		t.Errorf("recorded reply = %q, want the unfinished-turn note", final.Content)
	}
}

// At the iteration cap the model is asked once more with tool_choice
// "none" and its answer is the reply. The loop used to return the last
// tool-calling response, usually with no text, so the turn ended empty
// and uncommitted.
func TestToolLimit_AsksForAnAnswerWithoutTools(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	stub := &stubSkill{toolName: "stub_lookup", reply: "more"}
	// A cap of 5 is above the default tier's web-search growth (budget+3),
	// so it is the cap the loop runs with.
	pl, _, sess := integrityPipeline(t, mock, stub, 5)
	for i := 0; i < 5; i++ {
		mock.Enqueue(lookupCall)
	}
	mock.Enqueue(testutil.ScriptedResponse{Content: "Here is what I found."})

	text, _, err := pl.Handle(context.Background(), sess, "dig", nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if text != "Here is what I found." {
		t.Fatalf("reply = %q, want the final no-tools answer", text)
	}
	calls := mock.Calls()
	var last struct {
		ToolChoice string `json:"tool_choice"`
	}
	_ = json.Unmarshal(calls[len(calls)-1].RawBody, &last)
	if last.ToolChoice != "none" {
		t.Errorf("final request tool_choice = %q, want none", last.ToolChoice)
	}
	if stub.execCalls != 5 {
		t.Errorf("tool ran %d times, want 5", stub.execCalls)
	}
}

// A server that ignores tool_choice and asks for a tool again: the call
// is not run, and the turn is recorded with a note.
func TestToolLimit_IgnoredToolChoiceRunsNothingMore(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	stub := &stubSkill{toolName: "stub_lookup", reply: "more"}
	pl, convs, sess := integrityPipeline(t, mock, stub, 5)
	for i := 0; i < 6; i++ {
		mock.Enqueue(lookupCall)
	}

	text, _, err := pl.Handle(context.Background(), sess, "dig", nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if stub.execCalls != 5 {
		t.Errorf("tool ran %d times, want 5 (the final call must not run)", stub.execCalls)
	}
	if !strings.Contains(text, "limit of tool calls") {
		t.Errorf("reply = %q, want the tool-limit note", text)
	}
	convs.mu.Lock()
	defer convs.mu.Unlock()
	want := strings.Repeat("assistant,tool,", 5) + "assistant"
	if len(convs.calls) != 1 || persistedRoles(convs.calls[0]) != want {
		t.Fatalf("persisted %v, want the five tool exchanges and the note", convs.calls)
	}
}

// A Stop that lands between iterations (while a tool runs) returns the
// turn's prose, not only the last iteration's: the digest written in the
// first iteration was dropped for the second's "Saving…".
func TestRunToolLoop_StopBetweenIterationsKeepsEarlierProse(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	stub := &cancellingSkill{stubSkill: stubSkill{toolName: "stub_lookup", reply: "ok"}, cancelOn: 2, cancel: cancel}
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	pl := makePipelineWithIters(&mockEngine{}, testutil.NewMockLLM(t), reg, 5)
	prose := []string{"The digest: three papers.", "Saving…"}
	call := 0
	complete := func(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
		call++
		return &llm.CompletionResponse{
			Content:   prose[call-1],
			ToolCalls: []llm.ToolCall{{ID: "c", Name: "stub_lookup", Arguments: json.RawMessage(`{}`)}},
		}, nil
	}
	baseReq := llm.CompletionRequest{Model: "mock-model", Tools: []llm.ToolSpec{{Name: "stub_lookup"}}}
	resp, _, err := pl.runToolLoop(ctx, baseReq, baseReq.Model, "standard", classifier.SearchNone, 0, complete, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if !strings.Contains(resp.Content, prose[0]) || !strings.Contains(resp.Content, prose[1]) {
		t.Errorf("reply = %q, want both iterations' prose", resp.Content)
	}
	if resp.FinishReason != "stopped" {
		t.Errorf("finish = %q, want stopped", resp.FinishReason)
	}
}

// cancellingSkill presses Stop while its cancelOn-th call runs.
type cancellingSkill struct {
	stubSkill
	cancelOn int
	cancel   context.CancelCauseFunc
}

func (c *cancellingSkill) Execute(ctx context.Context, name string, params json.RawMessage) (skills.ToolResult, error) {
	r, err := c.stubSkill.Execute(ctx, name, params)
	if c.execCalls == c.cancelOn {
		c.cancel(errUserStopped)
	}
	return r, err
}

// "thanks" after a tool turn is a trivial turn, which replays only the
// last exchange. It replayed the last two messages: the tool result and
// the reply, with the result's call cut off, so the request opened on an
// orphan tool message.
func TestTrivialTurn_ReplaysTheWholeLastExchange(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	stub := &stubSkill{toolName: "stub_lookup", reply: "sunny"}
	pl, _, sess := integrityPipeline(t, mock, stub, 5)
	mock.Enqueue(lookupCall, testutil.ScriptedResponse{Content: "It's sunny."},
		testutil.ScriptedResponse{Content: "You're welcome."})

	if _, _, err := pl.Handle(context.Background(), sess, "what's the weather?", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pl.Handle(context.Background(), sess, "thanks", nil); err != nil {
		t.Fatal(err)
	}
	calls := mock.Calls()
	var roles []string
	for _, m := range calls[len(calls)-1].Messages {
		if m.Role != "system" {
			roles = append(roles, m.Role)
		}
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool,assistant,user" {
		t.Fatalf("trivial turn sent %s, want the whole last exchange then the new message", got)
	}
}
