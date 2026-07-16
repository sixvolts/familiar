package admin

// Scheduled-actions console API (SCHEDULED-ACTIONS-SPEC Phase 1).
// Owner-scoped exactly like the shards endpoints: non-admins see and
// mutate only their own actions, a non-owned id reads as 404, and
// shard sessions are refused outright — a kiosk must not be able to
// rewire its owner's robots.
//
// Target validation beyond shape (does the page exist, can the owner
// write it) happens here at write time, where the wiki store is
// available; the store package only checks shape.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/familiar/gateway/internal/actions"
)

func parseRFC3339(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("run_at must be RFC3339: %w", err)
	}
	return t, nil
}

// AttachActions wires the scheduled-actions store + runner. Both nil
// until main.go has a DB pool; the handlers 503 before that.
func (h *Handler) AttachActions(store *actions.Store, runner *actions.Runner) {
	h.actions = store
	h.actionsRunner = runner
}

func (h *Handler) requireActions(w http.ResponseWriter) bool {
	if h.actions == nil || h.actionsRunner == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "scheduled actions not configured")
		return false
	}
	return true
}

// actionScope resolves the caller for owner-scoping. Mirrors the
// shards endpoints' posture.
func (h *Handler) actionScope(w http.ResponseWriter, r *http.Request) (userID string, isAdmin bool, ok bool) {
	if h.refuseShardSession(w, r) {
		return "", false, false
	}
	au, authOK := AuthUserFrom(r.Context())
	if !authOK || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return "", false, false
	}
	return au.UserID, au.IsAdmin(), true
}

func (h *Handler) listActions(w http.ResponseWriter, r *http.Request) {
	if !h.requireActions(w) {
		return
	}
	userID, isAdmin, ok := h.actionScope(w, r)
	if !ok {
		return
	}
	scope := userID
	if isAdmin {
		// Admins default to their own list; ?user_id=<id> inspects
		// another user's, ?user_id=all the whole instance.
		switch q := r.URL.Query().Get("user_id"); q {
		case "":
			scope = userID
		case "all":
			scope = ""
		default:
			scope = q
		}
	}
	items, err := h.actions.List(r.Context(), scope, isAdmin)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// actionBody is the create/patch wire shape. Pointers distinguish
// "absent" from "zero" on PATCH.
type actionBody struct {
	ShardID                *string          `json:"shard_id"`
	Envelope               *string          `json:"envelope"`
	Name                   *string          `json:"name"`
	Prompt                 *string          `json:"prompt"`
	TriggerKind            *string          `json:"trigger_kind"`
	Cron                   *string          `json:"cron"`
	RunAt                  *string          `json:"run_at"` // RFC3339; "" clears
	WatchBookSlug          *string          `json:"watch_book_slug"`
	MinIntervalSeconds     *int             `json:"min_interval_seconds"`
	Timezone               *string          `json:"timezone"`
	ReportTargets          []actions.Target `json:"report_targets"`
	DeliveryPolicy         *string          `json:"delivery_policy"`
	TimeoutSeconds         *int             `json:"timeout_seconds"`
	MaxConsecutiveFailures *int             `json:"max_consecutive_failures"`
	MaxRunsPerDay          *int             `json:"max_runs_per_day"`
}

func (b *actionBody) applyTo(a *actions.Action) error {
	if b.ShardID != nil {
		a.ShardID = *b.ShardID
	}
	if b.Envelope != nil {
		a.Envelope = *b.Envelope
		// PATCH ergonomics: switching away from a shard envelope
		// clears the stale binding — but only when this request
		// didn't ALSO send shard_id explicitly. An explicit
		// contradictory pair (envelope=ephemeral + shard_id) falls
		// through to Validate's coherence check and 400s.
		if a.Envelope != actions.EnvelopeShard && b.ShardID == nil {
			a.ShardID = ""
		}
	}
	if b.Name != nil {
		a.Name = *b.Name
	}
	if b.Prompt != nil {
		a.Prompt = *b.Prompt
	}
	if b.TriggerKind != nil {
		a.TriggerKind = *b.TriggerKind
	}
	if b.WatchBookSlug != nil {
		a.WatchBookSlug = *b.WatchBookSlug
	}
	if b.MinIntervalSeconds != nil {
		a.MinIntervalSeconds = *b.MinIntervalSeconds
	}
	if b.Cron != nil {
		a.Cron = *b.Cron
		if *b.Cron != "" {
			a.RunAt = nil
		}
	}
	if b.RunAt != nil {
		if *b.RunAt == "" {
			a.RunAt = nil
		} else {
			t, err := parseRFC3339(*b.RunAt)
			if err != nil {
				return err
			}
			a.RunAt = &t
			a.Cron = ""
		}
	}
	if b.Timezone != nil {
		a.Timezone = *b.Timezone
	}
	if b.ReportTargets != nil {
		a.ReportTargets = b.ReportTargets
	}
	if b.DeliveryPolicy != nil {
		a.DeliveryPolicy = *b.DeliveryPolicy
	}
	if b.TimeoutSeconds != nil {
		a.TimeoutSeconds = *b.TimeoutSeconds
	}
	if b.MaxConsecutiveFailures != nil {
		a.MaxConsecutiveFailures = *b.MaxConsecutiveFailures
	}
	if b.MaxRunsPerDay != nil {
		a.MaxRunsPerDay = *b.MaxRunsPerDay
	}
	return nil
}

// prepareActionTrigger normalizes the Phase 3 trigger surface before
// Validate: page_saved resolves the watched book slug to its id for
// the OWNER (membership-scoped, honoring the "personal" alias) so
// the runner can match bus events; webhook generates the bearer
// token server-side; stale fields from a kind switch are cleared so
// the trigger-shape CHECK can't trip on leftovers.
func (h *Handler) prepareActionTrigger(r *http.Request, a *actions.Action) error {
	// Re-derive empty kind the way the store will, so the clearing
	// below sees the real kind.
	if a.TriggerKind == "" {
		if a.RunAt != nil {
			a.TriggerKind = actions.TriggerOneShot
		} else {
			a.TriggerKind = actions.TriggerCron
		}
	}
	switch a.TriggerKind {
	case actions.TriggerPageSaved:
		a.Cron = ""
		a.RunAt = nil
		a.WebhookToken = ""
		if a.WatchBookSlug == "" {
			return fmt.Errorf("page_saved requires watch_book_slug")
		}
		if h.wiki == nil {
			return fmt.Errorf("page_saved requires the wiki store")
		}
		b, err := h.resolveBookForOwner(r, a.WatchBookSlug, a.OwnerID)
		if err != nil {
			return fmt.Errorf("watch book %q: %w", a.WatchBookSlug, err)
		}
		a.WatchBookID = b.ID
	case actions.TriggerWebhook:
		a.Cron = ""
		a.RunAt = nil
		a.WatchBookID = ""
		a.WatchBookSlug = ""
		if a.WebhookToken == "" {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return fmt.Errorf("generate webhook token: %w", err)
			}
			a.WebhookToken = "whk_" + base64.RawURLEncoding.EncodeToString(raw)
		}
	default:
		a.WatchBookID = ""
		a.WatchBookSlug = ""
		a.WebhookToken = ""
	}
	return nil
}

