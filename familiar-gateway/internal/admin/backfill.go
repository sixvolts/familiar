package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/familiar/gateway/internal/backfill"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/safego"
)

// backfillAdapter bridges PgVectorStore's BackfillItem to the
// backfill.Source contract. PgVectorStore lives in the memory package
// and the backfill package can't import it without a cycle (memory
// imports sidecar types already), so the adapter lives here where
// both concrete types are fair game.
type backfillAdapter struct {
	store *memory.PgVectorStore
}

func (a *backfillAdapter) ListForBackfill(ctx context.Context, userID string) ([]backfill.Item, error) {
	rows, err := a.store.ListForBackfill(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]backfill.Item, 0, len(rows))
	for _, r := range rows {
		out = append(out, backfill.Item{
			ID:      r.ID,
			Content: r.Content,
			UserID:  r.UserID,
		})
	}
	return out, nil
}

// backfillRunner is the subset of *sidecar.Client the admin handler
// needs, held as an interface so the admin package does not have to
// import sidecar directly. cmd/gateway wires a real *sidecar.Client
// in through AttachBackfill and the interface satisfies itself.
type backfillRunner interface {
	backfill.Extractor
}

// AttachBackfill wires the relationship-backfill collaborators onto
// the handler. Must be called after AttachMemoryBrowser because the
// adapter built here shares the same *PgVectorStore. Passing any nil
// argument leaves backfill disabled (the endpoints reply with 503).
func (h *Handler) AttachBackfill(extractor backfillRunner, store *memory.PgVectorStore, sink backfill.Sink) {
	if extractor == nil || store == nil || sink == nil {
		return
	}
	h.backfillDeps = &backfill.Deps{
		Source:    &backfillAdapter{store: store},
		Extractor: extractor,
		Sink:      sink,
	}
}

// startBackfill serves POST /admin/api/controls/backfill-relationships.
// Kicks off a background goroutine and returns immediately. Returns
// 409 if a previous run is still in progress so a nervous operator
// double-clicking the button does not stack two scans on the sidecar.
func (h *Handler) startBackfill(w http.ResponseWriter, r *http.Request) {
	if h.backfillDeps == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "backfill not configured")
		return
	}

	var body struct {
		UserID    string `json:"user_id"`
		BatchSize int    `json:"batch_size"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	h.backfillMu.Lock()
	if h.backfillState != nil && h.backfillState.Running {
		h.backfillMu.Unlock()
		writeJSONError(w, http.StatusConflict, "backfill already running")
		return
	}
	// Seed a Running snapshot so an immediate GET sees "started" even
	// if the goroutine hasn't begun scanning yet.
	seed := backfill.Progress{Running: true}
	h.backfillState = &seed
	deps := *h.backfillDeps
	h.backfillMu.Unlock()

	opts := backfill.Options{UserID: body.UserID, BatchSize: body.BatchSize}

	// Use a detached context: the HTTP request context will be
	// cancelled as soon as we return the response, but the backfill
	// itself needs to outlive that.
	go func() {
		// Detached, so a panic here would kill the gateway. And recovery
		// alone would not be enough: the 409 guard above is keyed on
		// backfillState.Running, which only the completion path below
		// clears — a panicking run would leave it set for the process
		// lifetime and the endpoint would refuse every retry with
		// "backfill already running". Clear it and record why.
		defer safego.RecoverWith("admin relationship backfill", func(rec any) {
			h.backfillMu.Lock()
			defer h.backfillMu.Unlock()
			h.backfillState = &backfill.Progress{
				Running:   false,
				LastError: fmt.Sprintf("backfill panicked: %v", rec),
			}
		})
		ctx := context.Background()
		final, err := backfill.Run(ctx, deps, opts, func(p backfill.Progress) {
			h.backfillMu.Lock()
			snap := p
			h.backfillState = &snap
			h.backfillMu.Unlock()
		})
		h.backfillMu.Lock()
		defer h.backfillMu.Unlock()
		if err != nil && h.backfillState != nil && h.backfillState.LastError == "" {
			final.LastError = err.Error()
		}
		snap := final
		h.backfillState = &snap
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "started",
	})
}

// getBackfillStatus serves GET /admin/api/controls/backfill-relationships.
// Returns the current progress snapshot — including a zero struct
// when no run has ever started — so the frontend can render a
// consistent "idle" state without a 404 branch.
func (h *Handler) getBackfillStatus(w http.ResponseWriter, r *http.Request) {
	if h.backfillDeps == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "backfill not configured")
		return
	}
	h.backfillMu.Lock()
	var snap backfill.Progress
	if h.backfillState != nil {
		snap = *h.backfillState
	}
	h.backfillMu.Unlock()
	writeJSON(w, http.StatusOK, snap)
}
