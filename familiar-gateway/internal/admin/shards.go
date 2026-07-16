package admin

// Shards admin surface (FAMILIAR-SHARDS-PHASE1-SPEC Steps 8-9):
//
//   GET    /admin/api/skills/tools             — registered tool names
//   GET    /admin/api/shards                   — list
//   POST   /admin/api/shards                   — create
//   GET    /admin/api/shards/{id}              — detail
//   PATCH  /admin/api/shards/{id}              — update
//   DELETE /admin/api/shards/{id}              — delete (cascades tokens)
//   POST   /admin/api/shards/{id}/disable      — soft-disable
//   POST   /admin/api/shards/{id}/enable       — re-enable
//   GET    /admin/api/shards/{id}/tokens       — token history
//   POST   /admin/api/shards/{id}/tokens       — mint (plaintext returned once)
//   POST   /admin/api/shard_tokens/{tid}/revoke — revoke by token id
//
// All endpoints are admin-only (wrapped in adminOnly at Mux registration).
// The package depends on two optional stores wired at startup:
// ShardStore (narrow subset of shards.Store) and SkillCatalog (narrow
// subset of skills.Registry). When either is nil the endpoints return
// 503 so the UI can render a "shards disabled on this deploy" state.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/shards"
)

// ShardStore is the admin-console-facing slice of *shards.PGStore. Kept
// narrow so tests can drop in a fake without the DB, matching the
// UserManager pattern.
type ShardStore interface {
	CreateShard(ctx context.Context, s *shards.Shard) error
	GetShard(ctx context.Context, id string) (*shards.Shard, error)
	ListShards(ctx context.Context, ownerID string) ([]*shards.Shard, error)
	ListAllShards(ctx context.Context) ([]*shards.Shard, error)
	UpdateShard(ctx context.Context, s *shards.Shard) error
	DisableShard(ctx context.Context, id string) error
	EnableShard(ctx context.Context, id string) error
	DeleteShard(ctx context.Context, id string) error

	CreateToken(ctx context.Context, ownerID, shardID, label string) (string, *shards.Token, error)
	ListTokens(ctx context.Context, shardID string) ([]*shards.Token, error)
	RevokeToken(ctx context.Context, id string) error
}

// SkillCatalog is the admin-facing view of the skill registry. Phase 1
// wired it narrow (ToolNames for the shard allowlist checklist,
// KnownToolNames for save-time validation); Phase C adds Skills()
// which returns the structured per-skill view the SKILLS catalog
// panel renders. *skills.Registry satisfies the tool-name methods
// directly; Skills is projected from the registry at wiring time in
// cmd/gateway.
type SkillCatalog interface {
	ToolNames() []string
	KnownToolNames() map[string]bool
	Skills() []SkillInfo
}

// SkillInfo is one entry in the catalog — a skill and every tool it
// exposes. Name / Description / Version mirror the skills.Skill
// interface; Tools carries one entry per registered tool with its
// LLM-facing parameter schema preserved as raw JSON so the frontend
// can render it verbatim.
type SkillInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Tools       []SkillToolInfo `json:"tools"`
}

// SkillToolInfo is one tool's wire shape for the catalog endpoint.
// Parameters is a JSON Schema object (the same json.RawMessage the
// registry already stores); the frontend decodes it for display.
type SkillToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// skillCatalogAdapter wraps the pieces cmd/gateway projects from
// *skills.Registry (flat tool-name slice + known-set + per-skill
// structured info) behind the SkillCatalog interface. Kept in this
// package so admin tests can drop in a fake without the full
// registry.
type skillCatalogAdapter struct {
	names  []string
	known  map[string]bool
	skills []SkillInfo
}