// fillConversationTargets auto-creates the dedicated thread for any
// conversation OR push target submitted without an id ("Scheduled:
// <name>") so the report has a stable home the user can reply to later
// (and, for push, a target for the notification's deep link — one
// thread per action). Runs before Validate — by store time the id is
// always present.
func (h *Handler) fillConversationTargets(r *http.Request, a *actions.Action) error {
	for i, t := range a.ReportTargets {
		if (t.Kind != "conversation" && t.Kind != "push") || t.ConversationID != "" {
			continue
		}
		if h.conversations == nil {
			return fmt.Errorf("conversation target set but conversations not configured")
		}
		conv, err := h.conversations.Create(r.Context(), a.OwnerID, "Scheduled: "+a.Name, "familiar")
		if err != nil {
			return fmt.Errorf("auto-create conversation: %w", err)
		}
		a.ReportTargets[i].ConversationID = conv.ID
	}
	return nil
}

// validateActionRefs checks the parts of an action that point at
// other objects: the shard must exist and belong to the owner; page
// targets must resolve to a page the owner can write.
func (h *Handler) validateActionRefs(r *http.Request, a *actions.Action) (int, string) {
	if a.ShardID != "" {
		if h.shards == nil {
			return http.StatusBadRequest, "shard_id set but shards not configured"
		}
		sh, err := h.shards.GetShard(r.Context(), a.ShardID)
		if err != nil || sh.OwnerID != a.OwnerID {
			return http.StatusBadRequest, "shard not found or not owned by you"
		}
		// Binding a disabled shard would just feed the breaker one
		// error per fire — refuse at write time, where the message
		// can actually reach the human.
		if !sh.Active() {
			return http.StatusBadRequest, "shard " + sh.ID + " is disabled — enable it before binding"
		}
	}
	for _, t := range a.ReportTargets {
		switch t.Kind {
		case "page":
			if h.wiki == nil {
				return http.StatusBadRequest, "page target set but wiki not configured"
			}
			b, err := h.resolveBookForOwner(r, t.BookSlug, a.OwnerID)
			if err != nil {
				return http.StatusBadRequest, "page target: " + err.Error()
			}
			if _, err := h.wiki.GetPageByID(r.Context(), b.ID, t.PageID); err != nil {
				return http.StatusBadRequest, "page target: page not found in " + t.BookSlug
			}
			role, err := h.wiki.MemberRole(r.Context(), b.ID, a.OwnerID)
			if err != nil || (role != "owner" && role != "writer") {
				return http.StatusBadRequest, "page target: owner lacks write access to " + t.BookSlug
			}
		case "conversation":
			if h.conversations == nil {
				return http.StatusBadRequest, "conversation target set but conversations not configured"
			}
			owns, err := h.conversations.OwnsConversation(r.Context(), t.ConversationID, a.OwnerID)
			if err != nil || !owns {
				return http.StatusBadRequest, "conversation target: not found or not owned by you"
			}
		}
	}
	return 0, ""
}

