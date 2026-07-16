package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/sidecar"
	pb "github.com/familiar/gateway/proto/engine"
)

// resolveDecision is the index/fallback logic at the heart of the
// post-turn extract path — the seam the review flagged as untested and
// bug-prone. These lock in the two invariants: past-the-end indices
// default to ADD (never silently drop a fact), and an UPDATE with no
// explicit target falls back to the nearest neighbor.
func TestResolveDecision(t *testing.T) {
	decisions := []sidecar.BatchDecision{
		{Action: "ADD"},
		{Action: "UPDATE", TargetID: "explicit-target"},
		{Action: "UPDATE"}, // no target → neighbor fallback
		{Action: "DUPLICATE"},
		{Action: "UPDATE"}, // no target AND no neighbor → stays empty
	}

	cases := []struct {
		name        string
		i           int
		hasNeighbor bool
		neighborID  string
		wantAction  string
		wantTarget  string
	}{
		{"add", 0, false, "", "ADD", ""},
		{"update explicit target wins over neighbor", 1, true, "nbr-1", "UPDATE", "explicit-target"},
		{"update no target falls back to neighbor", 2, true, "nbr-2", "UPDATE", "nbr-2"},
		{"duplicate", 3, false, "", "DUPLICATE", ""},
		{"update no target no neighbor stays empty", 4, false, "", "UPDATE", ""},
		{"index past end defaults to ADD", 99, true, "nbr-x", "ADD", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action, target := resolveDecision(c.i, decisions, c.hasNeighbor, c.neighborID)
			if action != c.wantAction {
				t.Errorf("action = %q, want %q", action, c.wantAction)
			}
			if target != c.wantTarget {
				t.Errorf("targetID = %q, want %q", target, c.wantTarget)
			}
		})
	}
}

// supersedesFor is the guard that keeps a model-supplied target_id
// from hiding a live fact unless the action is a real UPDATE. A small
// local model routinely emits ADD (or unknown actions) with a target,
// so this must NOT pass it through as a Supersedes pointer.
// recoverBackground must swallow a panic so a detached extraction /
// summarization goroutine can't take the whole gateway down.
func TestRecoverBackground_SwallowsPanic(t *testing.T) {
	returnedNormally := false
	func() {
		defer recoverBackground("test")
		panic("boom from a model-output parser")
	}()
	returnedNormally = true // only reached if the panic was contained
	if !returnedNormally {
		t.Fatal("panic escaped recoverBackground")
	}

	// No panic → no-op, no false recovery.
	func() {
		defer recoverBackground("test")
	}()
}

func TestSupersedesFor(t *testing.T) {
	cases := []struct {
		action, target, want string
	}{
		{"UPDATE", "t-1", "t-1"},   // the only case that supersedes
		{"UPDATE", "", ""},         // update with no target: nothing to hide
		{"ADD", "t-2", ""},         // ADD with a stray target must not hide it
		{"DUPLICATE", "t-3", ""},   // duplicate never builds a fact anyway
		{"CONTRADICTS", "t-4", ""}, // unrecognized action falls through safely
	}
	for _, c := range cases {
		if got := supersedesFor(c.action, c.target); got != c.want {
			t.Errorf("supersedesFor(%q,%q) = %q, want %q", c.action, c.target, got, c.want)
		}
	}
}

// fakeVersioner records every RecordVersion call so the test can assert
// the created/superseded sequence.
type fakeVersioner struct {
	calls []verCall
	err   error
}

type verCall struct {
	memoryID, changeType string
}

func (f *fakeVersioner) RecordVersion(_ context.Context, memoryID, _, _, _, _, changeType string) error {
	f.calls = append(f.calls, verCall{memoryID: memoryID, changeType: changeType})
	return f.err
}

// recordVersions: an ADD (no Supersedes) writes one "created" on the new
// fact; an UPDATE (Supersedes set) writes "superseded" on the OLD memory
// then "created" on the new one.
func TestRecordVersions(t *testing.T) {
	facts := []*pb.FactProto{
		{Id: "fact-add", Content: "new fact"},                           // ADD
		{Id: "fact-upd", Content: "updated fact", Supersedes: "old-id"}, // UPDATE
	}
	v := &fakeVersioner{}
	recordVersions(context.Background(), v, facts)

	want := []verCall{
		{"fact-add", "created"},  // ADD → created on the new fact
		{"old-id", "superseded"}, // UPDATE → superseded on the old memory
		{"fact-upd", "created"},  // UPDATE → created on the new fact
	}
	if len(v.calls) != len(want) {
		t.Fatalf("recorded %d versions, want %d: %+v", len(v.calls), len(want), v.calls)
	}
	for i, w := range want {
		if v.calls[i] != w {
			t.Errorf("call %d = %+v, want %+v", i, v.calls[i], w)
		}
	}
}

// A versioner error must not panic or abort the batch — every fact still
// gets its calls attempted (best-effort, logged).
func TestRecordVersions_ErrorIsBestEffort(t *testing.T) {
	v := &fakeVersioner{err: context.DeadlineExceeded}
	facts := []*pb.FactProto{
		{Id: "a", Content: "x"},
		{Id: "b", Content: "y", Supersedes: "old"},
	}
	recordVersions(context.Background(), v, facts) // must not panic
	// ADD = 1 call, UPDATE = 2 calls → 3 total attempted despite errors.
	if len(v.calls) != 3 {
		t.Errorf("attempted %d calls, want 3 (errors must not short-circuit)", len(v.calls))
	}
}

func TestApproxTokenCount(t *testing.T) {
	turns := []sidecar.Turn{
		{Role: "user", Content: strings.Repeat("a", 100)},
		{Role: "assistant", Content: strings.Repeat("b", 100)},
	}
	// 200 chars / 4 = 50.
	if got := approxTokenCount(turns); got != 50 {
		t.Errorf("approxTokenCount = %d, want 50", got)
	}
	if got := approxTokenCount(nil); got != 0 {
		t.Errorf("approxTokenCount(nil) = %d, want 0", got)
	}
}

func TestPreviewString(t *testing.T) {
	if got := previewString("short", 200); got != "short" {
		t.Errorf("under-limit should pass through, got %q", got)
	}
	long := strings.Repeat("x", 300)
	got := previewString(long, 200)
	if len(got) > 210 { // 200 + an ellipsis affordance
		t.Errorf("over-limit preview not truncated: len=%d", len(got))
	}
}
