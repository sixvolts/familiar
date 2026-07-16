package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/familiar/gateway/internal/identity"
)

// adminUserScope resolves which user's data the caller may act on:
// their own id, unless they're an admin who supplied a non-empty
// ?user_id=, in which case that (trimmed) target wins. Non-admins
// can't escape their own id — a spoofed ?user_id= is ignored.
//
// This is the single source of truth for the standard admin-override
// rule that the per-surface scope helpers (profileScopeFor,
// dashboardScopeFor, graphScopeFor, scopeForConversations, …) delegate
// to, so the rule — including the trim — can't drift between handlers.
//
// NOTE: a few surfaces intentionally differ and do NOT use this:
// chat_sessions treats an admin's missing ?user_id= as "all sessions"
// (no filter), not "own"; scopeForWiki layers book-membership on top.
func adminUserScope(r *http.Request, au AuthUser) string {
	if au.IsAdmin() {
		if q := strings.TrimSpace(r.URL.Query().Get("user_id")); q != "" {
			return q
		}
	}
	return au.UserID
}

// AuthUser is the per-request snapshot the middleware leaves in the
// request context: canonical user ID, role (admin|user), email when
// set, and the principal envelope for shard sessions. Handlers pull
// this via AuthUserFrom instead of reading the raw session cookie
// themselves. Populated by authRequired; consumed by adminOnly and
// by per-handler ownership scoping (memory endpoints enforce
// "role=user can only see their own", books endpoints intersect
// against Permissions.Books for shard sessions, etc.).
//
// PrincipalType / PrincipalID / ShardID / Permissions are
// SHARD-AUTH-SPEC additions. For a USER session:
//   - PrincipalType = "user"
//   - PrincipalID = UserID
//   - ShardID = ""
//   - Permissions = nil (interpret as "everything the user can see")
//
// For a SHARD session:
//   - PrincipalType = "shard"
//   - PrincipalID = shard.ID
//   - ShardID = shard.ID
//   - UserID = shard.OwnerID (so existing UserID-keyed code resolves
//     to the canonical owner for ownership scoping)
//   - Permissions = the shard's intersected envelope
type AuthUser struct {
	UserID        string
	Role          string
	Email         string
	PrincipalType string
	PrincipalID   string
	ShardID       string
	Permissions   *SessionPermissions
}

// SessionPermissions captures the surface-by-surface kill switches
// for a shard session. Nil on AuthUser means "all permissions"
// (i.e. a regular user session). A non-nil envelope on a user
// session is meaningless and never produced.
//
// Books being nil means "all of the owner's memberships". A non-
// nil empty slice means "no books". Same convention for Panels —
// nil = all, empty = none — so a headless kiosk can disable every
// surface explicitly without colliding with the "unset = inherit"
// case.
//
// "scheduled" is deliberately NOT a grantable panel: every
// /console/api/actions route refuses shard sessions outright
// (actionScope), so a kiosk can never rewire its owner's robots —
// granting the panel would only render a UI whose every call 403s.
type SessionPermissions struct {
	Books     []string // nil = all owner memberships; non-nil = explicit set
	Panels    []string // nil = all panels; non-nil = explicit set
	CanChat   bool
	CanInvoke bool
	CanAdmin  bool
}

// IsAdmin is a tiny helper so per-handler scoping stays compact at
// call sites. Returns true for role="admin" only; empty-string role
// (test fallback when the identity store isn't wired) is treated as
// non-admin so tests must explicitly stand up an admin user.
//
// For a SHARD session the role check additionally requires that
// the session's permission envelope grants admin operations
// (Permissions.CanAdmin). A shard owned by an admin user does NOT
// automatically inherit admin powers — the shard config must opt
// in explicitly. That keeps "log in as kiosk shard" from elevating
// the iPad to admin even though the owner is one.
func (a AuthUser) IsAdmin() bool {
	if a.Role != "admin" {
		return false
	}
	if a.Permissions != nil && !a.Permissions.CanAdmin {
		return false
	}
	return true
}

// IsShardSession reports whether this request was authenticated
// via a shard passkey rather than a user passkey.
func (a AuthUser) IsShardSession() bool { return a.PrincipalType == PrincipalTypeShard }

