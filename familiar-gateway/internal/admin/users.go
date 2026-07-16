package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/identity"
)

// UserManager is the subset of *identity.Resolver the admin console
// uses. Kept narrow so tests can drop in a fake and so the admin
// package doesn't depend on internals it doesn't need.
type UserManager interface {
	ListUsers(ctx context.Context, statuses []identity.UserStatus) ([]identity.User, error)
	GetUser(ctx context.Context, id string) (*identity.User, error)
	ListIdentitiesForUser(ctx context.Context, userID string) ([]identity.IdentityLink, error)
	SetUserStatus(ctx context.Context, userID string, status identity.UserStatus, approver string) error
	LinkIdentity(ctx context.Context, userID, platform, platformID, displayName string) error
	UnlinkIdentity(ctx context.Context, platform, platformID string) error
	// Phase-2 additions — role plumbing and the last-admin guardrail.
	SetUserRole(ctx context.Context, userID, role string) error
	SetUserDisplayName(ctx context.Context, userID, displayName string) error
	CountAdmins(ctx context.Context) (int, error)
	// Phase-4 additions — email-keyed registration and first-run
	// owner bootstrap. GetByEmail returns (nil, nil) for "unknown
	// email" vs (nil, err) for a real DB problem so registerBegin
	// can surface a clean 403 without misattributing infra failures.
	GetByEmail(ctx context.Context, email string) (*identity.User, error)
	CreateFirstRun(ctx context.Context, id, displayName, email string) error
	// CreateInvited adds an admin-initiated invite row. role is
	// "user" or "admin"; status is forced to approved server-side
	// so the invitee can register a passkey and sign in without a
	// second admin click.
	CreateInvited(ctx context.Context, id, displayName, email, role string) error
	// InviteUser is the higher-level admin invite path. Derives a
	// canonical id from displayName (or the email local part when
	// blank), dedupes, inserts the row, and returns the freshly-
	// created user. Returns ErrDuplicateLink when email already
	// belongs to another row.
	InviteUser(ctx context.Context, displayName, email, role string) (*identity.User, error)
	// Powers the in-app user picker (e.g. the book-members modal).
	// Empty query returns empty result so the endpoint never lists
	// every user accidentally.
	SearchUsers(ctx context.Context, query string, limit int) ([]identity.User, error)
}

// AttachUserManager wires the user management store into the handler.
// Must be called before Mux() for the /admin/api/users endpoints to
// respond with live data. Nil is tolerated — the endpoints reply with
// "not configured" 503s so the frontend can render a disabled state.
func (h *Handler) AttachUserManager(um UserManager) {
	h.users = um
}

// userDTO is the wire shape for a single user row. Timestamps are
// formatted as RFC3339 strings so the JS client doesn't have to parse
// Go time literals.
type userDTO struct {
	ID              string            `json:"id"`
	DisplayName     string            `json:"display_name"`
	Status          string            `json:"status"`
	Role            string            `json:"role"`
	Email           string            `json:"email,omitempty"`
	BootstrapSource string            `json:"bootstrap_source,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	ApprovedAt      *time.Time        `json:"approved_at,omitempty"`
	ApprovedBy      string            `json:"approved_by,omitempty"`
	Identities      []identityLinkDTO `json:"identities,omitempty"`
	// PasskeyCount is how many WebAuthn credentials this user has
	// registered. 0 means invited-but-not-yet-enrolled — the admin UI
	// flags those and offers a one-click enrollment link.
	PasskeyCount int `json:"passkey_count"`
}

type identityLinkDTO struct {
	Platform    string    `json:"platform"`
	PlatformID  string    `json:"platform_id"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

func toUserDTO(u identity.User, links []identity.IdentityLink) userDTO {
	out := userDTO{
		ID:          u.ID,
		DisplayName: u.DisplayName,
		Status:      string(u.Status),
		Role:        u.Role,
		CreatedAt:   u.CreatedAt,
		ApprovedAt:  u.ApprovedAt,
		ApprovedBy:  u.ApprovedBy,
	}
	if u.Email != nil {
		out.Email = *u.Email
	}
	if u.BootstrapSource != nil {
		out.BootstrapSource = *u.BootstrapSource
	}
	for _, l := range links {
		out.Identities = append(out.Identities, identityLinkDTO{
			Platform:    l.Platform,
			PlatformID:  l.PlatformID,
			DisplayName: l.DisplayName,
			CreatedAt:   l.CreatedAt,
		})
	}
	return out
}

// listUsers serves GET /admin/api/users. Optional ?status=pending|...
// filter selects a single status; omit for all users. The response
// includes linked identities for each user so the frontend doesn't
// need a second round-trip per row.
// lookupUserDTO is the trimmed wire shape used by the picker
// endpoint — only the fields a typeahead row actually renders.
// Crucially excludes status / role / bootstrap_source so the
// endpoint can be exposed to any authenticated user (e.g. a book
// owner inviting members) without leaking admin-only fields.
type lookupUserDTO struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
}

