// Package maintenance holds the runtime "maintenance mode" switch:
// take the primary (big) chat model out of the rotation and route
// chat to a slower fallback model instead, surfacing a banner to
// every user.
//
// The fallback model is chosen at runtime from the admin panel (a
// dropdown of the registered models) — there is no config key. State
// is held in memory here and persisted by the admin layer (instance
// settings) so a restart mid-maintenance doesn't silently revert.
//
// Two ways in (the operator picked "both"):
//   - manual: an admin toggles it on (Enabled).
//   - auto:   the primary model's health check goes offline.
//
// Either way the pipeline routes the trusted chat path to the
// selected fallback model. Auto only engages once a fallback model
// has been selected — there's nothing to fall back to otherwise.
package maintenance

import "sync"

// Controller is the in-memory maintenance switch. It is safe for
// concurrent use. All registry coupling is injected as functions so
// this package imports nothing (no router/admin import cycles).
type Controller struct {
	mu      sync.RWMutex
	enabled bool   // admin manual toggle
	modelID string // selected fallback model id ("" → none chosen yet)

	// statusOf reports a model's health: "online" | "offline" | "unknown".
	statusOf func(string) string
	// labelOf returns a model's human display label, or "" if the id
	// is not a registered model (also used to validate selections).
	labelOf func(string) string
	// primaryFn returns the id of the primary chat model (the one
	// maintenance replaces) — typically router.GetChatModelID.
	primaryFn func() string
}

// State is the JSON-friendly snapshot handed to the admin panel and
// folded into /auth/status so the frontend banner can show/hide
// (including clearing itself when the primary recovers).
type State struct {
	Active         bool   `json:"active"`
	Reason         string `json:"reason,omitempty"` // "manual" | "auto"
	Enabled        bool   `json:"enabled"`          // manual toggle state
	ModelID        string `json:"model_id,omitempty"`
	Model          string `json:"model,omitempty"` // display label of the fallback
	PrimaryID      string `json:"primary_id,omitempty"`
	PrimaryModel   string `json:"primary_model,omitempty"`
	PrimaryOffline bool   `json:"primary_offline"`
	Message        string `json:"message,omitempty"`
}

// New builds a controller. statusOf/labelOf resolve against the model
// registry; primaryFn returns the current primary chat model id.
func New(statusOf, labelOf func(string) string, primaryFn func() string) *Controller {
	return &Controller{statusOf: statusOf, labelOf: labelOf, primaryFn: primaryFn}
}

// SetState updates the manual toggle and selected fallback model. A
// blank modelID clears the selection (and forces the switch off,
// since there's nothing to route to).
func (c *Controller) SetState(enabled bool, modelID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modelID = modelID
	c.enabled = enabled && modelID != ""
}

// Known reports whether id resolves to a registered model — used to
// validate an incoming selection before persisting it.
func (c *Controller) Known(id string) bool {
	return c.label(id) != ""
}

func (c *Controller) label(id string) string {
	if id == "" || c.labelOf == nil {
		return ""
	}
	return c.labelOf(id)
}

func (c *Controller) labelOrID(id string) string {
	if l := c.label(id); l != "" {
		return l
	}
	return id
}

func (c *Controller) status(id string) string {
	if id == "" || c.statusOf == nil {
		return ""
	}
	return c.statusOf(id)
}

func (c *Controller) primary() string {
	if c.primaryFn == nil {
		return ""
	}
	return c.primaryFn()
}

// State returns the current snapshot, recomputing active/reason from
// the live toggle + primary health on every call (so auto engages and
// clears as health flips, with no extra plumbing).
func (c *Controller) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := State{Enabled: c.enabled, ModelID: c.modelID}
	if c.modelID != "" {
		s.Model = c.labelOrID(c.modelID)
	}
	if p := c.primary(); p != "" {
		s.PrimaryID = p
		s.PrimaryModel = c.labelOrID(p)
		s.PrimaryOffline = c.status(p) == "offline"
	}

	// Active needs a chosen fallback in either mode.
	if c.modelID != "" {
		if c.enabled {
			s.Active = true
			s.Reason = "manual"
		} else if s.PrimaryOffline {
			s.Active = true
			s.Reason = "auto"
		}
	}
	if s.Active {
		s.Message = "Maintenance mode — using " + s.Model
	}
	return s
}

// Active reports whether maintenance is currently in effect and, if
// so, the fallback model id the pipeline should route chat to.
func (c *Controller) Active() (bool, string) {
	s := c.State()
	return s.Active, s.ModelID
}