// CanAccessBook returns true when this session is allowed to
// address the given book ID. User sessions return true
// unconditionally — book-level membership is enforced separately
// by WikiStore. Shard sessions consult Permissions.Books: nil
// means "all owner memberships" (so still defer to membership);
// a non-nil set means "only these book IDs", and the caller still
// has to verify the owner is a member.
func (a AuthUser) CanAccessBook(bookID string) bool {
	if a.Permissions == nil || a.Permissions.Books == nil {
		return true
	}
	for _, id := range a.Permissions.Books {
		if id == bookID {
			return true
		}
	}
	return false
}

// CanAccessPanel returns true when the named panel is visible to
// this session. Panels are coarse — "books", "notes", "chat",
// "memory", "shards", "dashboard". Empty/nil Panels means all
// panels.
func (a AuthUser) CanAccessPanel(name string) bool {
	if a.Permissions == nil || a.Permissions.Panels == nil {
		return true
	}
	for _, p := range a.Permissions.Panels {
		if p == name {
			return true
		}
	}
	return false
}

// ctxAuthUserKey is the request-context key authRequired uses to
// hand AuthUser to downstream handlers. Distinct type (contextKey)
// guards against collisions with other packages.
const ctxAuthUserKey contextKey = "familiar.admin.authuser"

// AuthUserFrom reads the AuthUser left in ctx by authRequired. The
// ok return is false when the middleware never ran — treat that as
// "deny" in handlers (the mux wiring should prevent this, but
// defence in depth keeps typos from silently opening routes).
func AuthUserFrom(ctx context.Context) (AuthUser, bool) {
	u, ok := ctx.Value(ctxAuthUserKey).(AuthUser)
	return u, ok
}

