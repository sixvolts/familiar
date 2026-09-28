package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/llm"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
)

// The tool loop echoes each assistant message with its reasoning, so a
// formatter that keeps the current turn's reasoning (Qwen) has it.
func TestToolLoop_EchoesReasoning(t *testing.T) {
	stub := &stubSkill{toolName: "stub_lookup", reply: "ok"}
	reg := skills.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	pl := makePipelineWithIters(&mockEngine{}, testutil.NewMockLLM(t), reg, 5)
	var second llm.CompletionRequest
	call := 0
	complete := func(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
		call++
		if call == 1 {
			return &llm.CompletionResponse{ReasoningContent: "Plan: look it up.",
				ToolCalls: []llm.ToolCall{{ID: "c", Name: "stub_lookup", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		second = req
		return &llm.CompletionResponse{Content: "done"}, nil
	}
	baseReq := llm.CompletionRequest{Model: "mock-model", Tools: []llm.ToolSpec{{Name: "stub_lookup"}}}
	if _, _, err := pl.runToolLoop(context.Background(), baseReq, baseReq.Model, "standard", classifier.SearchNone, 0, complete, nil, nil, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	var echoed *llm.Message
	for i := range second.Messages {
		if len(second.Messages[i].ToolCalls) > 0 {
			echoed = &second.Messages[i]
		}
	}
	if echoed == nil || echoed.ReasoningContent != "Plan: look it up." {
		t.Fatalf("echoed assistant message = %+v, want its reasoning", echoed)
	}
}

// One tool call with malformed arguments no longer drops every call of
// the turn from what is saved.
func TestMarshalToolCalls_KeepsCallsWithBadArguments(t *testing.T) {
	b := marshalToolCalls([]llm.ToolCall{
		{ID: "a", Name: "good", Arguments: json.RawMessage(`{"q":"x"}`)},
		{ID: "b", Name: "bad", Arguments: json.RawMessage(`{"q": "unterm`)},
	})
	var got []llm.ToolCall
	if err := json.Unmarshal(b, &got); err != nil || len(got) != 2 {
		t.Fatalf("saved %s (%v), want both calls", b, err)
	}
	if string(got[0].Arguments) != `{"q":"x"}` {
		t.Errorf("good call's arguments changed: %s", got[0].Arguments)
	}
}

// A turn that failed mid-loop records the prose it streamed alongside
// its tool calls in the note: the conversation shows the reply, not the
// tool-call rows, so the prose would otherwise vanish on reload.
func TestProviderErrorAfterToolRan_KeepsTheProse(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	stub := &stubSkill{toolName: "stub_lookup", reply: "page rewritten"}
	pl, convs, sess := integrityPipeline(t, mock, stub, 5)
	withProse := lookupCall
	withProse.Content = "Rewriting the intro now."
	mock.Enqueue(withProse, testutil.ScriptedResponse{Status: http.StatusBadGateway})
	if _, _, err := pl.Handle(context.Background(), sess, "rewrite the page", nil); err == nil {
		t.Fatal("a failed completion reported success")
	}
	convs.mu.Lock()
	defer convs.mu.Unlock()
	final := convs.calls[0].msgs[len(convs.calls[0].msgs)-1]
	if !strings.HasPrefix(final.Content, "Rewriting the intro now.\n\n"+unfinishedNotePrefix) {
		t.Errorf("recorded reply = %q, want the prose then the note", final.Content)
	}
}

// A summary finishing after its conversation was deleted (the session
// dropped from the manager) isn't saved: it would bring the deleted
// chat's summary back.
func TestSummarize_NotSavedForADroppedSession(t *testing.T) {
	sums := &fakeSummaries{}
	pl, sess := hydratePipeline(t, testutil.NewMockLLM(t), sums, &fakeConversation{})
	sc := newFakeSidecar(t, func(string) string {
		pl.sessions.Delete(sess.ID)
		return "They discussed the homelab servers and their backups."
	})
	pl.sidecarClient = sidecarFor(sc, sidecar.TaskSummarize)
	sess.SetSummary("", 0)
	for i := 0; i < VerbatimWindow+4; i++ {
		sess.AddTurn("user", fmt.Sprintf("t%d", i))
	}
	pl.runSummarize(sess, nil)
	if sums.saves != 0 {
		t.Errorf("summary saved %d times for a deleted conversation", sums.saves)
	}
}

// A turn in progress keeps its session registered through an eviction
// sweep, so Stop and status still find it.
func TestTurn_SessionNotEvictedWhileRunning(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	pl := makePipelineWithMockLLM(&mockEngine{}, mock, skills.NewRegistry())
	sess := pl.sessions.GetOrCreateWithID(integrityConv, "workspace", "alice")
	evicted := -1
	mock.Enqueue(testutil.ScriptedResponse{Content: "ok", Before: func(*http.Request) {
		sess.LastActive = time.Now().Add(-time.Hour) // quiet for an hour before this turn
		evicted = pl.sessions.EvictIdle(time.Minute)
	}})
	if _, _, err := pl.Handle(context.Background(), sess, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if evicted != 0 {
		t.Errorf("the sweep during the turn evicted %d session(s)", evicted)
	}
}
