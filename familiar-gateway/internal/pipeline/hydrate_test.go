package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/testutil"
)

// fakeSummaries is a SummaryStore that can be made to fail.
type fakeSummaries struct {
	mu      sync.Mutex
	summary string
	count   int
	fail    bool
	saves   int
}

func (f *fakeSummaries) Load(ctx context.Context, _ string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if f.fail {
		return "", 0, errors.New("pool exhausted")
	}
	return f.summary, f.count, nil
}

func (f *fakeSummaries) Save(_ context.Context, _, summary string, count int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summary, f.count = summary, count
	f.saves++
	return nil
}

// fakeConversation is a conversation store holding one conversation's
// rows, which appends grow as the real table does.
type fakeConversation struct {
	mu   sync.Mutex
	rows []session.Turn
	fail bool
}

func (f *fakeConversation) LoadRecentTurns(ctx context.Context, _, _ string, limit, skip int, visit func(string, string, []byte, string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.fail {
		return errors.New("connection reset")
	}
	rows := f.rows
	if skip < len(rows) {
		rows = rows[skip:]
	} else {
		rows = nil
	}
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	for _, r := range rows {
		visit(r.Role, r.Content, r.ToolCalls, r.ToolCallID)
	}
	return nil
}

func (f *fakeConversation) AppendIntermediateMessages(_ context.Context, _, _ string, msgs []IntermediateMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		f.rows = append(f.rows, session.Turn{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID})
	}
	return nil
}

// userSaves is what the client does before a turn: save the prompt.
func (f *fakeConversation) userSaves(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, session.Turn{Role: "user", Content: msg})
}

// history is n alternating user/assistant rows, "m1".."mn".
func history(n int) []session.Turn {
	out := make([]session.Turn, n)
	for i := range out {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out[i] = session.Turn{Role: role, Content: fmt.Sprintf("m%d", i+1)}
	}
	return out
}

func hydratePipeline(t *testing.T, mock *testutil.MockLLM, sums *fakeSummaries, conv *fakeConversation) (*Pipeline, *session.Session) {
	t.Helper()
	pl := makePipelineWithMockLLM(&mockEngine{}, mock, skills.NewRegistry())
	pl.sessionStore = sums
	pl.conversations = conv
	sess := pl.sessions.GetOrCreateWithID(integrityConv, "workspace", "alice")
	sess.ClaimIdentity("workspace", "alice")
	return pl, sess
}

func sentUserContents(call testutil.RecordedCall) []string {
	var out []string
	for _, m := range call.Messages {
		if m.Role == "user" {
			out = append(out, m.Content)
		}
	}
	return out
}

// A failed summary load leaves the session unhydrated (retried next
// turn) and blocks summarizing; the next turn's load succeeds and brings
// the history back. It used to be marked hydrated before loading, so
// the history never came back in that process, and the next summary
// replaced the persisted one.
func TestHydrate_FailedLoadIsRetried(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	sums := &fakeSummaries{summary: "They chose Postgres.", count: 2, fail: true}
	conv := &fakeConversation{rows: history(6)}
	pl, sess := hydratePipeline(t, mock, sums, conv)
	mock.Enqueue(testutil.ScriptedResponse{Content: "a1"}, testutil.ScriptedResponse{Content: "a2"})

	conv.userSaves("first")
	if _, _, err := pl.Handle(context.Background(), sess, "first", nil); err != nil {
		t.Fatal(err)
	}
	if sess.IsHydrated() || !sess.SummaryUnknown() {
		t.Fatalf("after a failed load: hydrated=%v summaryUnknown=%v, want false/true", sess.IsHydrated(), sess.SummaryUnknown())
	}

	sums.mu.Lock()
	sums.fail = false
	sums.mu.Unlock()
	conv.userSaves("second")
	if _, _, err := pl.Handle(context.Background(), sess, "second", nil); err != nil {
		t.Fatal(err)
	}
	if !sess.IsHydrated() {
		t.Fatal("not hydrated after a successful load")
	}
	if summary, _ := sess.Snapshot(); summary != "They chose Postgres." {
		t.Errorf("summary = %q, want the persisted one", summary)
	}
	// The retry replaced the degraded turn's buffer with the
	// conversation, which also holds that turn: m3..m6 (after the two
	// summarized), then first/a1, then this turn.
	var got []string
	for _, turn := range sess.RecentTurns(0) {
		got = append(got, turn.Content)
	}
	if want := "m3,m4,m5,m6,first,a1,second,a2"; strings.Join(got, ",") != want {
		t.Errorf("session = %s, want %s", strings.Join(got, ","), want)
	}
}