// lookupUsers serves GET /console/api/users/lookup?q=…&limit=…
// for the in-app user picker. Available to any authenticated
// user — there's no admin gate. Returns at most `limit` (default
// 10, max 25) trimmed DTOs whose id / display_name / email start
// with `q`. Empty `q` returns an empty list rather than
// everything; the endpoint isn't a directory dump tool.
func (h *Handler) lookupUsers(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user lookup not configured")
		return
	}
	q := r.URL.Query().Get("q")
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	users, err := h.users.SearchUsers(r.Context(), q, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]lookupUserDTO, 0, len(users))
	for _, u := range users {
		out := lookupUserDTO{ID: u.ID, DisplayName: u.DisplayName}
		if u.Email != nil {
			out.Email = *u.Email
		}
		items = append(items, out)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user management not configured")
		return
	}
	ctx := r.Context()
	var filter []identity.UserStatus
	if s := r.URL.Query().Get("status"); s != "" {
		filter = []identity.UserStatus{identity.UserStatus(s)}
	}
	users, err := h.users.ListUsers(ctx, filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Passkey counts per user so the UI can flag invited-but-not-yet-
	// enrolled accounts. One grouped query; nil store → all zero.
	var counts map[string]int
	if h.credentials != nil {
		counts, err = h.credentials.CountsByUser(ctx)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	items := make([]userDTO, 0, len(users))
	for _, u := range users {
		links, err := h.users.ListIdentitiesForUser(ctx, u.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		dto := toUserDTO(u, links)
		dto.PasskeyCount = counts[u.ID]
		items = append(items, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getUser serves GET /admin/api/users/{id}.
func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user management not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	u, err := h.users.GetUser(r.Context(), id)
	if errors.Is(err, identity.ErrUserNotFound) {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	links, err := h.users.ListIdentitiesForUser(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dto := toUserDTO(*u, links)
	if h.credentials != nil {
		creds, cerr := h.credentials.ListByUser(r.Context(), id)
		if cerr != nil {
			writeJSONError(w, http.StatusInternalServerError, cerr.Error())
			return
		}
		dto.PasskeyCount = len(creds)
	}
	writeJSON(w, http.StatusOK, dto)
}

// inviteUserRequest is the body for POST /console/api/users — the
// admin-only invite path that lets an operator add a teammate by
// email without going through Slack auto-provision.
type inviteUserRequest struct {
	Email        string `json:"email"`
	DisplayName  string `json:"display_name,omitempty"`
	Role         string `json:"role,omitempty"` // "user" (default) or "admin"
	GenerateLink bool   `json:"generate_enrollment_link,omitempty"`
	TargetRPID   string `json:"target_rp_id,omitempty"` // required when generate_enrollment_link
}

// inviteUserResponse carries the freshly-created user row plus an
// optional enrollment-token link the admin can share with the
// invitee for first-time passkey registration.
type inviteUserResponse struct {
	User          userDTO    `json:"user"`
	EnrollmentURL string     `json:"enrollment_url,omitempty"`
	EnrollmentExp *time.Time `json:"enrollment_expires_at,omitempty"`
}

// inviteUser serves POST /console/api/users — admin-only (enforced
// at registration). Creates a new user row by email with the
// requested role (default "user") and optionally mints a cross-
// domain enrollment token so the admin can hand the invitee a
// direct passkey-registration link.
//
// PUBLIC-PROXY-MIGRATION's invite path was Slack-only; this
// endpoint is the email-only equivalent for deployments where
// Slack isn't wired or for inviting someone who shouldn't have
// Slack access.
func (h *Handler) inviteUser(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user management not configured")
		return
	}
	var body inviteUserRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))
	if email == "" {
		writeJSONError(w, http.StatusBadRequest, "email required")
		return
	}
	role := strings.TrimSpace(body.Role)
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "admin" {
		writeJSONError(w, http.StatusBadRequest, "role must be 'user' or 'admin'")
		return
	}

	u, err := h.users.InviteUser(r.Context(), strings.TrimSpace(body.DisplayName), email, role)
	if errors.Is(err, identity.ErrDuplicateLink) {
		writeJSONError(w, http.StatusConflict, "a user with that email already exists")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	links, _ := h.users.ListIdentitiesForUser(r.Context(), u.ID)
	resp := inviteUserResponse{User: toUserDTO(*u, links)}

	// Optional one-shot enrollment-token mint. The admin picks the
	// target RP via target_rp_id (one of the configured RPs); on
	// success the response carries the user-facing URL.
	if body.GenerateLink {
		if h.enrollTokens == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "enrollment not configured")
			return
		}
		targetRP := strings.TrimSpace(body.TargetRPID)
		if targetRP == "" {
			writeJSONError(w, http.StatusBadRequest, "target_rp_id required when generate_enrollment_link is true")
			return
		}
		rp, ok := h.findRelyingParty(targetRP)
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "unknown target_rp_id "+targetRP)
			return
		}
		au, _ := AuthUserFrom(r.Context())
		issuer := u.ID
		if au.UserID != "" {
			issuer = au.UserID
		}
		tok, err := h.enrollTokens.Issue(r.Context(), u.ID, rp.RPID, issuer)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(rp.Origins) > 0 {
			resp.EnrollmentURL = strings.TrimRight(rp.Origins[0], "/") + "/enroll?token=" + tok.Token
		}
		exp := tok.ExpiresAt
		resp.EnrollmentExp = &exp
	}

	writeJSON(w, http.StatusCreated, resp)
}

