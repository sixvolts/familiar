package admin

// ResearchRunStore backs autonomous deep-research runs
// (RESEARCH-SKILL-SPEC §6.7). A run row is the source of truth the
// workspace's progress card restores from (survives tab-away) and the
// autonomous synthesis delivery reads: the research skill drives
// workers → gap-fill → synthesis in the background and writes its
// lifecycle here, and a poll endpoint surfaces the active run.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/familiar/gateway/internal/db"
)

// ErrRunNotFound is returned when a run id (or active-run lookup) has
// no matching row.
var ErrRunNotFound = errors.New("research run not found")

// ErrActiveRunExists is returned by Create when the conversation
// already has a non-terminal run — the DB partial-unique index is the
// atomic backstop for the kickoff guard's check-then-create window.
var ErrActiveRunExists = errors.New("an active research run already exists for this conversation")

// Run status values. researching/synthesizing are non-terminal (an
// active run); done/failed are terminal.
const (
	RunStatusResearching  = "researching"
	RunStatusSynthesizing = "synthesizing"
	RunStatusDone         = "done"
	RunStatusFailed       = "failed"
)

// ResearchRun mirrors one research_runs row.
type ResearchRun struct {
	ID               string `json:"id"`
	UserID           string `json:"user_id"`
	ConversationID   string `json:"conversation_id"`
	Topic            string `json:"topic"`
	Status           string `json:"status"`
	Round            int    `json:"round"`
	WorkersTotal     int    `json:"workers_total"`
	WorkersDone      int    `json:"workers_done"`
	Tokens           int64  `json:"tokens"`
	PagesRead        int    `json:"pages_read"`
	EvidenceBookSlug string `json:"evidence_book_slug,omitempty"`
	EvidencePageSlug string `json:"evidence_page_slug,omitempty"`
	NoteBookSlug     string `json:"note_book_slug,omitempty"`
	NotePageSlug     string `json:"note_page_slug,omitempty"`
	Error            string `json:"error,omitempty"`
	// Workers is the per-area roster the in-chat card renders — one entry
	// per sub-question, in dispatch order, keyed by stable index across
	// gap-fill rounds. State is queued|active|done|failed.
	Workers []ResearchWorker `json:"workers"`
}

// ResearchWorker is one area/sub-question in a run's roster.
type ResearchWorker struct {
	Question string `json:"question"`
	State    string `json:"state"` // queued | active | done | failed
}

// Worker states.
const (
	WorkerQueued = "queued"
	WorkerActive = "active"
	WorkerDone   = "done"
	WorkerFailed = "failed"
)

// ResearchRunStore is the DB gateway for research_runs.
type ResearchRunStore struct {
	db *db.Pool
}

// NewResearchRunStore returns a store, or nil when no pool is wired
// (matches the other admin stores' nil-tolerant construction).
func NewResearchRunStore(pool *db.Pool) *ResearchRunStore {
	if pool == nil || pool.DB == nil {
		return nil
	}
	return &ResearchRunStore{db: pool}
}

const researchRunCols = `id::text, user_id, conversation_id, topic, status, round,
	workers_total, workers_done, tokens, pages_read, evidence_book_slug,
	evidence_page_slug, note_book_slug, note_page_slug, error, workers::text`

func scanResearchRun(row interface{ Scan(...any) error }) (*ResearchRun, error) {
	var r ResearchRun
	var workersJSON []byte
	err := row.Scan(&r.ID, &r.UserID, &r.ConversationID, &r.Topic, &r.Status,
		&r.Round, &r.WorkersTotal, &r.WorkersDone, &r.Tokens, &r.PagesRead,
		&r.EvidenceBookSlug, &r.EvidencePageSlug, &r.NoteBookSlug, &r.NotePageSlug, &r.Error,
		&workersJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("research_runs: scan: %w", err)
	}
	// Tolerate a null/empty column (older rows) — an empty roster just
	// means the card falls back to the aggregate counters.
	if len(workersJSON) > 0 {
		if err := json.Unmarshal(workersJSON, &r.Workers); err != nil {
			return nil, fmt.Errorf("research_runs: workers unmarshal: %w", err)
		}
	}
	return &r, nil
}