// A client that disconnects while the load runs no longer cancels it.
func TestHydrate_ClientDisconnectDoesNotCancelTheLoad(t *testing.T) {
	sums := &fakeSummaries{summary: "earlier", count: 0}
	conv := &fakeConversation{rows: history(4)}
	pl, sess := hydratePipeline(t, testutil.NewMockLLM(t), sums, conv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pl.hydrateSession(ctx, sess, "next")
	if !sess.IsHydrated() || sess.TurnCount() != 4 {
		t.Fatalf("hydrated=%v turns=%d after a disconnect, want true/4", sess.IsHydrated(), sess.TurnCount())
	}
}

// Hydration replays only the messages the summary doesn't cover, and not
// this turn's own message, which the client saved first. It replayed the
// last 100 rows: the summarized ones twice (verbatim and in the summary)
// and the new message twice (loaded, then appended as the prompt).
func TestHydrate_ReplaysOnlyUnsummarizedTurnsAndNotThePrompt(t *testing.T) {
	mock := testutil.NewMockLLM(t)
	sums := &fakeSummaries{summary: "m1 to m6 happened.", count: 6}
	conv := &fakeConversation{rows: history(10)}
	pl, sess := hydratePipeline(t, mock, sums, conv)
	mock.Enqueue(testutil.ScriptedResponse{Content: "ok"})

	conv.userSaves("what now?")
	if _, _, err := pl.Handle(context.Background(), sess, "what now?", nil); err != nil {
		t.Fatal(err)
	}
	sent := sentUserContents(mock.Calls()[0])
	if got := strings.Join(sent, ","); got != "m7,m9,what now?" {
		t.Errorf("user messages sent = %s, want m7,m9,what now?", got)
	}
}

// A summary that claims more messages than the conversation has
// (messages were deleted) replays the most recent ones rather than none.
func TestHydrate_SummaryPastTheEndReplaysTheTail(t *testing.T) {
	sums := &fakeSummaries{summary: "lots", count: 40}
	conv := &fakeConversation{rows: history(6)}
	pl, sess := hydratePipeline(t, testutil.NewMockLLM(t), sums, conv)
	pl.hydrateSession(context.Background(), sess, "next")
	if sess.TurnCount() != 6 {
		t.Errorf("turns = %d, want the 6 there are", sess.TurnCount())
	}
}

// While the summary is unknown the session isn't summarized, so nothing
// overwrites the persisted summary.
func TestSummarize_WaitsForTheSummaryToLoad(t *testing.T) {
	sums := &fakeSummaries{fail: true}
	pl, sess := hydratePipeline(t, testutil.NewMockLLM(t), sums, &fakeConversation{})
	sc := newFakeSidecar(t, func(string) string { return "A new summary of the whole conversation so far." })
	pl.sidecarClient = sidecarFor(sc, sidecar.TaskSummarize)
	pl.hydrateSession(context.Background(), sess, "x")
	for i := 0; i < VerbatimWindow+4; i++ {
		sess.AddTurn("user", fmt.Sprintf("t%d", i))
	}
	pl.maybeSummarize(sess, nil)
	select {
	case <-sc.arrived:
		t.Fatal("summarized while the persisted summary was unknown")
	case <-time.After(300 * time.Millisecond):
	}
}
