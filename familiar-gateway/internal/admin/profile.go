package admin

// User profile panel. The profile is the per-user "assistant
// personality" prompt stored in user_profiles.user_prompt — a
// user-authored block of behavioral instructions the pipeline
// surfaces as its own labeled section after the admin system
// prompt.
//
// Endpoints:
//
//   GET   /console/api/profile  — current user's personality prompt
//   PATCH /console/api/profile  — replace the personality prompt
//
//   Admin override: ?user_id=<id> on either endpoint routes the
//   read/write to another user. Non-admin requests with ?user_id=
//   are ignored — the session user always wins.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// ProfileStore is the narrow interface the profile handlers consume.
// *userprofile.Store satisfies it directly. Tests drop in a fake
// keyed by user ID.
type ProfileStore interface {
	Get(ctx context.Context, userID string) (string, error)
	Set(ctx context.Context, userID, prompt string) error
}

// AttachProfileStore wires the user-profile store into the handler
// so /console/api/profile endpoints can serve live data. Nil is
// tolerated — endpoints respond with 503 so the frontend renders a
// "profile disabled on this deploy" state.
func (h *Handler) AttachProfileStore(p ProfileStore) { h.profiles = p }

// profileScopeFor mirrors graphScopeFor: non-admin always sees own
// user; admin can pass ?user_id=<id> to manage another user's
// profile. Keeps the scoping rule centralized so future endpoints
// can reuse it.
func profileScopeFor(r *http.Request) string {
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		return ""
	}
	return adminUserScope(r, au)
}

// getProfile serves GET /console/api/profile. Returns the user's
// personality prompt. A missing row returns an empty string (not
// 404) so the frontend can render the editor in an initial "write
// your assistant's personality" state.
func (h *Handler) getProfile(w http.ResponseWriter, r *http.Request) {
	if h.profiles == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user profile not configured on this deploy")
		return
	}
	userID := profileScopeFor(r)
	if userID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	prompt, err := h.profiles.Get(r.Context(), userID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":     userID,
		"user_prompt": prompt,
	})
}

// patchProfile serves PATCH /console/api/profile. Body:
//
//	{"user_prompt": "<text>"}
//
// An empty string is a valid "no personality" state and clears any
// prior prompt.
func (h *Handler) patchProfile(w http.ResponseWriter, r *http.Request) {
	if h.profiles == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user profile not configured on this deploy")
		return
	}
	userID := profileScopeFor(r)
	if userID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body struct {
		UserPrompt string `json:"user_prompt"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := h.profiles.Set(r.Context(), userID, body.UserPrompt); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "updated",
		"user_id":     userID,
		"user_prompt": strings.TrimSpace(body.UserPrompt),
	})
}
