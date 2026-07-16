package admin

// HTTP handlers for /console/api/shards/{id}/passkeys — the four
// CRUD-style endpoints SHARD-AUTH-SPEC Phase 1 calls out:
//
//   GET    .../passkeys              list active passkeys
//   POST   .../passkeys/begin        start WebAuthn registration
//   POST   .../passkeys/finish       complete registration
//   DELETE .../passkeys/{passkey_id} revoke
//
// All gated by an authenticated USER session (shard sessions can't
// manage their own passkeys — that's the whole point of the
// owner/shard split). Ownership check uses canSeeShard so admins
// can manage any shard while non-admins are scoped to their own.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/familiar/gateway/internal/shards"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// requireShardPasskeys is the 503 gate. Passkey management
// requires both the shard store and the passkey store wired —
// the shard store for the ownership lookup, the passkey store
// for the actual writes.
func (h *Handler) requireShardPasskeys(w http.ResponseWriter) bool {
	if !h.requireShardStore(w) {
		return false
	}
	if h.shardPasskeys == nil {
		writeJSONError(w, http.StatusServiceUnavailable,
			"shard passkeys not configured on this deploy")
		return false
	}
	return true
}

// resolveOwnedShard does the (load + ownership-check) sequence
// every passkey handler needs at the top. Returns the shard or
// nil + ok=false (response already written). Refuses shard
// sessions outright — only user sessions can manage passkeys.
func (h *Handler) resolveOwnedShard(w http.ResponseWriter, r *http.Request) (*shards.Shard, bool) {
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return nil, false
	}
	if au.IsShardSession() {
		writeJSONError(w, http.StatusForbidden,
			"shard sessions cannot manage passkeys")
		return nil, false
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing shard id")
		return nil, false
	}
	sh, err := h.shards.GetShard(r.Context(), id)
	if errors.Is(err, shards.ErrShardNotFound) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return nil, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if !canSeeShard(r, sh) {
		// 404 (not 403) so non-admins can't probe other users' shard IDs.
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return nil, false
	}
	return sh, true
}

