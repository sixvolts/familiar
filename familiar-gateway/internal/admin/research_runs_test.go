package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/testdsn"
	"github.com/familiar/gateway/internal/testutil"
)

// runStoreForTest returns a ResearchRunStore over a dedicated schema
// plus a seeded owner (the FK target).
func runStoreForTest(t *testing.T) (*ResearchRunStore, string) {
	t.Helper()
	dsn := os.Getenv(testutil.EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s not set", testutil.EnvDSN)
	}
	ctx := context.Background()
	adminPool, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = adminPool.Close() })
	if _, err := adminPool.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS research_run_test`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	pool, err := db.Open(testdsn.Scoped(t, dsn, "research_run_test"))
	if err != nil {
		t.Fatalf("db.Open (scoped): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// research_run_test is created IF NOT EXISTS and never dropped, so rows
	// from previous runs persist — including runs left ACTIVE, which the next
	// run's FailOrphaned sweeps. Tests that count or list runs then depend on
	// what a sibling happened to leave behind. Start each run from empty.
	if _, err := pool.ExecContext(ctx, `TRUNCATE research_runs CASCADE`); err != nil {
		t.Fatalf("truncate research_runs: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role)
		VALUES ('ru', 'Run User', 'approved', 'user') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return NewResearchRunStore(pool), "ru"
}

// qs makes n placeholder sub-questions for Create when the exact text
// doesn't matter (only the roster length / count does).
func qs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "q" + string(rune('a'+i))
	}
	return out
}

func TestResearchRunStore_Lifecycle(t *testing.T) {
	store, user := runStoreForTest(t)
	ctx := context.Background()
	conv := "conv-abc"

	// No active run yet.
	if _, err := store.ActiveForConversation(ctx, user, conv); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("active on empty = %v, want ErrRunNotFound", err)
	}

	run, err := store.Create(ctx, user, conv, "Meow Wolf history",
		[]string{"origins", "founders", "expansion", "art style", "finances"}, "research:ru", "research-meow-wolf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if run.Status != RunStatusResearching || run.Round != 1 || run.WorkersTotal != 5 {
		t.Errorf("fresh run = %+v", run)
	}

	// Now it's the active run.
	active, err := store.ActiveForConversation(ctx, user, conv)
	if err != nil || active.ID != run.ID {
		t.Fatalf("ActiveForConversation = %v / %+v", err, active)
	}
	// Not visible to another user.
	if _, err := store.ActiveForConversation(ctx, "someone-else", conv); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("active leaked across users: %v", err)
	}

	// Advance: round 2, workers done, then synthesizing.
	two := 2
	done := 5
	syn := RunStatusSynthesizing
	if err := store.Update(ctx, run.ID, RunPatch{Round: &two, WorkersDone: &done, Status: &syn}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	active, _ = store.ActiveForConversation(ctx, user, conv)
	if active.Round != 2 || active.WorkersDone != 5 || active.Status != RunStatusSynthesizing {
		t.Errorf("after update = %+v", active)
	}

	// Terminal: done + note location → no longer active.
	fin := RunStatusDone
	nb, np := "personal:ru", "research-meow-wolf"
	if err := store.Update(ctx, run.ID, RunPatch{Status: &fin, NoteBookSlug: &nb, NotePageSlug: &np}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := store.ActiveForConversation(ctx, user, conv); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("done run still active: %v", err)
	}
	got, err := store.Get(ctx, run.ID)
	if err != nil || got.NotePageSlug != np || got.Status != RunStatusDone {
		t.Errorf("Get after done = %v / %+v", err, got)
	}
}

// The partial-unique index enforces one active run per conversation:
// a second Create while one is active returns ErrActiveRunExists (the
// atomic backstop for the kickoff check-then-create race).
func TestResearchRunStore_OneActivePerConversation(t *testing.T) {
	store, user := runStoreForTest(t)
	ctx := context.Background()
	conv := "conv-unique"

	if _, err := store.Create(ctx, user, conv, "A", qs(3), "research:ru", "ev-a"); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := store.Create(ctx, user, conv, "B", qs(3), "research:ru", "ev-b"); !errors.Is(err, ErrActiveRunExists) {
		t.Fatalf("second Create = %v, want ErrActiveRunExists", err)
	}
	// After the first finishes, a new run is allowed again.
	active, _ := store.ActiveForConversation(ctx, user, conv)
	done := RunStatusDone
	if err := store.Update(ctx, active.ID, RunPatch{Status: &done}); err != nil {
		t.Fatalf("finish first: %v", err)
	}
	if _, err := store.Create(ctx, user, conv, "C", qs(3), "research:ru", "ev-c"); err != nil {
		t.Fatalf("Create after prior finished: %v", err)
	}
}

// FailOrphanedRuns marks every non-terminal run failed (restart
// reconciliation), leaving terminal runs untouched.
func TestResearchRunStore_FailOrphaned(t *testing.T) {
	store, user := runStoreForTest(t)
	ctx := context.Background()

	r1, _ := store.Create(ctx, user, "c1", "A", qs(2), "research:ru", "e1")
	r2, _ := store.Create(ctx, user, "c2", "B", qs(2), "research:ru", "e2")
	done := RunStatusDone
	_ = store.Update(ctx, r2.ID, RunPatch{Status: &done}) // already terminal

	// FailOrphanedRuns is store-wide; the shared test schema may carry
	// active runs from sibling tests, so assert per-run behavior rather
	// than a global count.
	orphans, err := store.FailOrphanedRuns(ctx, "restart")
	if err != nil {
		t.Fatalf("FailOrphanedRuns: %v", err)
	}
	// The reconciled runs come back so their conversations can be told
	// (the card just vanished before).
	var sawR1 bool
	for _, o := range orphans {
		if o.ID == r2.ID {
			t.Error("a finished run was returned as orphaned")
		}
		if o.ID == r1.ID {
			sawR1 = o.ConversationID == "c1" && o.Topic == "A" && o.Status == RunStatusFailed
		}
	}
	if !sawR1 {
		t.Errorf("orphan r1 not returned with its conversation and topic: %+v", orphans)
	}
	got1, _ := store.Get(ctx, r1.ID)
	if got1.Status != RunStatusFailed || got1.Error != "restart" {
		t.Errorf("orphan not failed: %+v", got1)
	}
	got2, _ := store.Get(ctx, r2.ID)
	if got2.Status != RunStatusDone {
		t.Errorf("terminal run was disturbed: %+v", got2)
	}
	// No active runs remain → conversation unblocked.
	if _, err := store.ActiveForConversation(ctx, user, "c1"); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("c1 still has an active run after reconcile: %v", err)
	}
}

// Create seeds a queued roster from the questions, and SetWorkerState
// transitions entries by stable index (the card's data source).
func TestResearchRunStore_WorkerRoster(t *testing.T) {
	store, user := runStoreForTest(t)
	ctx := context.Background()
	conv := "conv-roster"
	// The test schema persists across runs; clear any active run a prior
	// run left behind so Create isn't blocked by the one-active guard.
	fin := RunStatusDone
	if prev, err := store.ActiveForConversation(ctx, user, conv); err == nil {
		_ = store.Update(ctx, prev.ID, RunPatch{Status: &fin})
	}

	run, err := store.Create(ctx, user, conv, "T",
		[]string{"origins", "evolution", "modern era"}, "research:ru", "ev-r")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = store.Update(ctx, run.ID, RunPatch{Status: &fin}) })
	// Fresh roster: one queued area per question, in order.
	if len(run.Workers) != 3 {
		t.Fatalf("roster len = %d, want 3 (%+v)", len(run.Workers), run.Workers)
	}
	if run.Workers[0].Question != "origins" || run.Workers[0].State != WorkerQueued {
		t.Errorf("worker[0] = %+v, want {origins queued}", run.Workers[0])
	}

	// Transition by stable index.
	if err := store.SetWorkerState(ctx, run.ID, 0, WorkerActive); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if err := store.SetWorkerState(ctx, run.ID, 0, WorkerDone); err != nil {
		t.Fatalf("set done: %v", err)
	}
	if err := store.SetWorkerState(ctx, run.ID, 2, WorkerFailed); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	// Out-of-range index is a silent no-op, not an error.
	if err := store.SetWorkerState(ctx, run.ID, 9, WorkerDone); err != nil {
		t.Errorf("out-of-range idx should be a no-op, got %v", err)
	}

	got, err := store.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Workers[0].State != WorkerDone {
		t.Errorf("worker[0] state = %q, want done", got.Workers[0].State)
	}
	if got.Workers[1].State != WorkerQueued {
		t.Errorf("worker[1] state = %q, want still queued", got.Workers[1].State)
	}
	if got.Workers[2].State != WorkerFailed {
		t.Errorf("worker[2] state = %q, want failed", got.Workers[2].State)
	}
	// Question text is preserved across state writes.
	if got.Workers[2].Question != "modern era" {
		t.Errorf("worker[2] question = %q, want 'modern era'", got.Workers[2].Question)
	}
}

// UpdateIfActive is compare-and-set: it applies while the run is
// researching/synthesizing but no-ops once terminal — so a lifecycle
// transition can't revert a 'failed' a concurrent cancel wrote.
func TestResearchRunStore_UpdateIfActive(t *testing.T) {
	store, user := runStoreForTest(t)
	ctx := context.Background()
	fin := RunStatusDone
	if prev, err := store.ActiveForConversation(ctx, user, "conv-cas"); err == nil {
		_ = store.Update(ctx, prev.ID, RunPatch{Status: &fin})
	}
	run, err := store.Create(ctx, user, "conv-cas", "T", qs(2), "research:ru", "ev")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = store.Update(ctx, run.ID, RunPatch{Status: &fin}) })

	// Active → applies.
	syn := RunStatusSynthesizing
	if applied, err := store.UpdateIfActive(ctx, run.ID, RunPatch{Status: &syn}); err != nil || !applied {
		t.Fatalf("UpdateIfActive on active = (%v, %v), want (true, nil)", applied, err)
	}

	// A cancel marks it failed.
	failed := RunStatusFailed
	reason := "stopped by user"
	if err := store.Update(ctx, run.ID, RunPatch{Status: &failed, Error: &reason}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	// Now a lifecycle transition must NOT revert it.
	research := RunStatusResearching
	if applied, err := store.UpdateIfActive(ctx, run.ID, RunPatch{Status: &research}); err != nil || applied {
		t.Errorf("UpdateIfActive on failed = (%v, %v), want (false, nil)", applied, err)
	}
	done := RunStatusDone
	if applied, _ := store.UpdateIfActive(ctx, run.ID, RunPatch{Status: &done}); applied {
		t.Error("done-write reverted a failed run")
	}
	got, _ := store.Get(ctx, run.ID)
	if got.Status != RunStatusFailed || got.Error != "stopped by user" {
		t.Errorf("run = %s/%q, want failed/'stopped by user' (cancel must stick)", got.Status, got.Error)
	}
}

// IncrementWorkerDone accumulates areas + tokens + pages atomically,
// and evidence_book_slug round-trips.
func TestResearchRunStore_IncrementStats(t *testing.T) {
	store, user := runStoreForTest(t)
	ctx := context.Background()
	run, err := store.Create(ctx, user, "conv-stats", "T", qs(3), "research:ru", "ev-x")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if run.EvidenceBookSlug != "research:ru" {
		t.Errorf("evidence_book_slug = %q, want research:ru", run.EvidenceBookSlug)
	}
	if err := store.IncrementWorkerDone(ctx, run.ID, 9000, 3000, 4); err != nil {
		t.Fatalf("inc 1: %v", err)
	}
	if err := store.IncrementWorkerDone(ctx, run.ID, 6000, 2000, 3); err != nil {
		t.Fatalf("inc 2: %v", err)
	}
	got, _ := store.Get(ctx, run.ID)
	if got.WorkersDone != 2 || got.Tokens != 20000 || got.PagesRead != 7 {
		t.Errorf("after 2 increments = done %d / tokens %d / pages %d, want 2 / 20000 / 7",
			got.WorkersDone, got.Tokens, got.PagesRead)
	}
	if got.InputTokens != 15000 || got.OutputTokens != 5000 {
		t.Errorf("token split = in %d / out %d, want 15000 / 5000", got.InputTokens, got.OutputTokens)
	}
}

func cancelRunRequest(h *Handler, user, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/console/api/research/runs/"+id+"/cancel", nil)
	req.SetPathValue("id", id)
	req = req.WithContext(ctxWithAuth(req.Context(), AuthUser{UserID: user, Role: "user"}))
	rec := httptest.NewRecorder()
	h.cancelResearchRun(rec, req)
	return rec
}

// A run id that isn't a UUID is no run: it was a 500 carrying the
// database's cast error.
func TestCancelResearchRun_NonUUIDIsNotFound(t *testing.T) {
	h := &Handler{researchRuns: &ResearchRunStore{}}
	rec := cancelRunRequest(h, "ru", "abc")
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "uuid") {
		t.Errorf("cancel abc = %d %s, want a plain 404", rec.Code, rec.Body.String())
	}
}

// The stop marks an active run failed and cuts it; a finished run keeps
// its status (and nothing is cut).
func TestCancelResearchRun_ActiveAndFinished(t *testing.T) {
	store, user := runStoreForTest(t)
	ctx := context.Background()
	var cut []string
	h := &Handler{researchRuns: store, researchCanceller: func(id string) bool { cut = append(cut, id); return true }}

	active, _ := store.Create(ctx, user, "cancel-a", "A", qs(1), "research:ru", "e1")
	if rec := cancelRunRequest(h, user, active.ID); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "failed") {
		t.Fatalf("cancel active = %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(ctx, active.ID); got.Status != RunStatusFailed || got.Error != "stopped by user" {
		t.Errorf("active run after cancel: %+v", got)
	}

	done, _ := store.Create(ctx, user, "cancel-d", "D", qs(1), "research:ru", "e2")
	st := RunStatusDone
	_ = store.Update(ctx, done.ID, RunPatch{Status: &st})
	if rec := cancelRunRequest(h, user, done.ID); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done"`) {
		t.Fatalf("cancel done = %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(ctx, done.ID); got.Status != RunStatusDone {
		t.Errorf("a finished run was rewritten: %+v", got)
	}
	if len(cut) != 1 || cut[0] != active.ID {
		t.Errorf("cut = %v, want only the active run", cut)
	}
}
