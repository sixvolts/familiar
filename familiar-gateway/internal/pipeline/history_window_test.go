package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
	pb "github.com/familiar/gateway/proto/engine"
)

// toolHeavySession holds a user message, three tool calls with results,
// and the reply: U1, a(tc), t, a(tc), t, a(tc), t, A1.
func toolHeavySession(pl *Pipeline) *session.Session {
	sess := pl.sessions.GetOrCreate("cli", "user1")
	sess.AddTurn("user", "check the nginx config on gpu-host")
	for i := 0; i < 3; i++ {
		sess.AddMessage(session.Turn{Role: "assistant", ToolCalls: []byte(`[{"id":"c","name":"read_page","arguments":{}}]`)})
		sess.AddMessage(session.Turn{Role: "tool", ToolCallID: "c", Content: "server { listen 80; }"})
	}
	sess.AddTurn("assistant", "The config listens on port 80 with a 60s timeout.")
	return sess
}

type classifyReq struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// After a tool-heavy turn the classifier gets the conversation, not the
// turn's tool plumbing: its last six messages were tool results and empty
// tool-call stubs, so the user message "what about the timeout?" refers
// to was gone and the stubs arrived as empty assistant messages.
func TestClassifier_HistorySkipsToolRows(t *testing.T) {
	pl := makePipelineWithMockLLM(&mockEngine{}, testutil.NewMockLLM(t), skills.NewRegistry())
	sc := newFakeSidecar(t, func(string) string {
		return `{"thinking":"low","memory_depth":"shallow","search_depth":"none","condensed_query":""}`
	})
	pl.sidecarClient = sidecarFor(sc, sidecar.TaskClassify)
	sess := toolHeavySession(pl)
	if _, err := pl.classifyRequest(context.Background(), sess, "what about the timeout?", nil); err != nil {
		t.Fatal(err)
	}
	var req classifyReq
	_ = json.Unmarshal([]byte(<-sc.arrived), &req)
	var roles []string
	for _, m := range req.Messages[1:] { // after the system prompt
		if strings.TrimSpace(m.Content) == "" {
			t.Errorf("empty %s message sent to the classifier", m.Role)
		}
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,user" {
		t.Errorf("classifier messages = %s, want the prior exchange then the new message", got)
	}
	if !strings.Contains(req.Messages[1].Content, "nginx config") {
		t.Errorf("first history message = %q, want the user's request", req.Messages[1].Content)
	}
	if got := req.Messages[2].Content; got != "The config listens on port 80 with a 60s timeout." {
		t.Errorf("reply sent as %q, want the reply alone", got)
	}
}

// The extractor's "prior turns" are the conversation before this turn.
// They were the session's last 8 messages minus this turn's pair: after a
// tool-heavy turn, this turn's own tool results.
func TestExtractor_ContextIsThePriorConversation(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	stub := &stubSkill{toolName: "stub_lookup", reply: "gpu-host: 32GB"}
	pl, _, sess := integrityPipeline(t, mock, stub, 5)
	sc := newFakeSidecar(t, func(string) string { return `{"facts":[],"relationships":[]}` })
	pl.sidecarClient = sidecarFor(sc, sidecar.TaskExtract)
	sess.AddTurn("user", "gpu-host has 32GB")
	sess.AddTurn("assistant", "Noted: gpu-host has 32GB.")
	mock.Enqueue(lookupCall, lookupCall, lookupCall, testutil.ScriptedResponse{Content: "Updated the hardware page to 64GB."})

	if _, _, err := pl.Handle(context.Background(), sess, "bump it to 64GB and update the hardware page", nil); err != nil {
		t.Fatal(err)
	}
	var req classifyReq
	_ = json.Unmarshal([]byte(<-sc.arrived), &req)
	prompt := req.Messages[len(req.Messages)-1].Content
	ctxBlock := prompt[strings.Index(prompt, "<context>"):strings.Index(prompt, "</context>")]
	if !strings.Contains(ctxBlock, "user: gpu-host has 32GB") || strings.Contains(ctxBlock, "gpu-host: 32GB") ||
		strings.Contains(ctxBlock, "bump it to 64GB") {
		t.Errorf("extractor context = %q, want the prior exchange and none of this turn's tool results", ctxBlock)
	}
}