// shardPasskeyDTO is the trimmed list-view shape. We never expose
// the credential blob or sign_count over the wire — those are
// internal to the WebAuthn ceremony.
type shardPasskeyDTO struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Transports []string   `json:"transports,omitempty"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func toShardPasskeyDTO(p StoredShardPasskey) shardPasskeyDTO {
	return shardPasskeyDTO{
		ID: p.ID, Label: p.Label, Transports: p.Transports,
		CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt,
		LastUsedAt: p.LastUsedAt,
	}
}

// listShardPasskeys serves GET /console/api/shards/{id}/passkeys.
// Active rows only.
func (h *Handler) listShardPasskeys(w http.ResponseWriter, r *http.Request) {
	if !h.requireShardPasskeys(w) {
		return
	}
	sh, ok := h.resolveOwnedShard(w, r)
	if !ok {
		return
	}
	rows, err := h.shardPasskeys.ListByShard(r.Context(), sh.ID, false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]shardPasskeyDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, toShardPasskeyDTO(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// shardPasskeyRegisterBegin starts a WebAuthn registration ceremony
// bound to the shard. Body: {"label":"<device name>"}. Same shape
// as user registration begin (the response is the
// PublicKeyCredentialCreationOptions blob + a pending-cookie token
// that the finish handler picks up).
func (h *Handler) shardPasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !h.requireShardPasskeys(w) {
		return
	}
	sh, ok := h.resolveOwnedShard(w, r)
	if !ok {
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	existing, err := h.shardPasskeys.ListByShard(r.Context(), sh.ID, false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	user := newShardWebAuthnUser(sh, existing)
	wa, err := h.webauthnFor(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	creation, sessionData, err := wa.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		}),
	)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin registration: "+err.Error())
		return
	}
	// Stash the pending session data along with the shard id +
	// label. The pending store's value-shape carries a userID
	// string today; we tag it as "shard:{shardID}|{label}" so
	// finish can disambiguate user-reg from shard-reg without a
	// schema change. Same TTL.
	au, _ := AuthUserFrom(r.Context())
	token, err := randomToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.pending.put(token, *sessionData, PendingKindShardRegister, "shard:"+sh.ID+"|"+body.Label+"|"+au.UserID)
	h.setPendingCookie(w, token)
	writeJSON(w, http.StatusOK, creation)
}

// shardPasskeyRegisterFinish parses the client's credential.create()
// response and stores the resulting Credential against the shard.
func (h *Handler) shardPasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !h.requireShardPasskeys(w) {
		return
	}
	sh, ok := h.resolveOwnedShard(w, r)
	if !ok {
		return
	}
	cookie, err := r.Cookie(pendingCookieName)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing pending cookie")
		return
	}
	entry, ok := h.pending.take(cookie.Value)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "pending session expired or missing")
		return
	}
	// Refuse cross-flow pending entries — only shard-register
	// entries reach here. The kind check is the primary gate; the
	// userID tag below cross-validates the shard id.
	if entry.kind != PendingKindShardRegister {
		writeJSONError(w, http.StatusBadRequest, "pending session is not a shard registration")
		return
	}
	h.clearPendingCookie(w)

	// Decode the pending tag. Belt-and-suspenders: refuse if the
	// pending entry is for a different shard.
	shardID, label, createdBy, ok := splitShardPendingTag(entry.userID)
	if !ok || shardID != sh.ID {
		writeJSONError(w, http.StatusBadRequest, "pending session is not for this shard")
		return
	}

	existing, err := h.shardPasskeys.ListByShard(r.Context(), sh.ID, false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	user := newShardWebAuthnUser(sh, existing)
	wa, err := h.webauthnFor(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	cred, err := wa.FinishRegistration(user, entry.data, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "finish registration: "+err.Error())
		return
	}
	id, err := h.shardPasskeys.Insert(r.Context(), sh.ID, label, createdBy, cred)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store credential: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":            id,
		"label":         label,
		"credential_id": encodeCredentialID(cred.ID),
	})
}

// deleteShardPasskey serves DELETE
// /console/api/shards/{id}/passkeys/{passkey_id}. Soft-revoke;
// the row stays for audit.
func (h *Handler) deleteShardPasskey(w http.ResponseWriter, r *http.Request) {
	if !h.requireShardPasskeys(w) {
		return
	}
	sh, ok := h.resolveOwnedShard(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("passkey_id")
	if pid == "" {
		writeJSONError(w, http.StatusBadRequest, "missing passkey id")
		return
	}
	// Confirm the passkey actually belongs to this shard before
	// revoking — otherwise an admin could end-run the per-shard
	// scoping by hitting any shard's URL with an unrelated
	// passkey_id.
	rows, err := h.shardPasskeys.ListByShard(r.Context(), sh.ID, true)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	owns := false
	for _, p := range rows {
		if p.ID == pid {
			owns = true
			break
		}
	}
	if !owns {
		writeJSONError(w, http.StatusNotFound, "passkey not found")
		return
	}
	if err := h.shardPasskeys.Revoke(r.Context(), pid); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "passkey not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "id": pid})
}

// splitShardPendingTag decodes the "shard:{shardID}|{label}|{createdBy}"
// tag we stash on the pending entry's UserID field. Returns (shardID,
// label, createdBy, ok). Tolerates labels with embedded "|" by
// splitting at most twice.
func splitShardPendingTag(tag string) (string, string, string, bool) {
	const prefix = "shard:"
	if len(tag) < len(prefix) || tag[:len(prefix)] != prefix {
		return "", "", "", false
	}
	rest := tag[len(prefix):]
	// shardID|label|createdBy — split on the FIRST '|' for shardID,
	// then on the LAST '|' for createdBy, leaving label in the middle.
	first := -1
	last := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '|' {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || last <= first {
		return "", "", "", false
	}
	return rest[:first], rest[first+1 : last], rest[last+1:], true
}
