package admin

// HTTP handlers for the cross-domain passkey enrollment flow
// (CROSS-DOMAIN-ENROLLMENT.md). The flow lets an authenticated
// user on one RP mint a single-use token bound to a target RP;
// opening the link on the target origin walks them through a
// fresh WebAuthn registration ceremony against that RP.
//
// Endpoint layout:
//
//   POST /console/api/auth/enrollment-token  (session-authed)
//     Issue a token. canonical_id defaults to the caller; admins
//     can issue on behalf of any user via the optional field.
//
//   GET  /console/api/auth/passkeys          (session-authed)
//     Return the caller's existing passkeys (per-RP grouping) +
//     the list of RPs they're missing. Drives the profile
//     popover's "Add passkey" UI.
//
//   POST /console/api/auth/enroll/begin      (token-authed)
//     Validate the token + start the registration ceremony.
//     Returns the publicKey options the browser feeds to
//     navigator.credentials.create.
//
//   POST /console/api/auth/enroll/finish     (token-authed)
//     Validate token + ceremony attestation, write the credential
//     to the store, consume the token. The caller is NOT logged
//     in by this — they must authenticate normally after.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// createEnrollmentTokenRequest is the POST body shape.
type createEnrollmentTokenRequest struct {
	// CanonicalID is the user receiving the token. Omit (or set to
	// the caller's own canonical_id) for self-issuance. Admins may
	// supply any other canonical_id.
	CanonicalID string `json:"canonical_id,omitempty"`
	// TargetRPID is the RP the new passkey will register against.
	// Must match one of the configured [[admin.relying_party]]
	// blocks.
	TargetRPID string `json:"target_rp_id"`
}

// enrollmentTokenResponse is what the issuer hands back. The URL is
// constructed from the target RP's first origin, so the caller can
// surface it directly to the user (copy-paste or admin-shares-link).
type enrollmentTokenResponse struct {
	Token       string    `json:"token"`
	URL         string    `json:"url"`
	ExpiresAt   time.Time `json:"expires_at"`
	TargetRPID  string    `json:"target_rp_id"`
	CanonicalID string    `json:"canonical_id"`
}

// createEnrollmentToken issues a new enrollment token. The caller
// must be authenticated; the canonical_id defaults to the caller
// when omitted. Admins are allowed to issue on behalf of others.
func (h *Handler) createEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	if h.enrollTokens == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "enrollment not configured")
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "auth required")
		return
	}
	var body createEnrollmentTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	targetRP := strings.TrimSpace(body.TargetRPID)
	if targetRP == "" {
		writeJSONError(w, http.StatusBadRequest, "target_rp_id required")
		return
	}
	rp, ok := h.findRelyingParty(targetRP)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown target_rp_id %q", targetRP))
		return
	}

	// canonical_id resolution: empty / matches caller → self-issue,
	// fine for any authenticated user. Different from caller →
	// requires admin role.
	canonical := strings.TrimSpace(body.CanonicalID)
	if canonical == "" {
		canonical = au.UserID
	}
	if canonical != au.UserID && !au.IsAdmin() {
		writeJSONError(w, http.StatusForbidden,
			"only admins can issue enrollment tokens for other users")
		return
	}

	tok, err := h.enrollTokens.Issue(r.Context(), canonical, rp.RPID, au.UserID)
	if errors.Is(err, ErrEnrollmentRateLimited) {
		writeJSONError(w, http.StatusTooManyRequests,
			"too many active enrollment tokens — wait for older ones to expire")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Build the user-facing URL from the RP's first origin. Workspace
	// hosts /enroll; the page reads ?token=... and walks the user
	// through the ceremony against the gateway's /enroll/begin +
	// /enroll/finish endpoints.
	url := ""
	if len(rp.Origins) > 0 {
		url = strings.TrimRight(rp.Origins[0], "/") + "/enroll?token=" + tok.Token
	}
	writeJSON(w, http.StatusOK, enrollmentTokenResponse{
		Token:       tok.Token,
		URL:         url,
		ExpiresAt:   tok.ExpiresAt,
		TargetRPID:  tok.TargetRPID,
		CanonicalID: tok.CanonicalID,
	})
}