// "perfect" answering a pending question reaches the classifier: the
// trivial verdict strips the tools the yes needs.
func TestFastPath_NotWhenTheReplyAskedAQuestion(t *testing.T) {
	pl := makePipelineWithMockLLM(&mockEngine{}, testutil.NewMockLLM(t), skills.NewRegistry())
	sc := newFakeSidecar(t, func(string) string {
		return `{"thinking":"low","memory_depth":"shallow","search_depth":"none","condensed_query":""}`
	})
	pl.sidecarClient = sidecarFor(sc, sidecar.TaskClassify)
	sess := pl.sessions.GetOrCreate("cli", "user1")
	sess.AddTurn("user", "draft a page about the backup plan")
	sess.AddTurn("assistant", "I drafted it. Want me to publish it to your Recipes book?")
	route, err := pl.classifyRequest(context.Background(), sess, "thanks!", nil)
	if err != nil {
		t.Fatal(err)
	}
	if route.classifier.Source == classifier.SourceFastPath {
		t.Error("a reply to a question took the trivial fast path")
	}
}

// The backstop firing during the batched conflict pass commits the
// extracted candidates as ADD. It dropped them: "my new server is at
// 10.0.0.30" was never stored whenever extraction plus the batch ran long.
func TestPostTurnExtract_DeadlineInTheBatchStillCommits(t *testing.T) {
	old := postTurnDeadline
	postTurnDeadline = 300 * time.Millisecond
	defer func() { postTurnDeadline = old }()

	eng := &ctxEngine{}
	pl := makePipelineWithMockLLM(&eng.mockEngine, testutil.NewMockLLM(t), skills.NewRegistry())
	pl.engine = eng
	sc := newFakeSidecar(t, func(body string) string {
		if strings.Contains(body, "resolve fact conflicts") {
			time.Sleep(600 * time.Millisecond)
			return `{"decisions":[],"relationships":[]}`
		}
		return `{"facts":[{"content":"the new server is at 10.0.0.30","category":"configuration"}],"relationships":[]}`
	})
	pl.sidecarClient = sidecarFor(sc, sidecar.TaskExtract, sidecar.TaskConflict)
	sess := pl.sessions.GetOrCreate("cli", "user1")
	pl.runPostTurnExtract(sess, "my new server is at 10.0.0.30", "Noted.", nil, nil, nil)
	if len(eng.commitFacts) != 1 || eng.commitFacts[0].Content != "the new server is at 10.0.0.30" {
		t.Fatalf("committed %d facts, want the extracted one", len(eng.commitFacts))
	}
}

// ctxEngine refuses a commit on a done context, as the real one does.
type ctxEngine struct{ mockEngine }

func (e *ctxEngine) CommitFacts(ctx context.Context, sessionID string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.mockEngine.CommitFacts(ctx, sessionID, facts)
}

// With the embedder down, memory search runs keyword-only instead of not
// at all: an outage removed long-term memory from every turn, although
// the package doc promised a keyword fallback.
func TestRetrieval_KeywordOnlyWhenTheEmbedderIsDown(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	mock.Enqueue(testutil.ScriptedResponse{Content: "Biscuit."})
	pl := makePipelineWithMockLLM(&mockEngine{}, mock, skills.NewRegistry())
	pl.memStore = newRecordingStore(map[string][]memory.MemoryResult{
		"user1": {{Content: "the dog is named Biscuit"}},
	})
	pl.embedder = func(context.Context, string) ([]float32, error) { return nil, errors.New("embedder down") }
	sess := pl.sessions.GetOrCreate("cli", "user1")
	if _, _, err := pl.Handle(context.Background(), sess, "what is my dog called", nil); err != nil {
		t.Fatal(err)
	}
	sys := recordedSystemMsg(mock.Calls()[0].Messages)
	if !strings.Contains(sys, "Biscuit") {
		t.Errorf("no memory in the prompt with the embedder down:\n%s", sys)
	}
}