// Create inserts a new run (status defaults to researching) and returns
// it with its generated id. The partial-unique index on active runs is
// the atomic guard: a concurrent second active run for the same
// conversation hits ON CONFLICT DO NOTHING and Create returns
// ErrActiveRunExists rather than forking a duplicate run.
func (s *ResearchRunStore) Create(ctx context.Context, userID, conversationID, topic string, questions []string, evidenceBookSlug, evidencePageSlug string) (*ResearchRun, error) {
	// Seed the roster — one queued area per sub-question, in order.
	roster := make([]ResearchWorker, len(questions))
	for i, q := range questions {
		roster[i] = ResearchWorker{Question: q, State: WorkerQueued}
	}
	workersJSON, err := json.Marshal(roster)
	if err != nil {
		return nil, fmt.Errorf("research_runs: marshal roster: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO research_runs
		    (user_id, conversation_id, topic, workers_total, evidence_book_slug, evidence_page_slug, workers)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		ON CONFLICT (conversation_id) WHERE status IN ('researching','synthesizing')
		    DO NOTHING
		RETURNING `+researchRunCols,
		userID, conversationID, topic, len(questions), evidenceBookSlug, evidencePageSlug, string(workersJSON))
	run, err := scanResearchRun(row)
	if errors.Is(err, ErrRunNotFound) {
		// DO NOTHING skipped the insert — an active run already exists.
		return nil, ErrActiveRunExists
	}
	return run, err
}

// SetWorkerState transitions one area's state in the roster (keyed by
// its stable dispatch index). Concurrent worker goroutines call this for
// distinct indices; row-level locking serializes the jsonb_set writes so
// no update is lost. Out-of-range or unconfigured idx is a no-op error
// the caller treats as non-fatal (progress is best-effort telemetry).
func (s *ResearchRunStore) SetWorkerState(ctx context.Context, id string, idx int, state string) error {
	if idx < 0 {
		return nil
	}
	// create_if_missing=false makes an out-of-range idx a clean no-op
	// (the roster is returned unchanged) rather than appending a phantom
	// entry — so RowsAffected==0 means only "no such run".
	res, err := s.db.ExecContext(ctx, `
		UPDATE research_runs
		   SET workers = jsonb_set(workers, ARRAY[$2::text, 'state'], to_jsonb($3::text), false),
		       updated_at = NOW()
		 WHERE id = $1::uuid`, id, idx, state)
	if err != nil {
		return fmt.Errorf("research_runs: set worker state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRunNotFound
	}
	return nil
}

// IncrementWorkerDone atomically bumps the live progress counters as a
// worker finishes (§6.7): one more area done, plus its token + page
// tally. Called concurrently by every worker goroutine, so the
// increments are done in SQL. tokens/pages accumulate across gap-fill
// rounds (total work); workers_done is reset per round by Update.
func (s *ResearchRunStore) IncrementWorkerDone(ctx context.Context, id string, tokens int64, pages int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE research_runs
		   SET workers_done = workers_done + 1,
		       tokens = tokens + $2,
		       pages_read = pages_read + $3,
		       updated_at = NOW()
		 WHERE id = $1::uuid`, id, tokens, pages)
	if err != nil {
		return fmt.Errorf("research_runs: worker done: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRunNotFound
	}
	return nil
}

// FailOrphanedRuns marks every non-terminal run failed. Runs are driven
// by in-memory goroutines that don't survive a restart, so any run
// still 'researching'/'synthesizing' at boot is orphaned — reconciling
// them here unwedges conversations that would otherwise refuse a new
// run forever (§6.7). Returns the number reconciled.
func (s *ResearchRunStore) FailOrphanedRuns(ctx context.Context, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE research_runs SET status = 'failed', error = $1, updated_at = NOW()
		 WHERE status IN ('researching','synthesizing')`, reason)
	if err != nil {
		return 0, fmt.Errorf("research_runs: reconcile: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Get fetches a run by id (any owner — callers that need ownership
// checks pass user_id to ActiveForConversation or check r.UserID).
func (s *ResearchRunStore) Get(ctx context.Context, id string) (*ResearchRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+researchRunCols+` FROM research_runs WHERE id = $1::uuid`, id)
	return scanResearchRun(row)
}

// ActiveForConversation returns the newest NON-terminal run for a
// conversation owned by userID — what the progress card polls for.
// Returns ErrRunNotFound when there's no active run.
func (s *ResearchRunStore) ActiveForConversation(ctx context.Context, userID, conversationID string) (*ResearchRun, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+researchRunCols+` FROM research_runs
		 WHERE user_id = $1 AND conversation_id = $2
		   AND status IN ('researching','synthesizing')
		 ORDER BY updated_at DESC
		 LIMIT 1`, userID, conversationID)
	return scanResearchRun(row)
}

// RunPatch carries the fields a lifecycle transition updates. Nil
// fields are left unchanged; updated_at always bumps.
type RunPatch struct {
	Status       *string
	Round        *int
	WorkersDone  *int
	WorkersTotal *int
	NoteBookSlug *string
	NotePageSlug *string
	Error        *string
}

// patchSets builds the SET fragment + args for a RunPatch (shared by
// Update and UpdateIfActive). At least one field must be set.
func patchSets(p RunPatch) ([]string, []any, error) {
	sets := []string{"updated_at = NOW()"}
	args := []any{}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.Status != nil {
		add("status", *p.Status)
	}
	if p.Round != nil {
		add("round", *p.Round)
	}
	if p.WorkersDone != nil {
		add("workers_done", *p.WorkersDone)
	}
	if p.WorkersTotal != nil {
		add("workers_total", *p.WorkersTotal)
	}
	if p.NoteBookSlug != nil {
		add("note_book_slug", *p.NoteBookSlug)
	}
	if p.NotePageSlug != nil {
		add("note_page_slug", *p.NotePageSlug)
	}
	if p.Error != nil {
		add("error", *p.Error)
	}
	if len(args) == 0 {
		return nil, nil, fmt.Errorf("research_runs: empty update")
	}
	return sets, args, nil
}

// Update applies a patch to a run. At least one field must be set.
func (s *ResearchRunStore) Update(ctx context.Context, id string, p RunPatch) error {
	sets, args, err := patchSets(p)
	if err != nil {
		return err
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE research_runs SET %s WHERE id = $%d::uuid`,
			strings.Join(sets, ", "), len(args)), args...)
	if err != nil {
		return fmt.Errorf("research_runs: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRunNotFound
	}
	return nil
}

// UpdateIfActive applies a patch only while the run is still active
// (researching/synthesizing) — a compare-and-set so a lifecycle
// transition can't revert a terminal status a concurrent cancel wrote.
// Returns whether it applied (false = the run was already terminal,
// e.g. stopped by the user).
func (s *ResearchRunStore) UpdateIfActive(ctx context.Context, id string, p RunPatch) (bool, error) {
	sets, args, err := patchSets(p)
	if err != nil {
		return false, err
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE research_runs SET %s WHERE id = $%d::uuid AND status IN ('researching','synthesizing')`,
			strings.Join(sets, ", "), len(args)), args...)
	if err != nil {
		return false, fmt.Errorf("research_runs: update-if-active: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AttachResearchRuns wires the run store so the progress-card endpoint
// can serve it.
func (h *Handler) AttachResearchRuns(s *ResearchRunStore) { h.researchRuns = s }

// AttachResearchCanceller wires the research skill's CancelRun so the
// cancel endpoint can cut an active run's in-flight workers.
func (h *Handler) AttachResearchCanceller(f func(runID string) bool) { h.researchCanceller = f }

// cancelResearchRun backs POST /console/api/research/runs/{id}/cancel —
// the card's stop button. Owner-scoped: marks the run failed("stopped by
// user") and cuts its in-flight workers. Idempotent: cancelling an
// already-terminal run is a no-op 200.
func (h *Handler) cancelResearchRun(w http.ResponseWriter, r *http.Request) {
	if h.researchRuns == nil {
		writeJSONError(w, http.StatusNotFound, "research runs not enabled")
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "run id is required")
		return
	}
	run, err := h.researchRuns.Get(r.Context(), id)
	if errors.Is(err, ErrRunNotFound) {
		writeJSONError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run.UserID != uid {
		// Don't leak existence to non-owners.
		writeJSONError(w, http.StatusNotFound, "run not found")
		return
	}
	// Already terminal → nothing to do.
	if run.Status == RunStatusDone || run.Status == RunStatusFailed {
		writeJSON(w, http.StatusOK, map[string]any{"status": run.Status})
		return
	}
	// Mark failed FIRST so the supervisor's advanceRun / the synthesizer
	// (both re-read status) bail; then cut the in-flight workers.
	st := RunStatusFailed
	reason := "stopped by user"
	if err := h.researchRuns.Update(r.Context(), id, RunPatch{Status: &st, Error: &reason}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h.researchCanceller != nil {
		h.researchCanceller(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "failed"})
}

// getActiveResearchRun backs GET /console/api/research/runs/active?
// conversation_id= — the workspace polls it to render/restore the
// persistent progress card (§6.7). Owner-scoped: only the caller's own
// runs are visible. Returns {"run": null} when there's no active run,
// so the client can clear a stale card.
func (h *Handler) getActiveResearchRun(w http.ResponseWriter, r *http.Request) {
	if h.researchRuns == nil {
		writeJSON(w, http.StatusOK, map[string]any{"run": nil})
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	convID := strings.TrimSpace(r.URL.Query().Get("conversation_id"))
	if convID == "" {
		writeJSONError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}
	run, err := h.researchRuns.ActiveForConversation(r.Context(), uid, convID)
	if errors.Is(err, ErrRunNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"run": nil})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}