// passkeyDescriptor is one entry in the user's passkey list. The
// credential ID is base64url; the display name comes from whatever
// the registering ceremony recorded ("Passkey — owner (2026-04-24)"
// etc.). rp_id is derived from the credential's webauthn.Credential
// blob.
//
// AuthenticatorType distinguishes platform passkeys (Touch ID,
// Windows Hello, iCloud Keychain) from cross-platform security
// keys (YubiKey etc.) so the UI can chip them differently. Values:
// "platform" / "cross-platform" / "unknown" — the last covers
// credentials registered before the field was populated, plus the
// rare case where the authenticator refuses to declare an
// attachment.
//
// Transports is the AuthenticatorTransport list the library
// recorded at registration time (usb / nfc / ble / internal /
// hybrid). UI uses it as a fallback signal when AuthenticatorType
// is unknown — anything carrying "internal" is a platform
// authenticator regardless of what Attachment said.
type passkeyDescriptor struct {
	RPID              string     `json:"rp_id"`
	CredentialID      string     `json:"credential_id"`
	DisplayName       string     `json:"display_name"`
	CreatedAt         time.Time  `json:"created_at"`
	LastUsed          *time.Time `json:"last_used,omitempty"`
	AuthenticatorType string     `json:"authenticator_type"`
	Transports        []string   `json:"transports,omitempty"`
}

// availableRP is the per-RP summary the profile popover needs to
// render the "Add passkey for X" affordance.
type availableRP struct {
	RPID        string `json:"rp_id"`
	DisplayName string `json:"display_name"`
	Origin      string `json:"origin"` // first configured origin, for link surfacing
}

type passkeysListResponse struct {
	CanonicalID  string              `json:"canonical_id"`
	Passkeys     []passkeyDescriptor `json:"passkeys"`
	AvailableRPs []availableRP       `json:"available_rps"`
}

// listUserPasskeys returns the caller's enrolled passkeys + every
// configured RP (so the UI can show one row per RP with an "Add
// passkey" button when no credential is present for that RP).
//
// RP membership for an existing credential is derived from the
// stored webauthn.Credential's authenticator AAGUID-less blob —
// we don't currently record rp_id alongside the credential row,
// but we can infer it by comparing the credential's stored RP
// hash against each configured RPID's SHA-256.
func (h *Handler) listUserPasskeys(w http.ResponseWriter, r *http.Request) {
	if h.enrollTokens == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "enrollment not configured")
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "auth required")
		return
	}

	creds, err := h.credentials.ListByUser(r.Context(), au.UserID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	configured := h.cfg.EffectiveRelyingParties()
	// Local view of the configured RPs so credentialRPID can stay
	// decoupled from the config package's RelyingPartyConfig type.
	views := make([]relyingPartyConfigView, 0, len(configured))
	for _, rp := range configured {
		views = append(views, relyingPartyConfigView{RPID: rp.RPID, Origins: rp.Origins})
	}
	// Build an RPID → originHint map so the response can carry
	// the link target for the "Add passkey" button.
	originByRP := make(map[string]string, len(configured))
	for _, rp := range configured {
		if len(rp.Origins) > 0 {
			originByRP[rp.RPID] = rp.Origins[0]
		}
	}

	// Match each credential to its RPID. WebAuthn stores the RP ID
	// hash in the authenticator data; the go-webauthn library
	// preserves the original RPID on the Credential when available.
	// We fall back to hash-matching against each configured RP if
	// the field is empty (older credential rows pre-dating multi-RP
	// support).
	passkeys := make([]passkeyDescriptor, 0, len(creds))
	credByRP := make(map[string]struct{}, len(creds))
	for _, c := range creds {
		rpID := credentialRPID(c.Credential, views)
		if rpID != "" {
			credByRP[rpID] = struct{}{}
		}
		desc := passkeyDescriptor{
			RPID:              rpID,
			CredentialID:      c.ID,
			DisplayName:       c.DisplayName,
			CreatedAt:         c.CreatedAt,
			AuthenticatorType: classifyAuthenticator(c.Credential),
			Transports:        credentialTransports(c.Credential),
		}
		if c.LastUsed != nil {
			t := *c.LastUsed
			desc.LastUsed = &t
		}
		passkeys = append(passkeys, desc)
	}

	available := make([]availableRP, 0, len(configured))
	for _, rp := range configured {
		if _, has := credByRP[rp.RPID]; has {
			continue
		}
		available = append(available, availableRP{
			RPID:        rp.RPID,
			DisplayName: rpDisplayName(rp.RPID),
			Origin:      originByRP[rp.RPID],
		})
	}

	writeJSON(w, http.StatusOK, passkeysListResponse{
		CanonicalID:  au.UserID,
		Passkeys:     passkeys,
		AvailableRPs: available,
	})
}

