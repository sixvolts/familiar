package admin

// Instance-level settings: admin-editable config that lives in the
// DB (so it survives restarts and doesn't need a redeploy to change)
// but is NOT read from the DB on the request hot path. The pipeline
// reads the cached system prompt out of the in-memory PromptStore;
// this store is touched only at boot (to seed the cache) and when an
// admin saves (to refresh it). See TESTING-PLAN.md's sibling
// reasoning — DB-as-persistence is free; DB-reads-per-turn are not.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/familiar/gateway/internal/ctxbuild"
	"github.com/familiar/gateway/internal/db"
)

// Instance-settings keys. Stored as plain TEXT rows in
// instance_settings; booleans are the literal strings "true"/"false".
const (
	// SettingSystemPromptBase is the admin override for the prompt
	// base layer. Empty means "use the file-loaded base.md". Tier
	// overlays + tool_policy still apply on top either way.
	SettingSystemPromptBase = "system_prompt_base"
	// SettingSystemPromptUserVisible gates whether non-admin users
	// may view the system prompt. "true" / "false".
	SettingSystemPromptUserVisible = "system_prompt_user_visible"
)

// InstanceSettingsStore is the persistence surface for
// instance_settings. Backed by the shared pool.
type InstanceSettingsStore struct {
	db *db.Pool
}

// NewInstanceSettingsStore wires a store onto the shared pool.
// Returns nil if pool is nil, mirroring the other admin stores.
func NewInstanceSettingsStore(pool *db.Pool) *InstanceSettingsStore {
	if pool == nil || pool.DB == nil {
		return nil
	}
	return &InstanceSettingsStore{db: pool}
}

// Get returns one setting's value, or "" when the key is absent.
func (s *InstanceSettingsStore) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM instance_settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// GetAll returns every setting as a map. Used at boot to seed the
// in-memory caches in one round-trip.
func (s *InstanceSettingsStore) GetAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM instance_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Set upserts one setting.
func (s *InstanceSettingsStore) Set(ctx context.Context, key, value, updatedBy string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO instance_settings (key, value, updated_at, updated_by)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (key) DO UPDATE
		   SET value = EXCLUDED.value,
		       updated_at = NOW(),
		       updated_by = EXCLUDED.updated_by`,
		key, value, updatedBy)
	return err
}

// AttachInstanceSettings wires the settings store + the live
// PromptStore so the system-prompt endpoints can both persist a
// change and refresh the in-memory base override the pipeline
// reads. Both are optional — when either is nil the endpoints
// return 503.
func (h *Handler) AttachInstanceSettings(store *InstanceSettingsStore, ps *ctxbuild.PromptStore) {
	h.instanceSettings = store
	h.promptStore = ps
}

// systemPromptResponse is the wire shape for GET /system-prompt.
type systemPromptResponse struct {
	// Base is the prompt base layer — the admin override when set,
	// otherwise the file-loaded base.md. This is what both the
	// admin editor and the (optional) user-facing viewer display.
	Base string `json:"base"`
	// UserVisible reflects the admin toggle.
	UserVisible bool `json:"user_visible"`
	// HasOverride is true when an admin override is in effect (vs
	// the file-loaded default). Admin-only field — informational.
	HasOverride bool `json:"has_override,omitempty"`
	// Editable is true for admin callers (the PUT will succeed).
	Editable bool `json:"editable"`
}

// getSystemPrompt serves GET /console/api/system-prompt. Admins
// always get the prompt + the toggle state. Non-admins get it only
// when the admin has flipped system_prompt_user_visible on; otherwise
// 403. This is a settings-UI call, not the chat hot path, so reading
// the DB here is fine.
func (h *Handler) getSystemPrompt(w http.ResponseWriter, r *http.Request) {
	if h.instanceSettings == nil || h.promptStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "system prompt settings not configured")
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	visibleRaw, err := h.instanceSettings.Get(r.Context(), SettingSystemPromptUserVisible)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	userVisible := visibleRaw == "true"
	if !au.IsAdmin() && !userVisible {
		writeJSONError(w, http.StatusForbidden, "the system prompt is not visible to users on this deployment")
		return
	}
	resp := systemPromptResponse{
		Base:        h.promptStore.EffectiveBase(),
		UserVisible: userVisible,
		Editable:    au.IsAdmin(),
	}
	if au.IsAdmin() {
		resp.HasOverride = h.promptStore.HasBaseOverride()
	}
	writeJSON(w, http.StatusOK, resp)
}

// putSystemPrompt serves PUT /console/api/system-prompt (admin-only;
// wrapped in adminOnly at registration). Body:
//
//	{"base": "<prompt text>", "user_visible": true|false}
//
// An empty base clears the override — the pipeline falls back to the
// file-loaded base.md. Writes the DB, then refreshes the in-memory
// PromptStore so the change takes effect on the next turn without a
// gateway restart.
func (h *Handler) putSystemPrompt(w http.ResponseWriter, r *http.Request) {
	if h.instanceSettings == nil || h.promptStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "system prompt settings not configured")
		return
	}
	au, _ := AuthUserFrom(r.Context())
	var body struct {
		Base        string `json:"base"`
		UserVisible bool   `json:"user_visible"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	base := strings.TrimSpace(body.Base)
	if err := h.instanceSettings.Set(r.Context(), SettingSystemPromptBase, base, au.UserID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	visibleStr := "false"
	if body.UserVisible {
		visibleStr = "true"
	}
	if err := h.instanceSettings.Set(r.Context(), SettingSystemPromptUserVisible, visibleStr, au.UserID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Refresh the in-memory cache the pipeline reads.
	h.promptStore.SetBaseOverride(base)

	writeJSON(w, http.StatusOK, systemPromptResponse{
		Base:        h.promptStore.EffectiveBase(),
		UserVisible: body.UserVisible,
		HasOverride: h.promptStore.HasBaseOverride(),
		Editable:    true,
	})
}