// resolveBookForOwner resolves a book slug for the action's owner,
// honoring the same "personal" alias the wiki routes use.
func (h *Handler) resolveBookForOwner(r *http.Request, slug, ownerID string) (*Book, error) {
	if slug == "personal" {
		return h.wiki.EnsurePersonalBook(r.Context(), ownerID)
	}
	return h.wiki.GetBookBySlug(r.Context(), slug, ownerID, false)
}

func (h *Handler) createAction(w http.ResponseWriter, r *http.Request) {
	if !h.requireActions(w) {
		return
	}
	userID, _, ok := h.actionScope(w, r)
	if !ok {
		return
	}
	var body actionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	a := &actions.Action{OwnerID: userID, Enabled: true}
	if err := body.applyTo(a); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.prepareActionTrigger(r, a); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.fillConversationTargets(r, a); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := actions.Validate(a); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if code, msg := h.validateActionRefs(r, a); code != 0 {
		writeJSONError(w, code, msg)
		return
	}
	created, err := h.actions.Create(r.Context(), a)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) getAction(w http.ResponseWriter, r *http.Request) {
	if !h.requireActions(w) {
		return
	}
	userID, isAdmin, ok := h.actionScope(w, r)
	if !ok {
		return
	}
	a, err := h.actions.Get(r.Context(), r.PathValue("id"), userID, isAdmin)
	if errors.Is(err, actions.ErrActionNotFound) {
		writeJSONError(w, http.StatusNotFound, "action not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) patchAction(w http.ResponseWriter, r *http.Request) {
	if !h.requireActions(w) {
		return
	}
	userID, isAdmin, ok := h.actionScope(w, r)
	if !ok {
		return
	}
	a, err := h.actions.Get(r.Context(), r.PathValue("id"), userID, isAdmin)
	if errors.Is(err, actions.ErrActionNotFound) {
		writeJSONError(w, http.StatusNotFound, "action not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var body actionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := body.applyTo(a); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.prepareActionTrigger(r, a); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.fillConversationTargets(r, a); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := actions.Validate(a); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if code, msg := h.validateActionRefs(r, a); code != 0 {
		writeJSONError(w, code, msg)
		return
	}
	updated, err := h.actions.Update(r.Context(), a, isAdmin)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteAction(w http.ResponseWriter, r *http.Request) {
	if !h.requireActions(w) {
		return
	}
	userID, isAdmin, ok := h.actionScope(w, r)
	if !ok {
		return
	}
	err := h.actions.Delete(r.Context(), r.PathValue("id"), userID, isAdmin)
	if errors.Is(err, actions.ErrActionNotFound) {
		writeJSONError(w, http.StatusNotFound, "action not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setActionEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.requireActions(w) {
			return
		}
		userID, isAdmin, ok := h.actionScope(w, r)
		if !ok {
			return
		}
		err := h.actions.SetEnabled(r.Context(), r.PathValue("id"), userID, isAdmin, enabled)
		if errors.Is(err, actions.ErrActionNotFound) {
			writeJSONError(w, http.StatusNotFound, "action not found")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a, err := h.actions.Get(r.Context(), r.PathValue("id"), userID, isAdmin)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, a)
	}
}

func (h *Handler) runActionNow(w http.ResponseWriter, r *http.Request) {
	if !h.requireActions(w) {
		return
	}
	userID, isAdmin, ok := h.actionScope(w, r)
	if !ok {
		return
	}
	runID, err := h.actionsRunner.RunNow(r.Context(), r.PathValue("id"), userID, isAdmin)
	if errors.Is(err, actions.ErrActionNotFound) {
		writeJSONError(w, http.StatusNotFound, "action not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}

// actionWebhook is the PUBLIC webhook entry point — no session, the
// URL token IS the credential (generated server-side, unique-indexed,
// compared constant-time in the runner). Every failure shape reads as
// 404 except throttling (429), so the endpoint can't be used to
// distinguish bad token / wrong kind / disabled action.
func (h *Handler) actionWebhook(w http.ResponseWriter, r *http.Request) {
	if h.actions == nil || h.actionsRunner == nil {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	runID, err := h.actionsRunner.FireWebhook(r.Context(), r.PathValue("token"), body)
	if errors.Is(err, actions.ErrThrottled) {
		writeJSONError(w, http.StatusTooManyRequests, "throttled")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}

func (h *Handler) listActionRuns(w http.ResponseWriter, r *http.Request) {
	if !h.requireActions(w) {
		return
	}
	userID, isAdmin, ok := h.actionScope(w, r)
	if !ok {
		return
	}
	// Ownership ride-along: loading the action enforces the scope
	// before any ledger row is exposed.
	if _, err := h.actions.Get(r.Context(), r.PathValue("id"), userID, isAdmin); err != nil {
		writeJSONError(w, http.StatusNotFound, "action not found")
		return
	}
	runs, err := h.actions.ListRuns(r.Context(), r.PathValue("id"), 50)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": runs})
}