// deleteUserPasskey removes one stored credential. Self-service:
// any authenticated user can delete their own passkeys (recovery
// after losing a device). Admins can delete anyone's, which lets
// them revoke a compromised credential out-of-band.
//
// The endpoint refuses to remove the caller's last credential —
// dropping yourself out of the system is a footgun that should
// require an explicit admin action. The check counts the caller's
// own credentials regardless of which user the URL targets, so a
// non-admin deleting their own keys can never lock themselves out.
func (h *Handler) deleteUserPasskey(w http.ResponseWriter, r *http.Request) {
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "auth required")
		return
	}
	credID := strings.TrimSpace(r.PathValue("credential_id"))
	if credID == "" {
		writeJSONError(w, http.StatusBadRequest, "credential_id required")
		return
	}
	// Look the credential up to verify ownership before deleting.
	// GetByRawID expects raw bytes; the URL carries the base64url
	// form so decode first.
	rawID, err := base64.RawURLEncoding.DecodeString(credID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid credential_id encoding")
		return
	}
	stored, err := h.credentials.GetByRawID(r.Context(), rawID)
	if err != nil || stored == nil {
		writeJSONError(w, http.StatusNotFound, "credential not found")
		return
	}
	// Owner check: non-admins can only delete their own credentials.
	// Admins skip this gate so they can revoke a compromised key for
	// any user.
	if stored.UserID != au.UserID && !au.IsAdmin() {
		writeJSONError(w, http.StatusForbidden, "not authorized")
		return
	}
	// Don't let a non-admin lock themselves out by deleting their
	// last credential. Admins bypass — they can recover by
	// registering a fresh key out-of-band.
	if !au.IsAdmin() {
		mine, err := h.credentials.ListByUser(r.Context(), au.UserID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(mine) <= 1 {
			writeJSONError(w, http.StatusConflict,
				"refusing to delete your last passkey — register another credential first or ask an admin to remove this one")
			return
		}
	}
	if err := h.credentials.Delete(r.Context(), stored.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"credential_id": stored.ID,
	})
}

// enrollBeginRequest carries the enrollment token and (eventually)
// any client-provided hints. Today the token alone is enough.
type enrollBeginRequest struct {
	Token string `json:"token"`
}

// enrollBegin validates the token, derives the target user, and
// starts a WebAuthn registration ceremony against the RP resolved
// from the inbound Host. The token is NOT consumed yet — consume
// fires only after a successful attestation in enrollFinish.
func (h *Handler) enrollBegin(w http.ResponseWriter, r *http.Request) {
	if h.enrollTokens == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "enrollment not configured")
		return
	}
	var body enrollBeginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tok, rp, err := h.resolveEnrollmentToken(r, body.Token)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Existing creds for this user — passed to BeginRegistration so
	// the browser's authenticator selection knows to exclude
	// credentials the user already has (excludeCredentials).
	existing, err := h.credentials.ListByUser(r.Context(), tok.CanonicalID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	user := &adminUser{
		id:          tok.CanonicalID,
		displayName: tok.CanonicalID,
		credentials: toCredentials(existing),
	}
	creation, sessionData, err := rp.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		}),
	)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin registration: "+err.Error())
		return
	}

	// Stash the ceremony state keyed by the token itself — the
	// finish call comes in with the same token, so we don't need a
	// separate cookie. The pendingStore is happy to hold arbitrary
	// session-data blobs; we just key it off the token.
	h.pending.put(tok.Token, *sessionData, PendingKindEnroll, tok.CanonicalID)
	writeJSON(w, http.StatusOK, creation)
}