// NewSkillCatalog bundles the three views the admin handlers need.
// cmd/gateway walks skills.Registry once at startup to produce all
// three (ToolDefinitions for `toolNames`, KnownToolNames for `known`,
// SkillNames + Get loop for `skills`). Returning the projected
// snapshot keeps the admin package from importing the full registry
// type.
func NewSkillCatalog(toolNames []string, known map[string]bool, skills []SkillInfo) SkillCatalog {
	n := make([]string, len(toolNames))
	copy(n, toolNames)
	k := make(map[string]bool, len(known))
	for name := range known {
		k[name] = true
	}
	s := make([]SkillInfo, len(skills))
	copy(s, skills)
	return &skillCatalogAdapter{names: n, known: k, skills: s}
}

func (s *skillCatalogAdapter) ToolNames() []string             { return s.names }
func (s *skillCatalogAdapter) KnownToolNames() map[string]bool { return s.known }
func (s *skillCatalogAdapter) Skills() []SkillInfo             { return s.skills }

// AttachShardStore wires the shards persistence surface into the
// handler. Must run before Mux() if the /admin/api/shards endpoints
// should respond with live data; otherwise they return 503.
func (h *Handler) AttachShardStore(s ShardStore) { h.shards = s }

// AttachSkillCatalog wires the tool enumeration surface. Optional —
// when nil, listSkillTools returns an empty array (the UI falls back
// to rendering an empty checklist, so the operator sees the shape
// even without tools wired).
func (h *Handler) AttachSkillCatalog(c SkillCatalog) { h.skills = c }

// -----------------------------------------------------------------------------
// Wire DTOs
// -----------------------------------------------------------------------------

// shardDTO is the JSON shape the frontend consumes. Timestamps are
// formatted as RFC3339 strings (Go time.Time marshals that way by
// default). InputSchema and OutputSchema are passed through as opaque
// JSON so the frontend can render them in a textarea.
type shardDTO struct {
	ID              string          `json:"id"`
	OwnerID         string          `json:"owner_id"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Persistence     string          `json:"persistence"`
	Visibility      string          `json:"visibility"`
	ScopeTag        string          `json:"scope_tag"`
	ToolAllowlist   []string        `json:"tool_allowlist"`
	SystemPrompt    string          `json:"system_prompt"`
	ModelPreference string          `json:"model_preference,omitempty"`
	TierPreference  string          `json:"tier_preference,omitempty"`
	InputSchema     json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema    json.RawMessage `json:"output_schema,omitempty"`
	MaxTokens       int             `json:"max_tokens"`
	Temperature     float32         `json:"temperature"`
	// SHARD-AUTH-SPEC Phase 1 — passkey-driven login scoping. The
	// frontend's shard detail form edits these directly; the
	// loadShardPermissions middleware reads them per request.
	ConsoleAccess bool       `json:"console_access"`
	ConsolePanels []string   `json:"console_panels"`
	BookAccess    []string   `json:"book_access"`
	ChatEnabled   bool       `json:"chat_enabled"`
	APIEnabled    bool       `json:"api_enabled"`
	SessionMaxAge *int       `json:"session_max_age,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DisabledAt    *time.Time `json:"disabled_at,omitempty"`
	Active        bool       `json:"active"`
	TokenCount    int        `json:"token_count,omitempty"`
}

func toShardDTO(s *shards.Shard, tokenCount int) shardDTO {
	return shardDTO{
		ID:              s.ID,
		OwnerID:         s.OwnerID,
		Name:            s.Name,
		Description:     s.Description,
		Persistence:     string(s.Persistence),
		Visibility:      string(s.Visibility),
		ScopeTag:        s.ScopeTag,
		ToolAllowlist:   append([]string{}, s.ToolAllowlist...),
		SystemPrompt:    s.SystemPrompt,
		ModelPreference: s.ModelPreference,
		TierPreference:  s.TierPreference,
		InputSchema:     s.InputSchema,
		OutputSchema:    s.OutputSchema,
		MaxTokens:       s.MaxTokens,
		Temperature:     s.Temperature,
		ConsoleAccess:   s.ConsoleAccess,
		ConsolePanels:   append([]string{}, s.ConsolePanels...),
		BookAccess:      append([]string{}, s.BookAccess...),
		ChatEnabled:     s.ChatEnabled,
		APIEnabled:      s.APIEnabled,
		SessionMaxAge:   s.SessionMaxAge,
		CreatedAt:       s.CreatedAt,
		UpdatedAt:       s.UpdatedAt,
		DisabledAt:      s.DisabledAt,
		Active:          s.Active(),
		TokenCount:      tokenCount,
	}
}

