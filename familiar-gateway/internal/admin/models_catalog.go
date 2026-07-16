package admin

// Model catalog endpoint backing the chat UI's "..." → Models
// submenu. Returns id + display name for every registered model so
// the menu can render a list and the user can pick one. Implemented
// against a narrow ModelCatalog interface so the admin package
// doesn't depend on internal/router or internal/config — the
// gateway projects router.Registry.List() into this shape at wiring
// time. MODEL-SELECTOR.md §"Gateway Changes — Model List API".

import (
	"net/http"
)

// ModelInfo is the on-the-wire shape returned to the chat UI. Only
// fields the menu needs are exposed; secrets (api_key, vault_key,
// raw endpoint) stay server-side.
type ModelInfo struct {
	ID             string   `json:"id"`
	DisplayName    string   `json:"display_name"`
	LatencyProfile string   `json:"latency_profile,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
}

// ModelCatalog is the narrow surface the admin handler depends on.
// router.Registry's List() result is projected into a ModelInfo
// slice in cmd/gateway and handed in here.
type ModelCatalog interface {
	Models() []ModelInfo
}

// AttachModelCatalog wires a model catalog onto the handler.
// Optional — without it, /console/api/models returns 503.
func (h *Handler) AttachModelCatalog(c ModelCatalog) { h.models = c }

// listModels serves GET /console/api/models. Returns every
// catalog-listed model. The chat UI picks one ID and sends it in
// the `model` field of /v1/chat/completions; "automatic" /
// "familiar" lets the router pick.
func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	if h.models == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "model catalog not configured")
		return
	}
	items := h.models.Models()
	if items == nil {
		items = []ModelInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":  items,
		"default": "automatic",
	})
}