// authRequired is the Phase-2 replacement for requireAuth. In addition
// to validating the session cookie it loads the full User record
// (role + email + status), refuses non-approved users with 403, and
// stashes an AuthUser in the request context so downstream handlers
// can scope by role without a second DB round-trip.
//
// Also populates the legacy ContextKeyUserID so handlers that still
// read the raw string keep working — callers migrate to AuthUserFrom
// at their own pace.
//
// h.users being nil (test harness) falls back to session-only auth
// with empty role/email; adminOnly will then reject on the role
// check, which is the conservative default for tests.
func (h *Handler) authRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := h.authenticatedSession(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		// Default the session to user-shaped — pre-migration rows
		// might not have principal_type populated yet (the column
		// has DEFAULT 'user' but defensive coding here keeps the
		// middleware safe).
		principalType := sess.PrincipalType
		if principalType == "" {
			principalType = PrincipalTypeUser
		}

		au := AuthUser{
			UserID:        sess.UserID,
			PrincipalType: principalType,
			PrincipalID:   sess.PrincipalID,
		}
		if au.PrincipalID == "" {
			au.PrincipalID = sess.UserID
		}

		// Hydrate user role + email — same path as before. Applies
		// for BOTH user and shard sessions (a shard session's
		// "user" is the canonical owner, who still needs to be
		// approved before any of their shards can authenticate).
		if h.users != nil {
			u, err := h.users.GetUser(r.Context(), sess.UserID)
			if err != nil {
				if errors.Is(err, identity.ErrUserNotFound) {
					writeJSONError(w, http.StatusForbidden, "user not found")
					return
				}
				writeJSONError(w, http.StatusInternalServerError, "load user: "+err.Error())
				return
			}
			if u.Status != identity.StatusApproved {
				writeJSONError(w, http.StatusForbidden, "user not approved")
				return
			}
			au.Role = u.Role
			if u.Email != nil {
				au.Email = *u.Email
			}
		}

		// Shard-session branch: load the shard, verify it's still
		// active + console-enabled, build the per-request
		// permission envelope. Disabling a shard or flipping
		// console_access off invalidates all existing sessions
		// within one request cycle (per SHARD-AUTH-SPEC §
		// "Revocation").
		if au.PrincipalType == PrincipalTypeShard {
			if h.shards == nil {
				writeJSONError(w, http.StatusServiceUnavailable, "shard sessions not configured")
				return
			}
			au.ShardID = au.PrincipalID
			perms, err := h.loadShardPermissions(r.Context(), au.ShardID, sess.UserID)
			if err != nil {
				if errors.Is(err, errShardSessionRevoked) {
					writeJSONError(w, http.StatusUnauthorized, "shard session revoked")
					return
				}
				writeJSONError(w, http.StatusInternalServerError, "load shard: "+err.Error())
				return
			}
			au.Permissions = perms
		}

		ctx := context.WithValue(r.Context(), ContextKeyUserID, sess.UserID)
		ctx = context.WithValue(ctx, ctxAuthUserKey, au)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// errShardSessionRevoked is returned by loadShardPermissions when
// the shard has been disabled OR had console_access flipped off
// since the session was minted. authRequired surfaces it as 401
// so the frontend boots back to the login screen.
var errShardSessionRevoked = errors.New("shard session revoked")

// loadShardPermissions returns the per-request permission envelope
// for a shard session. Validates that the shard still exists, is
// still active, still has console_access=true, and that ownerID
// in the session matches the shard's recorded owner — a mismatch
// would mean the shard was reassigned (or worse) since the session
// was minted, and we don't want to honor it.
//
// Book access is intersected against the owner's actual book
// memberships at request time so revoking a book membership
// immediately constrains the shard.
func (h *Handler) loadShardPermissions(ctx context.Context, shardID, sessionUserID string) (*SessionPermissions, error) {
	if h.shards == nil {
		return nil, errors.New("shard store not configured")
	}
	sh, err := h.shards.GetShard(ctx, shardID)
	if err != nil {
		return nil, err
	}
	if !sh.Active() || !sh.ConsoleAccess {
		return nil, errShardSessionRevoked
	}
	if sh.OwnerID != sessionUserID {
		// Session's user_id no longer matches the shard's owner.
		// Out-of-band reassignment isn't supported in Phase 1, so
		// any mismatch is a revoke signal.
		return nil, errShardSessionRevoked
	}

	perms := &SessionPermissions{
		CanChat:   sh.ChatEnabled,
		CanInvoke: sh.APIEnabled,
		CanAdmin:  false, // shards never inherit admin powers in Phase 1
	}
	// Panels: nil = inherit, non-nil = explicit set.
	if len(sh.ConsolePanels) > 0 {
		perms.Panels = append([]string(nil), sh.ConsolePanels...)
	}

	// Book intersection. Empty BookAccess in the shard config
	// means "all of the owner's memberships" — leave Permissions
	// .Books nil so CanAccessBook short-circuits to true.
	if len(sh.BookAccess) > 0 {
		// Pull the owner's actual memberships so we can drop any
		// books the owner can no longer see. This runs once per
		// request — Phase 2 may cache per-session.
		var ownerBooks map[string]bool
		if h.wiki != nil {
			rows, err := h.wiki.ListBooksWithPersonal(ctx, sh.OwnerID, true)
			if err != nil {
				return nil, fmt.Errorf("load owner memberships: %w", err)
			}
			ownerBooks = make(map[string]bool, len(rows))
			for _, b := range rows {
				ownerBooks[b.ID] = true
			}
		}
		filtered := make([]string, 0, len(sh.BookAccess))
		for _, id := range sh.BookAccess {
			// If the wiki store isn't wired, we can't intersect —
			// trust the configured set as-is. Per-endpoint book
			// scope checks will still 404 on books the owner
			// isn't a member of.
			if ownerBooks == nil || ownerBooks[id] {
				filtered = append(filtered, id)
			}
		}
		perms.Books = filtered
	}
	return perms, nil
}

// requirePanel returns a middleware that 403s when the current
// session's permission envelope hides the named panel. User sessions
// (Permissions == nil) and shards with an inherit-everything envelope
// (Permissions.Panels == nil) pass through unchanged. Wrap INSIDE
// authRequired — it relies on AuthUserFrom.
func (h *Handler) requirePanel(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			au, ok := AuthUserFrom(r.Context())
			if !ok {
				writeJSONError(w, http.StatusForbidden, "missing authenticated user")
				return
			}
			if !au.CanAccessPanel(name) {
				writeJSONError(w, http.StatusForbidden, "panel not accessible to this session")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireChat gates chat-write endpoints. Mirrors CanInvoke for
// the API-invocation surfaces. User sessions short-circuit true.
func (h *Handler) requireChat(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		au, ok := AuthUserFrom(r.Context())
		if !ok {
			writeJSONError(w, http.StatusForbidden, "missing authenticated user")
			return
		}
		if au.IsShardSession() && au.Permissions != nil && !au.Permissions.CanChat {
			writeJSONError(w, http.StatusForbidden, "chat disabled for this session")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminOnly rejects non-admin roles with 403. Must be wrapped
// INSIDE authRequired so AuthUser is populated — using it on a route
// that isn't also authRequired-wrapped is a programming error and
// will return 403 because AuthUser won't be in context.
func (h *Handler) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		au, ok := AuthUserFrom(r.Context())
		if !ok || !au.IsAdmin() {
			writeJSONError(w, http.StatusForbidden, "admin role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
