package admin

import (
	"context"
	"net/http"
)

// StatusProvider supplies the dashboard snapshot the admin console
// renders on its landing page. Implementations are constructed in
// cmd/gateway where the concrete router/memory/session/skill handles
// all live together; the admin package stays agnostic to those types.
type StatusProvider interface {
	Snapshot(ctx context.Context) (StatusSnapshot, error)
}

// StatusSnapshot is the JSON shape returned by GET /admin/api/status.
// Every field is scalar or a small slice so the whole thing is cheap
// to serialise and poll on a 30-second cadence from the browser.
type StatusSnapshot struct {
	Gateway  GatewayStatus `json:"gateway"`
	Models   []ModelStatus `json:"models"`
	Memory   MemoryStatus  `json:"memory"`
	Users    UserStatus    `json:"users"`
	Sessions SessionStatus `json:"sessions"`
	Skills   []string      `json:"skills"`
}

// GatewayStatus reports gateway-level health. Uptime is computed at
// the provider (against a fixed start time) so the client doesn't have
// to do timezone-aware subtraction.
type GatewayStatus struct {
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// ModelStatus is one entry from the router's registered models list.
// Endpoint is included so operators can spot a misconfigured URL
// without logging into the box.
type ModelStatus struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Endpoint string `json:"endpoint,omitempty"`
}

// MemoryStatus is the single-call aggregate the dashboard shows for
// the memories table. Only the total is required; the rest are
// omitted on memory-browser-less deployments.
type MemoryStatus struct {
	Total int `json:"total"`
}

// UserStatus mirrors the identity.Resolver's user counts, pre-bucketed
// by status so the UI doesn't have to re-scan the list.
type UserStatus struct {
	Total    int `json:"total"`
	Approved int `json:"approved"`
	Pending  int `json:"pending"`
}

// SessionStatus is the current in-memory session count.
type SessionStatus struct {
	Active int `json:"active"`
}

// AttachStatusProvider wires a snapshot source into the handler. Must
// be called before Mux() for GET /admin/api/status to respond with
// live data; without it the endpoint returns 503 so the frontend can
// render a graceful "status not available" state.
func (h *Handler) AttachStatusProvider(sp StatusProvider) {
	h.status = sp
}

// status serves GET /admin/api/status. Pure read-through to the
// provider — no caching, no aggregation here.
func (h *Handler) getStatus(w http.ResponseWriter, r *http.Request) {
	if h.status == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "status provider not configured")
		return
	}
	snap, err := h.status.Snapshot(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}
