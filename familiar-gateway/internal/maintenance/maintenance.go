// Package maintenance holds the runtime "maintenance mode" switch:
// take the primary (big) chat model out of the rotation and route
// chat to a slower fallback model instead, surfacing a banner to
// every user.
//
// Since ROLE-FAILOVER this is the *last* tier of the chat role's
// failover chain, not a parallel mechanism. [roles.chat] already fails
// over primary → backup → global fallback automatically on health, and
// the router resolves it per turn. Maintenance adds the two things the
// chain can't express:
//
//   - manual drain: an admin toggles it on to pull the big model out
//     even while it is perfectly healthy (Enabled).
//   - an operator-chosen, runtime-selected model that isn't in the
//     config chain at all — picked from a dropdown of registered
//     models, persisted via instance settings so a restart
//     mid-maintenance doesn't silently revert.
//
// Auto-engage remains for the case where the chat chain is exhausted
// (every configured candidate offline): the admin-selected model is
// tried before giving up. It only engages once a fallback model has
// been selected — there's nothing to fall back to otherwise.
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
	// primaryFn returns the id of the chat role's configured primary —
	// the model maintenance replaces. This is tier 0 of the chat chain,
	// NOT whatever is currently serving (see servingFn).
	primaryFn func() string

	// servingFn returns the model id currently serving the chat role and
	// its tier in the chain (0 = primary). Optional: when nil the
	// controller assumes the primary is serving, which is the pre-roles
	// behavior. Lets the banner distinguish "running on the configured
	// backup" (handled by the chain, no maintenance needed) from
	// "running on the admin-selected maintenance model".
	servingFn func() (string, int)
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

	// ServingID / ServingModel / ServingTier describe what is actually
	// answering chat right now, which since ROLE-FAILOVER may be a
	// configured backup (tier > 0) rather than the primary or the
	// maintenance model. FailoverActive is true when the chat role has
	// fallen off its primary — the signal the banner uses to tell users
	// "running on the backup" without maintenance being engaged.
	ServingID      string `json:"serving_id,omitempty"`
	ServingModel   string `json:"serving_model,omitempty"`
	ServingTier    int    `json:"serving_tier"`
	FailoverActive bool   `json:"failover_active"`
}

// New builds a controller. statusOf/labelOf resolve against the model
// registry; primaryFn returns the chat role's configured primary id.
// Use WithServing to teach it which tier is actually serving.
func New(statusOf, labelOf func(string) string, primaryFn func() string) *Controller {
	return &Controller{statusOf: statusOf, labelOf: labelOf, primaryFn: primaryFn}
}

// WithServing attaches the "who is serving chat right now" probe (model
// id + chain tier), typically router.GetChatModelID + ChatModelTier.
// Returns the controller for chaining at wiring time.
func (c *Controller) WithServing(fn func() (string, int)) *Controller {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.servingFn = fn
	return c
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

	// Who is actually answering chat, per the chat role's chain.
	if c.servingFn != nil {
		id, tier := c.servingFn()
		if id != "" {
			s.ServingID = id
			s.ServingModel = c.labelOrID(id)
			s.ServingTier = tier
			s.FailoverActive = tier > 0
		}
	}

	// Active needs a chosen fallback in either mode.
	//
	// Auto engages only when the chat role's chain is EXHAUSTED — i.e.
	// the model currently serving is itself offline — not merely when the
	// primary is down. That ordering matters: maintenance is the last
	// tier, so a healthy configured backup must get the traffic before
	// the admin-selected maintenance model does. With no serving probe
	// wired (pre-roles behavior) fall back to "primary offline".
	if c.modelID != "" {
		switch {
		case c.enabled:
			s.Active = true
			s.Reason = "manual"
		case c.chainExhausted(s):
			s.Active = true
			s.Reason = "auto"
		}
	}
	switch {
	case s.Active:
		s.Message = "Maintenance mode — using " + s.Model
	case s.FailoverActive:
		// The chain handled it on its own; say so rather than staying
		// silent, since answers are coming from a different model than
		// the operator's primary.
		s.Message = "Primary model unavailable — using " + s.ServingModel
	}
	return s
}

// chainExhausted reports whether the chat role has no usable model left
// — the condition for auto-engaging maintenance. Called with the read
// lock held (from State), so it must not re-lock.
func (c *Controller) chainExhausted(s State) bool {
	if c.servingFn == nil {
		// No serving probe: pre-roles behavior, where "primary offline"
		// is the only signal available.
		return s.PrimaryOffline
	}
	if s.ServingID == "" {
		// The chat role resolves to nothing at all — which is "no chat
		// model is configured", NOT "every tier is down". Treating
		// absence as exhaustion made maintenance latch on permanently on
		// any deployment without a chat model: State() would report
		// Active=auto forever, so the banner never cleared after an admin
		// switched maintenance off. Fall back to the pre-roles signal
		// instead; auto-engage needs positive evidence, not silence.
		return s.PrimaryOffline
	}
	// The chain returned a model but it's offline too → every tier is
	// down, so the admin-selected model is the only thing left to try.
	return c.status(s.ServingID) == "offline"
}

// Active reports whether maintenance is currently in effect and, if
// so, the fallback model id the pipeline should route chat to.
func (c *Controller) Active() (bool, string) {
	s := c.State()
	return s.Active, s.ModelID
}