// enrollFinishRequest carries the attestation response from the
// browser. Token re-supplied so the gateway can re-resolve the RP
// and consume the token after credential storage.
type enrollFinishRequest struct {
	Token       string          `json:"token"`
	Attestation json.RawMessage `json:"attestation"`
}

// enrollFinish validates the attestation, writes the credential,
// and consumes the token. On any failure the token stays valid —
// the user can retry without re-issuing.
func (h *Handler) enrollFinish(w http.ResponseWriter, r *http.Request) {
	if h.enrollTokens == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "enrollment not configured")
		return
	}
	var body enrollFinishRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tok, rp, err := h.resolveEnrollmentToken(r, body.Token)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	entry, ok := h.pending.take(tok.Token)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "no in-flight registration for this token")
		return
	}
	// Refuse cross-flow completion — only /enroll/begin entries
	// reach this handler, so the token gets Consume'd correctly.
	if entry.kind != PendingKindEnroll {
		writeJSONError(w, http.StatusBadRequest, "pending ceremony is not an enrollment")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(strings.NewReader(string(body.Attestation)))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse attestation: "+err.Error())
		return
	}
	existing, err := h.credentials.ListByUser(r.Context(), tok.CanonicalID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	user := &adminUser{
		id:          tok.CanonicalID,
		displayName: tok.CanonicalID,
		credentials: toCredentials(existing),
	}
	cred, err := rp.CreateCredential(user, entry.data, parsed)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "create credential: "+err.Error())
		return
	}

	// Label mirrors the standard registerFinish flow: "Passkey" for
	// platform authenticators, "Security key" for cross-platform,
	// "Key" otherwise. The profile UI keys off this prefix to render
	// the right chip alongside the credential row.
	prefix := "Key"
	switch cred.Authenticator.Attachment {
	case protocol.Platform:
		prefix = "Passkey"
	case protocol.CrossPlatform:
		prefix = "Security key"
	}
	label := fmt.Sprintf("%s — %s (%s)", prefix, tok.CanonicalID, time.Now().UTC().Format("2006-01-02"))
	if err := h.credentials.Insert(r.Context(), tok.CanonicalID, label, cred); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store credential: "+err.Error())
		return
	}
	if err := h.enrollTokens.Consume(r.Context(), tok.Token); err != nil {
		// Credential is already written; log but don't fail the
		// user — they'd retry, find the credential exists, get
		// confused. Token will be cleaned by sweep.
		// (no-op)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"rp_id":         tok.TargetRPID,
		"canonical_id":  tok.CanonicalID,
		"credential_id": encodeCredentialID(cred.ID),
	})
}

// resolveEnrollmentToken validates the token + inbound Host: the
// token must exist, be active, and its target_rp_id must match the
// RP resolved from the request's Host header. Returns the token
// row + the matched *webauthn.WebAuthn on success.
func (h *Handler) resolveEnrollmentToken(r *http.Request, raw string) (*EnrollmentToken, *webauthn.WebAuthn, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil, fmt.Errorf("token required")
	}
	tok, err := h.enrollTokens.Get(r.Context(), raw)
	if err != nil {
		return nil, nil, ErrEnrollmentTokenInvalid
	}
	rp, err := h.webauthnFor(r)
	if err != nil {
		return nil, nil, err
	}
	// Cross-check the inbound Host's RP matches what the token was
	// issued for. Refuse if not — opening the link on the wrong
	// domain must not succeed silently.
	hostRP, ok := h.rpidForHost(r.Host)
	if !ok || hostRP != tok.TargetRPID {
		return nil, nil, ErrEnrollmentTokenInvalid
	}
	return tok, rp, nil
}

