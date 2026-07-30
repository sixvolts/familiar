package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/sidecar"
	pb "github.com/familiar/gateway/proto/engine"
)

// resolveDecision decides what a batch-classifier verdict is allowed to
// do. Every read path HIDES a superseded row, so a wrong target here is
// silent data loss — and the classifier is a small local model shown
// exactly one existing fact per candidate, so the only id it may name is
// that neighbour's.
//
// These cases previously asserted the opposite contract: an explicit target
// was honoured verbatim, an UPDATE with no target superseded the neighbour
// at any similarity, and a DUPLICATE with no neighbour dropped the write.
// Each is now a rejection, and each rejection is a fact that stays visible.
const testFloor = 0.75

func nbr(id string, sim float64) memory.NearestFact {
	return memory.NearestFact{ID: id, Content: "existing fact", Similarity: sim}
}

func TestResolveDecision(t *testing.T) {
	cases := []struct {
		name        string
		decision    sidecar.BatchDecision
		neighbor    memory.NearestFact
		hasNeighbor bool
		wantAction  string
		wantTarget  string
		why         string
	}{
		{
			name: "add passes through", decision: sidecar.BatchDecision{Action: "ADD"},
			wantAction: "ADD", wantTarget: "",
		},
		{
			name:     "update naming the neighbour is honoured",
			decision: sidecar.BatchDecision{Action: "UPDATE", TargetID: "nbr-1"},
			neighbor: nbr("nbr-1", 0.90), hasNeighbor: true,
			wantAction: "UPDATE", wantTarget: "nbr-1",
			why: "the neighbour is the one row the model was shown",
		},
		{
			name:     "update naming a DIFFERENT id is refused",
			decision: sidecar.BatchDecision{Action: "UPDATE", TargetID: "some-other-uuid"},
			neighbor: nbr("nbr-1", 0.90), hasNeighbor: true,
			wantAction: "ADD", wantTarget: "",
			why: "a hallucinated or cross-candidate target would bury a row nobody looked at",
		},
		{
			name:        "update naming a target with no neighbour at all is refused",
			decision:    sidecar.BatchDecision{Action: "UPDATE", TargetID: "invented"},
			hasNeighbor: false,
			wantAction:  "ADD", wantTarget: "",
		},
		{
			name:     "update with no target supersedes only above the floor",
			decision: sidecar.BatchDecision{Action: "UPDATE"},
			neighbor: nbr("nbr-2", 0.88), hasNeighbor: true,
			wantAction: "UPDATE", wantTarget: "nbr-2",
		},
		{
			name:     "update with no target below the floor adds instead",
			decision: sidecar.BatchDecision{Action: "UPDATE"},
			neighbor: nbr("dog-fact", 0.20), hasNeighbor: true,
			wantAction: "ADD", wantTarget: "",
			why: "this is the reported bug: dark-mode preference burying the dog's name",
		},
		{
			name:        "update with no target and no neighbour adds",
			decision:    sidecar.BatchDecision{Action: "UPDATE"},
			hasNeighbor: false,
			wantAction:  "ADD", wantTarget: "",
		},
		{
			name:     "duplicate of the neighbour is honoured",
			decision: sidecar.BatchDecision{Action: "DUPLICATE", TargetID: "nbr-3"},
			neighbor: nbr("nbr-3", 0.80), hasNeighbor: true,
			wantAction: "DUPLICATE", wantTarget: "nbr-3",
		},
		{
			name:     "duplicate naming a different id keeps the write",
			decision: sidecar.BatchDecision{Action: "DUPLICATE", TargetID: "invented"},
			neighbor: nbr("nbr-3", 0.80), hasNeighbor: true,
			wantAction: "ADD", wantTarget: "",
			why: "dropping a write on a hallucinated id contradicts this path's prefer-a-duplicate rule",
		},
		{
			name:        "duplicate with no neighbour keeps the write",
			decision:    sidecar.BatchDecision{Action: "DUPLICATE"},
			hasNeighbor: false,
			wantAction:  "ADD", wantTarget: "",
			why: "a duplicate of nothing is not a duplicate",
		},
		{
			name:     "unrecognised action carrying a bad target loses it",
			decision: sidecar.BatchDecision{Action: "CONTRADICTS", TargetID: "invented"},
			neighbor: nbr("nbr-4", 0.90), hasNeighbor: true,
			wantAction: "CONTRADICTS", wantTarget: "",
			why: "supersedesFor only acts on UPDATE, but the target must not survive either",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action, target := resolveDecision(0, []sidecar.BatchDecision{c.decision},
				c.neighbor, c.hasNeighbor, testFloor)
			if action != c.wantAction || target != c.wantTarget {
				t.Errorf("got (%q, %q), want (%q, %q)%s",
					action, target, c.wantAction, c.wantTarget,
					func() string {
						if c.why != "" {
							return " — " + c.why
						}
						return ""
					}())
			}
		})
	}
}

// A missing decision (the model returned fewer than len(prep)) must never
// silently drop a fact.
func TestResolveDecisionPastEndDefaultsToAdd(t *testing.T) {
	action, target := resolveDecision(99, []sidecar.BatchDecision{{Action: "UPDATE", TargetID: "x"}},
		nbr("nbr", 0.99), true, testFloor)
	if action != "ADD" || target != "" {
		t.Errorf("got (%q, %q), want (ADD, \"\")", action, target)
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
