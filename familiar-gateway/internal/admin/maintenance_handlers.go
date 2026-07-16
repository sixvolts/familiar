package admin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/familiar/gateway/internal/maintenance"
)

// Persisted keys in instance_settings so a restart mid-maintenance
// doesn't silently revert to the (down) primary model.
const (
	maintenanceEnabledKey = "maintenance_enabled"
	maintenanceModelKey   = "maintenance_model"
)

// AttachMaintenance wires the runtime maintenance-mode controller and
// rehydrates its last-persisted state from instance_settings (if
// available). Call after AttachInstanceSettings so the load can run.
func (h *Handler) AttachMaintenance(c *maintenance.Controller) {
	h.maintenance = c
	if c == nil || h.instanceSettings == nil {
		return
	}
	all, err := h.instanceSettings.GetAll(context.Background())
	if err != nil {
		return
	}
	model := all[maintenanceModelKey]
	if model == "" {
		return
	}
	// Only restore a selection that still resolves to a registered
	// model — a model removed from config between restarts is dropped.
	if !c.Known(model) {
		return
	}
	c.SetState(all[maintenanceEnabledKey] == "true", model)
}

// getMaintenance returns the current maintenance state. Any
// authenticated user may read it (the frontend banner needs it); only
// admins can change it via POST.
func (h *Handler) getMaintenance(w http.ResponseWriter, r *http.Request) {
	if h.maintenance == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	resp := maintenanceResponse(h.maintenance.State())
	resp["available"] = true
	writeJSON(w, http.StatusOK, resp)
}

// setMaintenance toggles maintenance mode and/or selects the fallback
// model. Body: {"enabled": bool, "model_id": "<registered model id>"}.
// Admin-only (wrapped with adminOnly at registration).
func (h *Handler) setMaintenance(w http.ResponseWriter, r *http.Request) {
	if h.maintenance == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "maintenance mode is not available")
		return
	}
	var body struct {
		Enabled bool   `json:"enabled"`
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// A selection (or an enable) must name a model the registry knows.
	if body.ModelID != "" && !h.maintenance.Known(body.ModelID) {
		writeJSONError(w, http.StatusBadRequest, "unknown model id")
		return
	}
	if body.Enabled && body.ModelID == "" {
		writeJSONError(w, http.StatusBadRequest, "select a fallback model before enabling maintenance mode")
		return
	}

	h.maintenance.SetState(body.Enabled, body.ModelID)

	// Persist so the switch survives a gateway restart.
	if h.instanceSettings != nil {
		by := "admin"
		if u, ok := AuthUserFrom(r.Context()); ok && u.UserID != "" {
			by = u.UserID
		}
		_ = h.instanceSettings.Set(r.Context(), maintenanceModelKey, body.ModelID, by)
		enabledStr := "false"
		if body.Enabled {
			enabledStr = "true"
		}
		_ = h.instanceSettings.Set(r.Context(), maintenanceEnabledKey, enabledStr, by)
	}

	resp := maintenanceResponse(h.maintenance.State())
	resp["available"] = true
	writeJSON(w, http.StatusOK, resp)
}

func maintenanceResponse(s maintenance.State) map[string]any {
	return map[string]any{
		"active":          s.Active,
		"reason":          s.Reason,
		"enabled":         s.Enabled,
		"model_id":        s.ModelID,
		"model":           s.Model,
		"primary_id":      s.PrimaryID,
		"primary_model":   s.PrimaryModel,
		"primary_offline": s.PrimaryOffline,
		"message":         s.Message,
	}
}