// patchOwnProfile serves PATCH /console/api/profile/me — the
// self-service path for a signed-in user to update their own
// display_name. Today the body only carries display_name; future
// preference fields can join the same endpoint without a route
// proliferation. Admin-side bulk edits stay on
// PATCH /console/api/users/{id} which can touch role + status too.
func (h *Handler) patchOwnProfile(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user management not configured")
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "auth required")
		return
	}
	var body struct {
		DisplayName *string `json:"display_name,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if body.DisplayName == nil {
		writeJSONError(w, http.StatusBadRequest, "display_name required")
		return
	}
	dn := strings.TrimSpace(*body.DisplayName)
	if dn == "" {
		writeJSONError(w, http.StatusBadRequest, "display_name cannot be empty")
		return
	}
	if len(dn) > 80 {
		writeJSONError(w, http.StatusBadRequest, "display_name too long (max 80 chars)")
		return
	}
	if err := h.users.SetUserDisplayName(r.Context(), au.UserID, dn); err != nil {
		if errors.Is(err, identity.ErrUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	u, err := h.users.GetUser(r.Context(), au.UserID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	links, _ := h.users.ListIdentitiesForUser(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, toUserDTO(*u, links))
}

// patchUser serves PATCH /admin/api/users/{id}. Partial update of
// role, display_name, and/or status. Admin-only (enforced by the
// mux's adminOnly wrap). Guards against demoting the last admin —
// returns 409 Conflict rather than silently locking everyone out.
//
// Body (all fields optional):
//
//	{
//	  "role":         "admin" | "user",
//	  "display_name": "New Name",
//	  "status":       "approved" | "disabled"
//	}
//
// Status transitions to "pending" or "denied" are deliberately NOT
// supported here — those live on the existing status-change endpoint
// (POST /admin/api/users/{id}/status) which records an approver. The
// PATCH endpoint is for operator housekeeping on already-known users.
func (h *Handler) patchUser(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user management not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}

	var body struct {
		Role        *string `json:"role,omitempty"`
		DisplayName *string `json:"display_name,omitempty"`
		Status      *string `json:"status,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if body.Role == nil && body.DisplayName == nil && body.Status == nil {
		writeJSONError(w, http.StatusBadRequest, "at least one of role/display_name/status required")
		return
	}
	if body.Role != nil && *body.Role != "admin" && *body.Role != "user" {
		writeJSONError(w, http.StatusBadRequest, "role must be 'admin' or 'user'")
		return
	}
	if body.Status != nil {
		switch *body.Status {
		case string(identity.StatusApproved), string(identity.StatusDisabled):
		default:
			writeJSONError(w, http.StatusBadRequest,
				"status must be 'approved' or 'disabled' (use /status endpoint for pending/denied)")
			return
		}
	}

	ctx := r.Context()

	// Last-admin guard: demoting from admin OR disabling an admin
	// can't leave the system with zero admins. Check first so we
	// never do a partial update and *then* realize we have to roll
	// back.
	if body.Role != nil && *body.Role == "user" {
		if err := h.ensureNotLastAdmin(ctx, id); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if body.Status != nil && *body.Status == string(identity.StatusDisabled) {
		// If disabling an admin, same guardrail applies.
		u, err := h.users.GetUser(ctx, id)
		if err == nil && u != nil && u.Role == "admin" {
			if err := h.ensureNotLastAdmin(ctx, id); err != nil {
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			}
		}
	}

	// Apply updates. Fails fast on the first error — callers get a
	// partial state when one update in a multi-field body breaks,
	// but that's preferable to silent rollback complexity for a
	// low-volume admin endpoint.
	approver, _ := ctx.Value(ContextKeyUserID).(string)
	if body.DisplayName != nil {
		if err := h.users.SetUserDisplayName(ctx, id, *body.DisplayName); err != nil {
			if errors.Is(err, identity.ErrUserNotFound) {
				writeJSONError(w, http.StatusNotFound, "user not found")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.Role != nil {
		if err := h.users.SetUserRole(ctx, id, *body.Role); err != nil {
			if errors.Is(err, identity.ErrUserNotFound) {
				writeJSONError(w, http.StatusNotFound, "user not found")
				return
			}
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Status != nil {
		if err := h.users.SetUserStatus(ctx, id, identity.UserStatus(*body.Status), approver); err != nil {
			if errors.Is(err, identity.ErrUserNotFound) {
				writeJSONError(w, http.StatusNotFound, "user not found")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.revokeSessionsIfNotApproved(ctx, id, *body.Status)
	}

	// Echo the updated row so the frontend can refresh without a
	// separate GET round-trip.
	u, err := h.users.GetUser(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	links, _ := h.users.ListIdentitiesForUser(ctx, id)
	writeJSON(w, http.StatusOK, toUserDTO(*u, links))
}

// ensureNotLastAdmin refuses an operation when the target user is
// the only admin. Called by patchUser before demote / disable.
// Returns nil when the operation is safe (target isn't admin, or
// other admins exist).
func (h *Handler) ensureNotLastAdmin(ctx context.Context, targetID string) error {
	target, err := h.users.GetUser(ctx, targetID)
	if err != nil {
		// Non-existent target isn't a last-admin problem — let the
		// downstream call report the real error.
		return nil
	}
	if target.Role != "admin" {
		return nil
	}
	n, err := h.users.CountAdmins(ctx)
	if err != nil {
		return err
	}
	if n <= 1 {
		return errors.New("cannot demote or disable the last admin")
	}
	return nil
}

// setUserStatus serves POST /admin/api/users/{id}/status. The body is
// {"status": "approved"|"denied"|"disabled"|"pending"}. The admin
// whose session cookie authenticated the request is recorded as the
// approver.
func (h *Handler) setUserStatus(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user management not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	approver, _ := r.Context().Value(ContextKeyUserID).(string)
	err := h.users.SetUserStatus(r.Context(), id, identity.UserStatus(body.Status), approver)
	if errors.Is(err, identity.ErrUserNotFound) {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Revoke outstanding sessions when the new status isn't
	// approved — without this hook a disabled user keeps a valid
	// cookie until natural TTL expiry, and /api/chat (which
	// doesn't run authRequired) would keep accepting it.
	h.revokeSessionsIfNotApproved(r.Context(), id, body.Status)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "id": id, "new_status": body.Status})
}

// revokeSessionsIfNotApproved deletes every active session owned by
// userID when the new status isn't "approved". Logs but doesn't
// fail-the-request on DB error — the status flip already committed
// and the worst-case is that sessions linger until their TTL elapses.
func (h *Handler) revokeSessionsIfNotApproved(ctx context.Context, userID, newStatus string) {
	if newStatus == string(identity.StatusApproved) {
		return
	}
	if h.sessions == nil || userID == "" {
		return
	}
	if err := h.sessions.DeleteByUser(ctx, userID); err != nil {
		log.Printf("[admin] warning: revoke sessions for %s: %v", userID, err)
	}
}

// linkIdentity serves POST /admin/api/users/{id}/identities. Body:
// {"platform": "openai", "platform_id": "alice", "display_name": "Alice"}.
// Used to attach an OpenAI / Open WebUI handle to an already-approved
// user after the Slack-originated request flow.
func (h *Handler) linkIdentity(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user management not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	var body struct {
		Platform    string `json:"platform"`
		PlatformID  string `json:"platform_id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if body.Platform == "" || body.PlatformID == "" {
		writeJSONError(w, http.StatusBadRequest, "platform and platform_id required")
		return
	}
	err := h.users.LinkIdentity(r.Context(), id, body.Platform, body.PlatformID, body.DisplayName)
	if errors.Is(err, identity.ErrUserNotFound) {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if errors.Is(err, identity.ErrDuplicateLink) {
		writeJSONError(w, http.StatusConflict, "that platform identity is already linked to another user")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// unlinkIdentity serves DELETE /admin/api/users/{id}/identities/{platform}/{platform_id}.
// Strips a single platform link from a user. The user itself is
// untouched; deleting the user is a status transition to "disabled".
func (h *Handler) unlinkIdentity(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user management not configured")
		return
	}
	platform := r.PathValue("platform")
	platformID := r.PathValue("platform_id")
	if platform == "" || platformID == "" {
		writeJSONError(w, http.StatusBadRequest, "platform and platform_id required")
		return
	}
	if err := h.users.UnlinkIdentity(r.Context(), platform, platformID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