// tokenDTO intentionally does NOT carry the plaintext — the only time
// plaintext is returned is from the mint endpoint, which uses a
// separate response shape. Keep the list-safe and mint-only shapes
// distinct so it's architecturally impossible for the list endpoint
// to ever leak a secret.
type tokenDTO struct {
	ID          string     `json:"id"`
	ShardID     string     `json:"shard_id"`
	Label       string     `json:"label"`
	TokenPrefix string     `json:"token_prefix"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Active      bool       `json:"active"`
}

func toTokenDTO(t *shards.Token) tokenDTO {
	return tokenDTO{
		ID:          t.ID,
		ShardID:     t.ShardID,
		Label:       t.Label,
		TokenPrefix: t.TokenPrefix,
		CreatedAt:   t.CreatedAt,
		LastUsedAt:  t.LastUsedAt,
		ExpiresAt:   t.ExpiresAt,
		RevokedAt:   t.RevokedAt,
		Active:      t.Active(),
	}
}

// mintTokenResponse is the ONE place the plaintext value crosses the
// wire. The frontend immediately shows it in a one-shot modal; any
// subsequent list endpoint only sees the DTO above.
type mintTokenResponse struct {
	Plaintext string   `json:"plaintext"`
	Token     tokenDTO `json:"token"`
}

// -----------------------------------------------------------------------------
// Request body types
// -----------------------------------------------------------------------------

// shardCreateBody matches the frontend's create form. Every field is
// required except model/tier preference (one or neither) and the
// optional schema blobs. Temperature defaults to 0.7 when zero to
// match the DB default.
type shardCreateBody struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Persistence     string          `json:"persistence"`
	Visibility      string          `json:"visibility"`
	ScopeTag        string          `json:"scope_tag"`
	ToolAllowlist   []string        `json:"tool_allowlist"`
	SystemPrompt    string          `json:"system_prompt"`
	ModelPreference string          `json:"model_preference"`
	TierPreference  string          `json:"tier_preference"`
	InputSchema     json.RawMessage `json:"input_schema"`
	OutputSchema    json.RawMessage `json:"output_schema"`
	MaxTokens       int             `json:"max_tokens"`
	Temperature     float32         `json:"temperature"`
	// SHARD-AUTH-SPEC Phase 1 scoping fields. All optional on
	// create — defaults match the DB column defaults
	// (console_access=false, chat_enabled=true, api_enabled=true).
	ConsoleAccess *bool    `json:"console_access,omitempty"`
	ConsolePanels []string `json:"console_panels,omitempty"`
	BookAccess    []string `json:"book_access,omitempty"`
	ChatEnabled   *bool    `json:"chat_enabled,omitempty"`
	APIEnabled    *bool    `json:"api_enabled,omitempty"`
	SessionMaxAge *int     `json:"session_max_age,omitempty"`
}

// shardUpdateBody is the PATCH shape. Every field is a pointer so the
// handler can distinguish "unset, keep existing value" from "explicit
// zero value." The ID is immutable — taken from the URL, not the body.
type shardUpdateBody struct {
	Name            *string          `json:"name,omitempty"`
	Description     *string          `json:"description,omitempty"`
	Persistence     *string          `json:"persistence,omitempty"`
	Visibility      *string          `json:"visibility,omitempty"`
	ScopeTag        *string          `json:"scope_tag,omitempty"`
	ToolAllowlist   *[]string        `json:"tool_allowlist,omitempty"`
	SystemPrompt    *string          `json:"system_prompt,omitempty"`
	ModelPreference *string          `json:"model_preference,omitempty"`
	TierPreference  *string          `json:"tier_preference,omitempty"`
	InputSchema     *json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema    *json.RawMessage `json:"output_schema,omitempty"`
	MaxTokens       *int             `json:"max_tokens,omitempty"`
	Temperature     *float32         `json:"temperature,omitempty"`
	// SHARD-AUTH-SPEC Phase 1 scoping fields. nil = "leave
	// unchanged" (the standard PATCH convention used elsewhere
	// in this struct). Empty arrays explicitly clear the field.
	ConsoleAccess *bool     `json:"console_access,omitempty"`
	ConsolePanels *[]string `json:"console_panels,omitempty"`
	BookAccess    *[]string `json:"book_access,omitempty"`
	ChatEnabled   *bool     `json:"chat_enabled,omitempty"`
	APIEnabled    *bool     `json:"api_enabled,omitempty"`
	SessionMaxAge *int      `json:"session_max_age,omitempty"`
}

type mintTokenBody struct {
	Label string `json:"label"`
}

// -----------------------------------------------------------------------------
// Skill catalog endpoint
// -----------------------------------------------------------------------------

// listSkillTools serves GET /admin/api/skills/tools. Returns the flat
// list of registered tool names so the frontend's allowlist checklist
// can render one checkbox per tool. The frontend also uses this to
// disable write-capable memory tools when a shard's persistence is
// set to ephemeral.
func (h *Handler) listSkillTools(w http.ResponseWriter, r *http.Request) {
	var names []string
	if h.skills != nil {
		names = h.skills.ToolNames()
	}
	if names == nil {
		names = []string{}
	}
	// Annotate each tool with whether it's write-capable so the UI
	// can grey it out for ephemeral shards without re-encoding the
	// logic client-side. The source of truth stays in the shards
	// package via shards.IsWriteCapable.
	type toolInfo struct {
		Name         string `json:"name"`
		WriteCapable bool   `json:"write_capable"`
	}
	out := make([]toolInfo, 0, len(names))
	for _, n := range names {
		out = append(out, toolInfo{
			Name:         n,
			WriteCapable: shards.IsWriteCapable(n),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// listSkillCatalog serves GET /console/api/skills (Phase C of
// FAMILIAR-CONSOLE-SPEC). Returns the per-skill structured view
// — skill name, description, version, and every tool the skill
// exposes including its parameter schema. Any authenticated user
// can read this; no role gating. When no skill registry is wired
// into the handler, returns an empty list so the frontend renders
// a sensible "no skills" state instead of a 503.
func (h *Handler) listSkillCatalog(w http.ResponseWriter, r *http.Request) {
	var items []SkillInfo
	if h.skills != nil {
		items = h.skills.Skills()
	}
	if items == nil {
		items = []SkillInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": items})
}

// -----------------------------------------------------------------------------
// Shard CRUD
// -----------------------------------------------------------------------------

// requireShardStore is the common 503 gate. Separated so each handler
// can call it and early-return consistently.
func (h *Handler) requireShardStore(w http.ResponseWriter) bool {
	if h.shards == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "shards not configured on this deploy")
		return false
	}
	return true
}

// ownerIDFor returns the canonical user ID that should own newly
// created shards for this request. Phase 1 assumes one admin — the
// authenticated user. Later phases may let admins create shards on
// behalf of other users.
func ownerIDFor(r *http.Request) string {
	if v, ok := r.Context().Value(ContextKeyUserID).(string); ok {
		return v
	}
	return ""
}

// canSeeShard returns true when the authenticated user is allowed to
// read or mutate this shard. Admins see every shard on the instance;
// non-admins only their own. Used by every per-shard endpoint to keep
// the role check in one place — adding a new endpoint is "look up
// the shard, call canSeeShard, 404 if false." The 404 (not 403) is
// deliberate per FAMILIAR-CONSOLE-SPEC: leaking the existence of
// other users' shards would let a non-admin enumerate IDs.
func canSeeShard(r *http.Request, sh *shards.Shard) bool {
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		return false
	}
	if au.IsAdmin() {
		return true
	}
	return sh.OwnerID == au.UserID
}

// refuseShardSession writes a 403 and returns true when the caller's
// session is a shard session. Shard sessions never manage shards —
// CRUD, tokens, enable/disable, anything that mutates a shard row or
// its envelope. Without this gate a constrained shard could PATCH
// its own row and grant itself an unrestricted permission envelope
// (empty BookAccess + ConsolePanels short-circuit to "all the
// owner's books / all panels"). The sibling shard-passkey routes
// already enforce this via resolveOwnedShard; the CRUD surface
// matches that posture here.
func (h *Handler) refuseShardSession(w http.ResponseWriter, r *http.Request) bool {
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return true
	}
	if au.IsShardSession() {
		writeJSONError(w, http.StatusForbidden, "shard sessions cannot manage shards")
		return true
	}
	return false
}

// listShards serves GET /console/api/shards. Returns every shard the
// caller can see — admin sees all instance shards, non-admin sees
// only their own — newest first. Each row includes a token_count so
// the list view can show it without a second round-trip per shard.
func (h *Handler) listShards(w http.ResponseWriter, r *http.Request) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}

	var (
		list []*shards.Shard
		err  error
	)
	if au.IsAdmin() {
		list, err = h.shards.ListAllShards(r.Context())
	} else {
		list, err = h.shards.ListShards(r.Context(), au.UserID)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]shardDTO, 0, len(list))
	for _, s := range list {
		toks, err := h.shards.ListTokens(r.Context(), s.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		items = append(items, toShardDTO(s, len(toks)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getShard serves GET /admin/api/shards/{id}.
func (h *Handler) getShard(w http.ResponseWriter, r *http.Request) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	s, err := h.shards.GetShard(r.Context(), id)
	if errors.Is(err, shards.ErrShardNotFound) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !canSeeShard(r, s) {
		// Don't leak the existence of a shard owned by another user.
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	toks, _ := h.shards.ListTokens(r.Context(), s.ID)
	writeJSON(w, http.StatusOK, toShardDTO(s, len(toks)))
}

// createShard serves POST /admin/api/shards.
func (h *Handler) createShard(w http.ResponseWriter, r *http.Request) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	ownerID := ownerIDFor(r)
	if ownerID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body shardCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// Apply defaults that match the DB schema defaults, so the
	// frontend can submit a partially-filled form.
	if strings.TrimSpace(body.ScopeTag) == "" && body.ID != "" {
		body.ScopeTag = "shard:" + body.ID
	}
	if body.MaxTokens == 0 {
		body.MaxTokens = 2048
	}
	if body.Temperature == 0 && !strings.Contains(string(body.InputSchema), "temperature_zero") {
		// Match the DB default. Callers who genuinely want 0.0 get it
		// by submitting the shard twice (create then PATCH) — the
		// form UI in Phase 1 doesn't expose 0.0 explicitly. This is
		// a known rough edge tracked with the "temperature_zero"
		// sentinel above; it's acceptable for Phase 1 ephemeral
		// extractors whose authors set e.g. 0.1.
		body.Temperature = 0.7
	}

	// Default the scoping toggles to the DB column defaults so an
	// older frontend that doesn't send them keeps shipping shards
	// with the legacy posture (console_access off, chat/api on).
	consoleAccess := false
	if body.ConsoleAccess != nil {
		consoleAccess = *body.ConsoleAccess
	}
	chatEnabled := true
	if body.ChatEnabled != nil {
		chatEnabled = *body.ChatEnabled
	}
	apiEnabled := true
	if body.APIEnabled != nil {
		apiEnabled = *body.APIEnabled
	}

	s := &shards.Shard{
		ID:              body.ID,
		OwnerID:         ownerID,
		Name:            body.Name,
		Description:     body.Description,
		Persistence:     shards.PersistenceMode(body.Persistence),
		Visibility:      shards.VisibilityMode(body.Visibility),
		ScopeTag:        body.ScopeTag,
		ToolAllowlist:   body.ToolAllowlist,
		SystemPrompt:    body.SystemPrompt,
		ModelPreference: body.ModelPreference,
		TierPreference:  body.TierPreference,
		InputSchema:     body.InputSchema,
		OutputSchema:    body.OutputSchema,
		MaxTokens:       body.MaxTokens,
		Temperature:     body.Temperature,
		ConsoleAccess:   consoleAccess,
		ConsolePanels:   body.ConsolePanels,
		BookAccess:      body.BookAccess,
		ChatEnabled:     chatEnabled,
		APIEnabled:      apiEnabled,
		SessionMaxAge:   body.SessionMaxAge,
	}

	// Validate allowlist against the registered catalog if available.
	// This is the single place the admin UI can prevent a shard from
	// being saved with a tool that doesn't exist at runtime.
	var known map[string]bool
	if h.skills != nil {
		known = h.skills.KnownToolNames()
	}
	if err := shards.ValidateAllowlist(s.ToolAllowlist, s.Persistence, known); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.shards.CreateShard(r.Context(), s); err != nil {
		// Validation failures from the store (slug format, mode enum,
		// etc.) come back as wrapped errors on pre-flight checks.
		// Integrity violations (duplicate id, duplicate scope_tag)
		// surface as DB errors. Both deserve 400 so the UI can show
		// the message inline.
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Re-load so the response includes timestamps + DB-assigned defaults.
	loaded, err := h.shards.GetShard(r.Context(), s.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toShardDTO(loaded, 0))
}

// patchShard serves PATCH /admin/api/shards/{id}.
func (h *Handler) patchShard(w http.ResponseWriter, r *http.Request) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	existing, err := h.shards.GetShard(r.Context(), id)
	if errors.Is(err, shards.ErrShardNotFound) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !canSeeShard(r, existing) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}

	var body shardUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	// Apply supplied fields onto the existing shard.
	merged := *existing
	if body.Name != nil {
		merged.Name = *body.Name
	}
	if body.Description != nil {
		merged.Description = *body.Description
	}
	if body.Persistence != nil {
		merged.Persistence = shards.PersistenceMode(*body.Persistence)
	}
	if body.Visibility != nil {
		merged.Visibility = shards.VisibilityMode(*body.Visibility)
	}
	if body.ScopeTag != nil {
		merged.ScopeTag = *body.ScopeTag
	}
	if body.ToolAllowlist != nil {
		merged.ToolAllowlist = *body.ToolAllowlist
	}
	if body.SystemPrompt != nil {
		merged.SystemPrompt = *body.SystemPrompt
	}
	if body.ModelPreference != nil {
		merged.ModelPreference = *body.ModelPreference
	}
	if body.TierPreference != nil {
		merged.TierPreference = *body.TierPreference
	}
	if body.InputSchema != nil {
		merged.InputSchema = *body.InputSchema
	}
	if body.OutputSchema != nil {
		merged.OutputSchema = *body.OutputSchema
	}
	if body.MaxTokens != nil {
		merged.MaxTokens = *body.MaxTokens
	}
	if body.Temperature != nil {
		merged.Temperature = *body.Temperature
	}
	if body.ConsoleAccess != nil {
		merged.ConsoleAccess = *body.ConsoleAccess
	}
	if body.ConsolePanels != nil {
		merged.ConsolePanels = *body.ConsolePanels
	}
	if body.BookAccess != nil {
		merged.BookAccess = *body.BookAccess
	}
	if body.ChatEnabled != nil {
		merged.ChatEnabled = *body.ChatEnabled
	}
	if body.APIEnabled != nil {
		merged.APIEnabled = *body.APIEnabled
	}
	if body.SessionMaxAge != nil {
		merged.SessionMaxAge = body.SessionMaxAge
	}

	// Re-validate the allowlist against the updated persistence mode.
	var known map[string]bool
	if h.skills != nil {
		known = h.skills.KnownToolNames()
	}
	if err := shards.ValidateAllowlist(merged.ToolAllowlist, merged.Persistence, known); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.shards.UpdateShard(r.Context(), &merged); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	loaded, err := h.shards.GetShard(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	toks, _ := h.shards.ListTokens(r.Context(), id)
	writeJSON(w, http.StatusOK, toShardDTO(loaded, len(toks)))
}

// deleteShard serves DELETE /admin/api/shards/{id}. Cascades tokens
// via the DB FK; operators who want a paper trail should Disable
// instead.
func (h *Handler) deleteShard(w http.ResponseWriter, r *http.Request) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	existing, err := h.shards.GetShard(r.Context(), id)
	if errors.Is(err, shards.ErrShardNotFound) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !canSeeShard(r, existing) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	// Scheduled actions bound to this shard are disabled BEFORE the
	// delete — the FK is ON DELETE SET NULL, so afterwards they'd be
	// indistinguishable from deliberately-trusted actions and would
	// silently run with the full tool set. Demotion-by-deletion must
	// be loud and inert, never an escalation; last_status =
	// 'shard_deleted' tells the owner why the action stopped, and
	// re-enabling (with or without a new envelope) is an explicit act.
	var disabledActions int64
	if h.actions != nil {
		n, aErr := h.actions.DisableByShard(r.Context(), id)
		if aErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "disable dependent actions: "+aErr.Error())
			return
		}
		disabledActions = n
		if n > 0 {
			log.Printf("[admin] shard %s delete: disabled %d dependent scheduled action(s)", id, n)
		}
	}
	if err := h.shards.DeleteShard(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "deleted", "id": id,
		"disabled_actions": disabledActions,
	})
}

// disableShard serves POST /admin/api/shards/{id}/disable.
func (h *Handler) disableShard(w http.ResponseWriter, r *http.Request) {
	h.setShardDisabled(w, r, true)
}

// enableShard serves POST /admin/api/shards/{id}/enable.
func (h *Handler) enableShard(w http.ResponseWriter, r *http.Request) {
	h.setShardDisabled(w, r, false)
}

func (h *Handler) setShardDisabled(w http.ResponseWriter, r *http.Request, disable bool) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	existing, err := h.shards.GetShard(r.Context(), id)
	if errors.Is(err, shards.ErrShardNotFound) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !canSeeShard(r, existing) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	if disable {
		err = h.shards.DisableShard(r.Context(), id)
	} else {
		err = h.shards.EnableShard(r.Context(), id)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	loaded, _ := h.shards.GetShard(r.Context(), id)
	toks, _ := h.shards.ListTokens(r.Context(), id)
	writeJSON(w, http.StatusOK, toShardDTO(loaded, len(toks)))
}

// -----------------------------------------------------------------------------
// Token CRUD
// -----------------------------------------------------------------------------

// listShardTokens serves GET /admin/api/shards/{id}/tokens.
func (h *Handler) listShardTokens(w http.ResponseWriter, r *http.Request) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	existing, err := h.shards.GetShard(r.Context(), id)
	if errors.Is(err, shards.ErrShardNotFound) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !canSeeShard(r, existing) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}

	toks, err := h.shards.ListTokens(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]tokenDTO, 0, len(toks))
	for _, t := range toks {
		items = append(items, toTokenDTO(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// createShardToken serves POST /admin/api/shards/{id}/tokens. Returns
// the plaintext in the response body — this is the ONE time it's
// visible. The admin UI renders it in a one-shot modal with a
// copy-to-clipboard control, then discards it.
func (h *Handler) createShardToken(w http.ResponseWriter, r *http.Request) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	ownerID := ownerIDFor(r)
	if ownerID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	existing, err := h.shards.GetShard(r.Context(), id)
	if errors.Is(err, shards.ErrShardNotFound) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Token minting is intentionally strict: even an admin cannot mint
	// against another user's shard. The minted token's owner_id is the
	// caller's; the shardapi auth path checks email == shard.owner ==
	// token.owner, so a token an admin minted against Alison's shard
	// would never authenticate when invoked. 404-not-found on the
	// shard is the same answer a non-admin would see, keeping the
	// behavior consistent.
	if existing.OwnerID != ownerID {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	if !existing.Active() {
		writeJSONError(w, http.StatusConflict, "shard is disabled; re-enable before minting tokens")
		return
	}

	var body mintTokenBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// Empty body is fine (label defaults to empty string); we only
		// 400 on a malformed body.
		if !errors.Is(err, errors.New("EOF")) && err.Error() != "EOF" {
			writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}

	plaintext, tok, err := h.shards.CreateToken(r.Context(), ownerID, id, body.Label)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, mintTokenResponse{
		Plaintext: plaintext,
		Token:     toTokenDTO(tok),
	})
}

// revokeShardToken serves POST /admin/api/shard_tokens/{tid}/revoke.
// Idempotent — revoking an already-revoked token is still 200. The
// request body is empty.
//
// We don't expose the token row directly here (no GetToken endpoint)
// — the revoke path takes a token id and the store's RevokeToken is
// idempotent, so "did it exist" is answered by ErrTokenNotFound. That
// keeps the endpoint a simple POST with no lookup round-trip.
func (h *Handler) revokeShardToken(w http.ResponseWriter, r *http.Request) {
	if h.refuseShardSession(w, r) {
		return
	}
	if !h.requireShardStore(w) {
		return
	}
	tid := r.PathValue("tid")
	if tid == "" {
		writeJSONError(w, http.StatusBadRequest, "missing token id")
		return
	}
	// Ownership check: find the token's shard, check the current
	// user is allowed to revoke. Admins can revoke any token (they
	// can also disable/delete the parent shard, so revoke is a
	// strictly-narrower power). Non-admins only see their own.
	au, _ := AuthUserFrom(r.Context())
	allowed, err := h.canRevokeToken(r.Context(), au, tid)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusNotFound, "token not found")
		return
	}
	if err := h.shards.RevokeToken(r.Context(), tid); err != nil {
		if errors.Is(err, shards.ErrTokenNotFound) {
			writeJSONError(w, http.StatusNotFound, "token not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "id": tid})
}

// canRevokeToken decides whether the authenticated user is allowed
// to revoke this token. Admins can revoke any token; non-admins can
// only revoke tokens on shards they own. Implementation walks the
// caller's visible shards (all instance shards for admin, own only
// for non-admin) and checks each one's token list — O(n_shards) per
// revoke, which is fine at Phase 1 scale. A dedicated GetToken
// helper on the store would let this be O(1); deferred until tokens
// outgrow "list all per check" cheap.
//
// Returns false (without an error) when the token doesn't exist or
// the caller can't see it; the handler surfaces both as 404 to avoid
// leaking token existence across users.
func (h *Handler) canRevokeToken(ctx context.Context, au AuthUser, tokenID string) (bool, error) {
	if au.UserID == "" {
		return false, nil
	}
	var shardsList []*shards.Shard
	var err error
	if au.IsAdmin() {
		shardsList, err = h.shards.ListAllShards(ctx)
	} else {
		shardsList, err = h.shards.ListShards(ctx, au.UserID)
	}
	if err != nil {
		return false, err
	}
	for _, s := range shardsList {
		toks, err := h.shards.ListTokens(ctx, s.ID)
		if err != nil {
			return false, err
		}
		for _, t := range toks {
			if t.ID == tokenID {
				return true, nil
			}
		}
	}
	return false, nil
}