// findRelyingParty looks up an RP by its rp_id. Returns the config
// block (so callers can read its Origins for URL construction) and
// a boolean.
func (h *Handler) findRelyingParty(rpID string) (*relyingPartyConfigView, bool) {
	for _, rp := range h.cfg.EffectiveRelyingParties() {
		if rp.RPID == rpID {
			view := relyingPartyConfigView{RPID: rp.RPID, Origins: append([]string(nil), rp.Origins...)}
			return &view, true
		}
	}
	return nil, false
}

// relyingPartyConfigView is the local view of an RP — same shape
// as config.RelyingPartyConfig but exposed only via a copy to keep
// the handler from holding a pointer into config.
type relyingPartyConfigView struct {
	RPID    string
	Origins []string
}

// rpidForHost returns the RPID associated with the inbound Host.
// Mirrors webauthnFor's lookup but returns the string ID for
// comparison against a token's target_rp_id.
func (h *Handler) rpidForHost(host string) (string, bool) {
	key := strings.ToLower(stripPort(host))
	for _, rp := range h.cfg.EffectiveRelyingParties() {
		for _, candidate := range rp.Hosts {
			if strings.EqualFold(candidate, key) {
				return rp.RPID, true
			}
		}
	}
	return "", false
}

// rpDisplayName humanises an RPID for the UI. Tailscale and the
// public hostname get friendly labels; everything else falls back
// to the raw RPID so an operator-added RP shows up sanely without
// needing a config edit.
func rpDisplayName(rpID string) string {
	switch {
	case rpID == "familiar.wiki":
		return "Public"
	case strings.Contains(rpID, ".ts.net"):
		return "Tailscale"
	case rpID == "localhost":
		return "Local"
	default:
		return rpID
	}
}

// credentialRPID infers the RPID a stored credential was registered
// against. go-webauthn doesn't expose the RPID directly on Credential
// (it lives in the authenticator data hash), so we match against the
// configured RPs by re-hashing each candidate. Returns "" when no
// configured RP matches — that credential is from an RP the operator
// has since removed.
func credentialRPID(cred webauthn.Credential, configured []relyingPartyConfigSlice) string {
	// go-webauthn doesn't surface RPIDHash on Credential in the
	// version we depend on, so we approximate: if there's only one
	// configured RP, attribute every credential to it; otherwise
	// leave empty and let the UI render "unknown RP" — the
	// per-credential-row rp_id is informational, not a security
	// boundary (the actual ceremony enforces RP match).
	if len(configured) == 1 {
		return configured[0].RPID
	}
	return ""
}

// relyingPartyConfigSlice is the same shape as relyingPartyConfigView
// but lets the credentialRPID helper take a slice without a separate
// import cycle through config — kept local to this file.
type relyingPartyConfigSlice = relyingPartyConfigView

// classifyAuthenticator returns "platform" / "cross-platform" /
// "unknown" for the stored Credential. Drives the per-passkey chip
// in the management UI so the user can tell a Touch-ID-style passkey
// from a YubiKey-style security key at a glance.
//
// Primary signal: Authenticator.Attachment, populated by the library
// from the attestation response. Secondary signal: a Transport list
// containing "internal" is the strongest hint of a platform
// authenticator that didn't declare its attachment (some older
// firmware behaves this way). Anything outside either signal stays
// "unknown" — better honest than wrong.
func classifyAuthenticator(cred webauthn.Credential) string {
	switch cred.Authenticator.Attachment {
	case protocol.Platform:
		return "platform"
	case protocol.CrossPlatform:
		return "cross-platform"
	}
	for _, t := range cred.Transport {
		switch t {
		case protocol.Internal:
			return "platform"
		case protocol.USB, protocol.NFC, protocol.BLE:
			return "cross-platform"
		}
	}
	return "unknown"
}

// credentialTransports flattens the typed AuthenticatorTransport
// slice into plain strings so the JSON shape stays stable across
// library version bumps and the UI can render chips without
// knowing the library's type names.
func credentialTransports(cred webauthn.Credential) []string {
	if len(cred.Transport) == 0 {
		return nil
	}
	out := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		s := string(t)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// The configured slice passed in by listUserPasskeys is built from
// EffectiveRelyingParties() and converted to this local view shape
// inline; that keeps the helper signature decoupled from
// config.RelyingPartyConfig.
