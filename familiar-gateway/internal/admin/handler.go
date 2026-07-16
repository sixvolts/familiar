// Package admin implements the WebAuthn-gated console backend:
// credential storage, session cookies, and the /console/api/* HTTP
// handlers (renamed from /admin/api/* in FAMILIAR-CONSOLE-SPEC
// Phase B; legacy /admin paths still 301 to /console). The package
// depends on the shared *db.Pool for persistence and exposes a
// single http.Handler via Handler.Mux() that the gateway's HTTP
// server mounts.
//
// Package name remains `admin` because internal/admin owns the
// WebAuthn ceremony, credential rotation, and session-cookie code —
// all of which are still admin-grade machinery regardless of which
// role uses the resulting console. The path/UI rename is surface
// only; the role-based authz inside is unchanged.
package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"sync"

	"github.com/familiar/gateway/internal/actions"
	"github.com/familiar/gateway/internal/backfill"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/ctxbuild"
	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/maintenance"
	"github.com/familiar/gateway/internal/media"
	"github.com/familiar/gateway/internal/pageevents"
	"github.com/familiar/gateway/internal/push"
	"github.com/familiar/gateway/internal/skillpkg"
	"github.com/familiar/gateway/internal/skills/weather"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// contextKey is unexported so other packages can't accidentally poke
// values into the request context under our keys.
type contextKey string

const (
	// ContextKeyUserID is the request-context key the auth middleware
	// uses to hand the authenticated canonical user ID to downstream
	// handlers. Read with r.Context().Value(admin.ContextKeyUserID).
	ContextKeyUserID contextKey = "admin_user_id"

	sessionCookieName = "familiar_admin_session"
	pendingCookieName = "familiar_admin_pending"
)

// Handler owns the admin subsystem: the WebAuthn library instance,
// both stores, the pending-ceremony cache, and the HTTP mux. One
// instance is created at startup and its Mux is mounted under /admin/
// by the HTTP adapter.
type Handler struct {
	cfg config.AdminConfig
	// rps maps a lowercase, port-stripped inbound Host header to its
	// WebAuthn relying party. Multi-RP support landed with
	// PUBLIC-PROXY-MIGRATION so the gateway can sit behind both a
	// public hostname (host-a.familiar.wiki) and the Tailscale-direct
	// hostname without one set of passkeys invalidating the other.
	rps map[string]*webauthn.WebAuthn
	// cookieSecure stamps Secure: true on every session / pending
	// cookie when set. Production (both paths HTTPS-fronted) flips
	// it on; dev-on-localhost leaves it off so cookies survive the
	// non-TLS round-trip.
	cookieSecure bool
	credentials  *CredentialStore
	sessions     *SessionStore
	pending      *pendingStore
	// enrollTokens persists short-lived cross-domain passkey
	// enrollment tokens (CROSS-DOMAIN-ENROLLMENT.md). Used by the
	// /console/api/auth/enrollment-token + /console/api/auth/enroll/*
	// endpoints; nil when the admin subsystem isn't fully wired.
	enrollTokens       *EnrollmentTokenStore
	sessionMaxAge      time.Duration
	memoryBrowser      MemoryBrowser                                             // optional; wired via AttachMemoryBrowser
	memoryEmbed        func(ctx context.Context, text string) ([]float32, error) // optional; re-embeds PATCHed content
	users              UserManager                                               // optional; wired via AttachUserManager
	status             StatusProvider                                            // optional; wired via AttachStatusProvider
	shards             ShardStore                                                // optional; wired via AttachShardStore (Phase 1 shards)
	skills             SkillCatalog                                              // optional; wired via AttachSkillCatalog
	graph              GraphStore                                                // optional; wired via AttachGraphStore (Phase D memory graph)
	profiles           ProfileStore                                              // optional; wired via AttachProfileStore (user profile panel)
	chatSessions       ChatSessionLister                                         // optional; wired via AttachChatSessionLister (sessions panel)
	conversations      *ConversationStore                                        // optional; wired via AttachConversationStore (workspace chat)
	notes              *NotesStore                                               // optional; wired via AttachNotesStore (workspace notes)
	wiki               *WikiStore                                                // optional; wired via AttachWikiStore (books + wiki pages)
	shardPasskeys      *ShardPasskeyStore                                        // optional; wired via AttachShardPasskeyStore (SHARD-AUTH-SPEC)
	models             ModelCatalog                                              // optional; wired via AttachModelCatalog (MODEL-SELECTOR)
	weather            *weather.Skill                                            // optional; wired via AttachWeather (Home weather widget)
	pageEvents         *pageevents.Bus                                           // optional; wired via AttachPageEvents (SSE push)
	actions            *actions.Store                                            // optional; wired via AttachActions (SCHEDULED-ACTIONS-SPEC)
	actionsRunner      *actions.Runner                                           // optional; wired via AttachActions
	skillPkgs          *skillpkg.Store                                           // optional; wired via AttachSkillPackages (SKILL-PACKAGES-SPEC)
	skillPkgKnownTools map[string]bool
	media              *media.Store            // optional; wired via AttachMedia (MEDIA-DIAGRAMS)
	researchRuns       *ResearchRunStore       // optional; wired via AttachResearchRuns (§6.7 progress card)
	researchCanceller  func(runID string) bool // optional; cuts an active run's workers on user "stop"
	push               *push.Store             // optional; wired via AttachPush (Web Push)
	pushVAPIDPublicKey string                  // VAPID public key handed to subscribing clients
	maintenance        *maintenance.Controller // optional; wired via AttachMaintenance

	// Instance settings + the live PromptStore. Wired together via
	// AttachInstanceSettings so the system-prompt endpoints can both
	// persist a change and refresh the in-memory base override the
	// pipeline reads. Both nil → the endpoints return 503.
	instanceSettings *InstanceSettingsStore
	promptStore      *ctxbuild.PromptStore

	// Public-link sharing policy (wiki page / note shares). Empty
	// publicHostSet disables share serving — the toggle returns 503
	// and /p/{key} returns 404 regardless of inbound host.
	sharingCfg    config.SharingConfig
	publicHostSet map[string]bool

	// Relationship backfill state. backfillDeps is set once at
	// startup; backfillState is the latest Progress snapshot and is
	// overwritten under backfillMu from the worker goroutine.
	backfillDeps  *backfill.Deps
	backfillMu    sync.Mutex
	backfillState *backfill.Progress
}

// New builds a Handler from the admin config plus the shared pool.
// Returns an error when the resolved relying-party list is empty,
// any RP carries no origins/hosts, or the WebAuthn library rejects
// the per-RP config.
//
// PUBLIC-PROXY-MIGRATION: admin now supports multiple relying
// parties, each bound to a distinct set of inbound Host headers.
// Legacy single-RP configs (rp_id + rp_origins, no
// [[admin.relying_party]] blocks) get auto-synthesised into one
// RP via AdminConfig.EffectiveRelyingParties so existing
// deployments keep working without a config edit.
func New(cfg config.AdminConfig, pool *db.Pool) (*Handler, error) {
	if pool == nil || pool.DB == nil {
		return nil, errors.New("admin: db pool required")
	}
	rps := cfg.EffectiveRelyingParties()
	if len(rps) == 0 {
		return nil, errors.New("admin: at least one relying party required (legacy rp_id+rp_origins or [[admin.relying_party]] block)")
	}
	display := cfg.RPDisplayName
	if display == "" {
		display = "Familiar Admin"
	}

	// Build one *webauthn.WebAuthn per RP and key it under every
	// host the RP claims. Duplicate hosts are rejected at validate
	// time, but defensively check here too — a duplicate at this
	// layer is a programming bug worth surfacing.
	rpMap := make(map[string]*webauthn.WebAuthn, len(rps))
	for _, rp := range rps {
		if rp.RPID == "" || len(rp.Origins) == 0 || len(rp.Hosts) == 0 {
			return nil, fmt.Errorf("admin: relying party %q is missing rp_id, origins, or hosts", rp.RPID)
		}
		wa, err := webauthn.New(&webauthn.Config{
			RPID:                 rp.RPID,
			RPDisplayName:        display,
			RPOrigins:            rp.Origins,
			EncodeUserIDAsString: true,
		})
		if err != nil {
			return nil, fmt.Errorf("admin: webauthn config (rp_id=%q): %w", rp.RPID, err)
		}
		for _, host := range rp.Hosts {
			key := strings.ToLower(host)
			if _, dup := rpMap[key]; dup {
				return nil, fmt.Errorf("admin: host %q is claimed by multiple relying parties", key)
			}
			rpMap[key] = wa
		}
	}

	ttl := time.Duration(cfg.SessionMaxAge) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}

	h := &Handler{
		cfg:           cfg,
		rps:           rpMap,
		cookieSecure:  cfg.CookieSecure,
		credentials:   NewCredentialStore(pool),
		sessions:      NewSessionStore(pool),
		pending:       newPendingStore(),
		enrollTokens:  NewEnrollmentTokenStore(pool),
		sessionMaxAge: ttl,
	}
	return h, nil
}

// StartSessionGC launches the background sweep that reaps expired
// admin_sessions rows. Call once after construction with the root
// context; the goroutine stops when ctx is cancelled.
func (h *Handler) StartSessionGC(ctx context.Context) {
	h.sessions.StartGC(ctx, time.Hour)
}

// webauthnFor picks the right *webauthn.WebAuthn for an inbound
// request based on its Host header (case-insensitive, port stripped).
// Returns an error when no RP is configured for the host — the call
// sites map that to a 400 so an unconfigured host can't silently
// fall through to whichever RP happened to be enumerated first.
func (h *Handler) webauthnFor(r *http.Request) (*webauthn.WebAuthn, error) {
	host := strings.ToLower(stripPort(r.Host))
	wa, ok := h.rps[host]
	if !ok {
		return nil, fmt.Errorf("no relying party configured for host %q", host)
	}
	return wa, nil
}

// stripPort returns host with any ":port" suffix removed. IPv6
// literals are preserved as-is when the bracketed form is used.
func stripPort(host string) string {
	if host == "" {
		return host
	}
	// IPv6 literal: "[::1]:8000" → keep through the closing bracket.
	if host[0] == '[' {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return host[:end+1]
		}
		return host
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// Mux returns an http.Handler that serves the Familiar console API.
// As of FAMILIAR-WORKSPACE-SPEC Phase 0 the gateway is API-only;
// static file serving moved to familiar-workspace, which reverse-
// proxies these endpoints to us. Routes served here:
//
//	POST /console/api/auth/*       — WebAuthn ceremony endpoints
//	GET  /console/api/auth/status  — session probe
//	/console/api/*                 — authed, wrapped in requireAuth
//
// The /admin → /console back-compat redirect now lives in the
// workspace, since URL bookmarks land at the workspace's hostname,
// not the gateway's localhost loopback.
//
// authed is optional: pass nil until dashboard API endpoints exist.
//
// Per-role classification (any-role vs admin-only) is set at
// registration time below. The any-role tier covers anything a
// regular user needs to manage their own data — memories, shards,
// shard tokens, the skill catalog they need to populate a shard's
// allowlist. Admin-only stays for cross-user operations: user
// management, backfill controls, full-instance status snapshot.
// Per-handler ownership scoping (404 when a non-admin asks for
// someone else's row) lives inside the handlers themselves; this
// file just decides whether the route requires the admin role.
func (h *Handler) Mux(authed http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /console/api/auth/register/begin", h.registerBegin)
	mux.HandleFunc("POST /console/api/auth/register/finish", h.registerFinish)
	mux.HandleFunc("GET /console/api/auth/register/status", h.registerStatus)
	mux.HandleFunc("POST /console/api/auth/login/begin", h.loginBegin)
	mux.HandleFunc("POST /console/api/auth/login/finish", h.loginFinish)
	mux.HandleFunc("POST /console/api/auth/logout", h.logout)
	mux.HandleFunc("GET /console/api/auth/status", h.authStatus)

	// Cross-domain passkey enrollment (CROSS-DOMAIN-ENROLLMENT.md).
	// /enrollment-token is session-gated — only an authenticated
	// user can issue one (admin override lets one user issue on
	// behalf of another). /enroll/begin + /enroll/finish are
	// token-gated, not session-gated, since the user enrolling is
	// by definition not yet authenticated on the target RP.
	mux.HandleFunc("POST /console/api/auth/enroll/begin", h.enrollBegin)
	mux.HandleFunc("POST /console/api/auth/enroll/finish", h.enrollFinish)

	// Public-link share render. Unauthenticated by design — anyone
	// with the link sees the page. The handler host-checks against
	// the configured public allow-list and 404s on Tailscale-direct
	// or any other private host so a link only resolves from the
	// public-facing side.
	mux.HandleFunc("GET /p/{key}", h.publicShare)
	mux.HandleFunc("GET /p/{key}/media/{id}", h.publicShareMedia)

	// Scheduled-action webhooks (SCHEDULED-ACTIONS-SPEC Phase 3).
	// Public by design — external systems fire these; the URL token
	// is the credential and failure shapes are deliberately uniform
	// (see actionWebhook).
	mux.HandleFunc("POST /console/api/actions/hooks/{token}", h.actionWebhook)

	// Authenticated API surface. Everything under /console/api/ passes
	// through authRequired, which loads AuthUser (role + email) into
	// the request context. Routes that must additionally require the
	// admin role are wrapped with adminOnly at registration time —
	// see the "Admin-only:" block below.
	//
	// Memory browser endpoints are always registered (even without a
	// backing store) so the frontend can probe
	// /console/api/memories/facets and discover the feature is disabled —
	// the endpoint returns {"available": false} in that case instead
	// of a 404 that the client would have to interpret.
	authedMux := http.NewServeMux()

	// ── Any-role (authRequired only) ───────────────────────────
	// Memory endpoints, hot memory, status, skills catalog, and
	// shards CRUD all live here. Each of those handlers enforces
	// per-user scoping internally — non-admins see only their own
	// rows, admins see everything. Routes that have NO per-user
	// concept (status, skills/tools) just don't filter; the data is
	// safe to surface to any authenticated user.
	authedMux.HandleFunc("GET /console/api/status", h.getStatus)
	// Maintenance mode: GET is any-role (the frontend banner reads it);
	// POST (the toggle + fallback-model selection) is admin-only,
	// wrapped in the admin-only block below.
	authedMux.HandleFunc("GET /console/api/maintenance", h.getMaintenance)
	authedMux.HandleFunc("GET /console/api/skills", h.listSkillCatalog)
	// Cross-domain passkey enrollment (CROSS-DOMAIN-ENROLLMENT.md).
	// Both endpoints require an active session; admins can additionally
	// issue tokens on behalf of other users via the canonical_id field.
	authedMux.HandleFunc("POST /console/api/auth/enrollment-token", h.createEnrollmentToken)
	authedMux.HandleFunc("GET /console/api/auth/passkeys", h.listUserPasskeys)
	authedMux.HandleFunc("DELETE /console/api/auth/passkeys/{credential_id}", h.deleteUserPasskey)
	// Self-service profile editor. Any authenticated user can rename
	// themselves here; admin-side renames of other users stay on
	// PATCH /console/api/users/{id} (admin-only).
	authedMux.HandleFunc("PATCH /console/api/profile/me", h.patchOwnProfile)
	authedMux.HandleFunc("GET /console/api/skills/tools", h.listSkillTools)
	authedMux.HandleFunc("GET /console/api/models", h.listModels)

	authedMux.HandleFunc("GET /console/api/memories", h.listMemories)
	authedMux.HandleFunc("GET /console/api/memories/facets", h.memoryFacets)
	authedMux.HandleFunc("GET /console/api/memories/{id}", h.getMemory)
	authedMux.HandleFunc("PATCH /console/api/memories/{id}", h.patchMemory)
	authedMux.HandleFunc("DELETE /console/api/memories/{id}", h.deleteMemory)
	authedMux.HandleFunc("GET /console/api/memories/{id}/versions", h.memoryVersions)
	authedMux.HandleFunc("GET /console/api/memories/{id}/relationships", h.memoryRelationships)
	authedMux.HandleFunc("GET /console/api/memories/{id}/chain", h.memoryChain)
	authedMux.HandleFunc("POST /console/api/memories/{id}/chain/collapse", h.collapseMemoryChain)
	authedMux.HandleFunc("GET /console/api/profile", h.getProfile)
	authedMux.HandleFunc("PATCH /console/api/profile", h.patchProfile)
	authedMux.HandleFunc("GET /console/api/sessions", h.listChatSessions)
	authedMux.HandleFunc("GET /console/api/memory/graph", h.listGraph)
	authedMux.HandleFunc("GET /console/api/memory/entities", h.listEntities)
	authedMux.HandleFunc("GET /console/api/memory/relationship/{id}", h.getRelationship)
	authedMux.HandleFunc("PATCH /console/api/memory/relationship/{id}", h.patchRelationship)
	authedMux.HandleFunc("DELETE /console/api/memory/relationship/{id}", h.deleteRelationship)
	authedMux.HandleFunc("GET /console/api/memory/entity/{name}/facts", h.entityFacts)
	authedMux.HandleFunc("POST /console/api/memory/entity/{name}/merge", h.mergeEntity)
	authedMux.HandleFunc("DELETE /console/api/memory/entity/{name}", h.deleteEntity)
	authedMux.HandleFunc("GET /console/api/memory/health", h.memoryHealth)

	// Workspace conversations (FAMILIAR-WORKSPACE-SPEC Phase 1a).
	// Role-scoped via scopeForConversations — non-admin sees own
	// only; admin can override with ?user_id=. Per-resource authz
	// (404 on non-owned) lives in the handlers.
	authedMux.HandleFunc("GET /console/api/conversations", h.listConversations)
	authedMux.HandleFunc("POST /console/api/conversations", h.createConversation)
	authedMux.HandleFunc("GET /console/api/conversations/{id}", h.getConversation)
	authedMux.HandleFunc("PATCH /console/api/conversations/{id}", h.patchConversation)
	authedMux.HandleFunc("DELETE /console/api/conversations/{id}", h.deleteConversation)
	authedMux.HandleFunc("GET /console/api/conversations/{id}/messages", h.listConversationMessages)
	authedMux.HandleFunc("POST /console/api/conversations/{id}/messages", h.appendConversationMessage)
	// Move a conversation between folders. body: {"folder_id":
	// "<uuid>" | "" | null}. Empty / null clears the folder.
	authedMux.HandleFunc("POST /console/api/conversations/{id}/move", h.moveConversation)

	// Chat folders ("projects") — flat per-user grouping for
	// conversations. Backed by chat_folders; conversations carry
	// folder_id (nullable, SET NULL on folder delete).
	authedMux.HandleFunc("GET /console/api/chat/folders", h.listChatFolders)
	authedMux.HandleFunc("POST /console/api/chat/folders", h.createChatFolder)
	authedMux.HandleFunc("PATCH /console/api/chat/folders/{id}", h.patchChatFolder)
	authedMux.HandleFunc("DELETE /console/api/chat/folders/{id}", h.deleteChatFolder)

	// Workspace notes (FAMILIAR-WORKSPACE-SPEC Phase 2a). Same
	// scope-helper pattern as conversations. /search needs the
	// most specific path registered first so it matches before
	// the {id} pattern.
	// /console/api/notes/* removed — frontend now talks to
	// /console/api/books/personal/pages directly. The notes table
	// lives on as a backup; no public reads or writes touch it.

	// Home aggregates. /home/pins unions pinned notes + pinned
	// conversations into one chronologically-sorted list so the
	// Home surface (desktop + mobile) renders pins with a single
	// fetch. Backed by the per-kind .pinned columns; no new table.
	authedMux.HandleFunc("GET /console/api/home/pins", h.homePins)
	// /home/weather backs the Home weather widget — browser-supplied
	// lat/lon in, a structured current+hourly forecast out.
	authedMux.HandleFunc("GET /console/api/home/weather", h.homeWeather)
	// SSE push for page-saved / page-deleted events. Editors open
	// one EventSource per shell; the handler filters by the
	// connected user's book memberships.
	authedMux.HandleFunc("GET /console/api/events/pages", h.servePageEvents)

	// System prompt. GET is any-role — the handler itself enforces
	// the system_prompt_user_visible gate for non-admins. PUT is
	// admin-only (wrapped below in the admin-only block).
	authedMux.HandleFunc("GET /console/api/system-prompt", h.getSystemPrompt)

	// Books + wiki pages (BOOKS-WIKI-ARCHITECTURE Phase 1a).
	// All routes are membership-scoped via scopeForWiki — non-
	// members get 404 (existence-leak prevention). Per-search
	// path registered before {slug} so /search doesn't get eaten.
	authedMux.HandleFunc("GET /console/api/books", h.listBooks)
	authedMux.HandleFunc("POST /console/api/books", h.createBook)
	// Personal book — registered before the {slug} pattern so the
	// literal "personal" path always takes precedence.
	authedMux.HandleFunc("GET /console/api/books/personal", h.getPersonalBook)
	authedMux.HandleFunc("GET /console/api/books/{slug}", h.getBook)
	authedMux.HandleFunc("PATCH /console/api/books/{slug}", h.patchBook)
	authedMux.HandleFunc("DELETE /console/api/books/{slug}", h.deleteBook)
	authedMux.HandleFunc("GET /console/api/books/{slug}/members", h.listBookMembers)
	authedMux.HandleFunc("POST /console/api/books/{slug}/members", h.addBookMember)
	authedMux.HandleFunc("PATCH /console/api/books/{slug}/members/{user_id}", h.patchBookMember)
	authedMux.HandleFunc("DELETE /console/api/books/{slug}/members/{user_id}", h.deleteBookMember)
	authedMux.HandleFunc("GET /console/api/books/{slug}/search", h.searchBookPages)
	authedMux.HandleFunc("GET /console/api/books/{slug}/pages", h.listBookPages)
	authedMux.HandleFunc("POST /console/api/books/{slug}/pages", h.createBookPage)
	authedMux.HandleFunc("GET /console/api/books/{slug}/pages/{page_slug}", h.getBookPage)
	authedMux.HandleFunc("PATCH /console/api/books/{slug}/pages/{page_slug}", h.patchBookPage)
	authedMux.HandleFunc("DELETE /console/api/books/{slug}/pages/{page_slug}", h.deleteBookPage)
	authedMux.HandleFunc("GET /console/api/books/{slug}/pages/{page_slug}/revisions", h.listBookPageRevisions)
	authedMux.HandleFunc("GET /console/api/books/{slug}/pages/{page_slug}/revisions/{rev_id}", h.getBookPageRevision)
	authedMux.HandleFunc("GET /console/api/books/{slug}/pages/{page_slug}/links", h.listBookPageLinks)
	authedMux.HandleFunc("GET /console/api/books/{slug}/pages/{page_slug}/backlinks", h.listBookPageBacklinks)
	// By-id sibling endpoints — used by the frontend Notes panel
	// (which keeps page IDs in its model from the legacy notes shape)
	// and any caller that has an id and doesn't want to round-trip
	// through a list to get a slug.
	authedMux.HandleFunc("GET /console/api/books/{slug}/page-by-id/{page_id}", h.getBookPageByID)
	authedMux.HandleFunc("PATCH /console/api/books/{slug}/page-by-id/{page_id}", h.patchBookPageByID)
	authedMux.HandleFunc("DELETE /console/api/books/{slug}/page-by-id/{page_id}", h.deleteBookPageByID)
	authedMux.HandleFunc("POST /console/api/books/{slug}/page-by-id/{page_id}/append", h.appendBookPageByID)
	authedMux.HandleFunc("POST /console/api/books/{slug}/page-by-id/{page_id}/pin", h.pinBookPageByID)

	// Page media (MEDIA-DIAGRAMS Phase 1): upload is write-gated on
	// the book; serving is membership-gated and gateway-proxied.
	authedMux.HandleFunc("POST /console/api/books/{slug}/page-by-id/{page_id}/media", h.uploadPageMedia)
	authedMux.HandleFunc("GET /console/api/books/{slug}/page-by-id/{page_id}/media", h.listPageMedia)
	authedMux.HandleFunc("DELETE /console/api/media/{id}", h.deleteMedia)
	authedMux.HandleFunc("GET /console/api/media/{id}", h.serveMedia)
	// Web Push subscriptions (PWA notifications). key → VAPID public key
	// for the browser to subscribe with; subscribe/unsubscribe manage the
	// caller's own device registrations.
	authedMux.HandleFunc("GET /console/api/push/key", h.pushKey)
	authedMux.HandleFunc("POST /console/api/push/subscribe", h.pushSubscribe)
	authedMux.HandleFunc("DELETE /console/api/push/subscribe", h.pushUnsubscribe)
	// Reparent + reorder. body: {"parent_id": "<uuid>"|"", "sort_order": <int>}.
	// Same write-capability gate as PATCH/DELETE; the store enforces
	// cycle / cross-book / soft-delete invariants.
	authedMux.HandleFunc("POST /console/api/books/{slug}/page-by-id/{page_id}/move", h.moveBookPageByID)
	// Public-link share toggle. body: {"enabled": true|false}.
	// Requires the same page-write capability as PATCH/DELETE.
	authedMux.HandleFunc("POST /console/api/books/{slug}/page-by-id/{page_id}/share", h.shareToggle)
	authedMux.HandleFunc("GET /console/api/books/{slug}/page-by-id/{page_id}/links", h.listBookPageLinksByID)
	authedMux.HandleFunc("GET /console/api/books/{slug}/page-by-id/{page_id}/backlinks", h.listBookPageBacklinksByID)

	// Dashboard aggregates (Phase F). Each handler role-scopes via
	// dashboardScopeFor — non-admin sees own data regardless of
	// ?user_id=; admin can override. No admin-only wrapper.
	authedMux.HandleFunc("GET /console/api/dashboard/overview", h.dashboardOverview)
	authedMux.HandleFunc("GET /console/api/dashboard/recent_sessions", h.dashboardRecentSessions)
	authedMux.HandleFunc("GET /console/api/dashboard/recent_writes", h.dashboardRecentWrites)
	authedMux.HandleFunc("GET /console/api/dashboard/entity_breakdown", h.dashboardEntityBreakdown)
	authedMux.HandleFunc("GET /console/api/dashboard/shard_summary", h.dashboardShardSummary)
	authedMux.HandleFunc("GET /console/api/dashboard/graph_preview", h.dashboardGraphPreview)
	authedMux.HandleFunc("GET /console/api/dashboard/growth_sparkline", h.dashboardGrowthSparkline)

	// Shards CRUD + tokens — moved out of admin-only in Phase B.
	// Each handler scopes to the authenticated user's shards via
	// ownerIDFor; admins implicitly bypass the filter inside the
	// handlers. Token revoke verifies via tokenBelongsToOwner.
	authedMux.HandleFunc("GET /console/api/shards", h.listShards)
	authedMux.HandleFunc("POST /console/api/shards", h.createShard)
	authedMux.HandleFunc("GET /console/api/shards/{id}", h.getShard)
	authedMux.HandleFunc("PATCH /console/api/shards/{id}", h.patchShard)
	authedMux.HandleFunc("DELETE /console/api/shards/{id}", h.deleteShard)
	authedMux.HandleFunc("POST /console/api/shards/{id}/disable", h.disableShard)
	authedMux.HandleFunc("POST /console/api/shards/{id}/enable", h.enableShard)
	authedMux.HandleFunc("GET /console/api/shards/{id}/tokens", h.listShardTokens)
	authedMux.HandleFunc("POST /console/api/shards/{id}/tokens", h.createShardToken)
	authedMux.HandleFunc("POST /console/api/shard_tokens/{tid}/revoke", h.revokeShardToken)

	// Shard passkeys — SHARD-AUTH-SPEC Phase 1. Same scoping posture
	// as shard tokens: handlers verify ownership via canSeeShard and
	// refuse shard sessions outright (a shard can't manage its own
	// passkeys; that's the whole point of the owner/shard split).
	authedMux.HandleFunc("GET /console/api/shards/{id}/passkeys", h.listShardPasskeys)
	authedMux.HandleFunc("POST /console/api/shards/{id}/passkeys/begin", h.shardPasskeyRegisterBegin)
	authedMux.HandleFunc("POST /console/api/shards/{id}/passkeys/finish", h.shardPasskeyRegisterFinish)
	authedMux.HandleFunc("DELETE /console/api/shards/{id}/passkeys/{passkey_id}", h.deleteShardPasskey)

	// Scheduled actions — SCHEDULED-ACTIONS-SPEC Phase 1. Owner-
	// scoped like shards; shard sessions refused in actionScope.
	authedMux.HandleFunc("GET /console/api/actions", h.listActions)
	authedMux.HandleFunc("POST /console/api/actions", h.createAction)
	authedMux.HandleFunc("GET /console/api/actions/{id}", h.getAction)
	authedMux.HandleFunc("PATCH /console/api/actions/{id}", h.patchAction)
	authedMux.HandleFunc("DELETE /console/api/actions/{id}", h.deleteAction)
	authedMux.HandleFunc("POST /console/api/actions/{id}/enable", h.setActionEnabled(true))
	authedMux.HandleFunc("POST /console/api/actions/{id}/disable", h.setActionEnabled(false))
	authedMux.HandleFunc("POST /console/api/actions/{id}/run", h.runActionNow)
	authedMux.HandleFunc("GET /console/api/actions/{id}/runs", h.listActionRuns)

	// Imported skills — SKILL-PACKAGES-SPEC Phase 2. Library
	// management is admin-only (code/prompt admission is an operator
	// decision); listing + shard binding are owner-level.
	authedMux.HandleFunc("GET /console/api/skillpacks", h.listSkillPackages)
	authedMux.Handle("POST /console/api/skillpacks/import", h.adminOnly(http.HandlerFunc(h.importSkillPackage)))
	authedMux.Handle("POST /console/api/skillpacks/rescan", h.adminOnly(http.HandlerFunc(h.rescanSkillPackages)))
	authedMux.Handle("POST /console/api/skillpacks/{id}/enable", h.adminOnly(h.setSkillPackageDisabled(false)))
	authedMux.Handle("POST /console/api/skillpacks/{id}/disable", h.adminOnly(h.setSkillPackageDisabled(true)))
	authedMux.Handle("POST /console/api/skillpacks/{id}/chat", h.adminOnly(http.HandlerFunc(h.setSkillPackageChat)))
	authedMux.Handle("DELETE /console/api/skillpacks/{id}", h.adminOnly(http.HandlerFunc(h.deleteSkillPackage)))
	authedMux.HandleFunc("GET /console/api/shards/{id}/skills", h.listShardSkills)
	authedMux.HandleFunc("PUT /console/api/shards/{id}/skills", h.putShardSkills)

	// User skills — USER-SKILLS-SPEC Phase A. Owner-scoped private
	// libraries: any authed user manages their own skills (import /
	// enable / disable / delete) without an admin. Usable by the
	// Autonomous deep-research progress card (§6.7): the workspace polls
	// this while a run is active to render/restore the spinner.
	authedMux.HandleFunc("GET /console/api/research/runs/active", h.getActiveResearchRun)
	authedMux.HandleFunc("POST /console/api/research/runs/{id}/cancel", h.cancelResearchRun)
	// owner's shards in Phase A; trusted-path exposure is Phase B.
	authedMux.HandleFunc("GET /console/api/skills/mine", h.listMySkills)
	authedMux.HandleFunc("POST /console/api/skills/mine/import", h.importMySkill)
	authedMux.HandleFunc("POST /console/api/skills/mine/{id}/enable", h.setMySkillDisabled(false))
	authedMux.HandleFunc("POST /console/api/skills/mine/{id}/disable", h.setMySkillDisabled(true))
	authedMux.HandleFunc("POST /console/api/skills/mine/{id}/chat", h.setMySkillChat)
	authedMux.HandleFunc("DELETE /console/api/skills/mine/{id}", h.deleteMySkill)
	// Authoring (Phase C): server-side SKILL.md composition, editor
	// content load, duplicate-as-mine, zip export.
	authedMux.HandleFunc("PUT /console/api/skills/mine/{name}", h.putMySkill)
	authedMux.HandleFunc("GET /console/api/skills/mine/{id}/content", h.getMySkillContent)
	authedMux.HandleFunc("POST /console/api/skills/mine/{id}/duplicate", h.duplicateMySkill)
	authedMux.HandleFunc("GET /console/api/skills/mine/{id}/export", h.exportMySkill)

	// ── Admin-only (authRequired + adminOnly) ──────────────────
	// Cross-user operations and instance maintenance. The frontend
	// hides these via the .admin-only CSS class; the backend enforces
	// regardless via the adminOnly middleware so a curl bypass still
	// 403s.
	authedMux.Handle("PUT /console/api/system-prompt", h.adminOnly(http.HandlerFunc(h.putSystemPrompt)))
	authedMux.Handle("POST /console/api/maintenance", h.adminOnly(http.HandlerFunc(h.setMaintenance)))
	authedMux.Handle("POST /console/api/controls/backfill-relationships", h.adminOnly(http.HandlerFunc(h.startBackfill)))
	authedMux.Handle("GET /console/api/controls/backfill-relationships", h.adminOnly(http.HandlerFunc(h.getBackfillStatus)))
	// Any-role lookup endpoint that powers the in-app user picker
	// (book-members modal). Trimmed payload, prefix match only —
	// see lookupUsers for the threat model. Registered BEFORE the
	// admin-only listUsers so /lookup doesn't get swallowed.
	authedMux.HandleFunc("GET /console/api/users/lookup", h.lookupUsers)
	authedMux.Handle("GET /console/api/users", h.adminOnly(http.HandlerFunc(h.listUsers)))
	authedMux.Handle("POST /console/api/users", h.adminOnly(http.HandlerFunc(h.inviteUser)))
	authedMux.Handle("GET /console/api/users/{id}", h.adminOnly(http.HandlerFunc(h.getUser)))
	authedMux.Handle("PATCH /console/api/users/{id}", h.adminOnly(http.HandlerFunc(h.patchUser)))
	authedMux.Handle("POST /console/api/users/{id}/status", h.adminOnly(http.HandlerFunc(h.setUserStatus)))
	authedMux.Handle("POST /console/api/users/{id}/identities", h.adminOnly(http.HandlerFunc(h.linkIdentity)))
	authedMux.Handle("DELETE /console/api/users/{id}/identities/{platform}/{platform_id}", h.adminOnly(http.HandlerFunc(h.unlinkIdentity)))

	if authed != nil {
		authedMux.Handle("/console/api/", authed)
	}
	mux.Handle("/console/api/", h.authRequired(authedMux))
	return mux
}

// RequireAuth wraps next so every request must carry a valid admin
// session cookie. Exposed for dashboard endpoints the gateway wires up
// later. On failure returns 401 with a JSON error body.
func (h *Handler) RequireAuth(next http.Handler) http.Handler { return h.requireAuth(next) }

func (h *Handler) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := h.authenticatedUser(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		ctx := context.WithValue(r.Context(), ContextKeyUserID, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authenticatedUser reads the session cookie and returns the canonical
// user ID on success. Separate from requireAuth so status endpoints
// can call it without writing 401s. For SHARD sessions this still
// returns the canonical owner — handlers that need to know it's a
// shard session call authenticatedSession instead.
func (h *Handler) authenticatedUser(r *http.Request) (string, bool) {
	sess, ok := h.authenticatedSession(r)
	if !ok {
		return "", false
	}
	return sess.UserID, true
}

// UserIDFromRequest reads the admin session cookie and returns the
// canonical user ID. Exported so the native chat adapter can resolve
// workspace UI users without requiring an X-User-Email header.
// Implements native.SessionReader.
//
// The session cookie alone isn't sufficient — a user whose status
// has been flipped to disabled/denied/pending still holds a valid
// session token until its TTL expires (or until DeleteByUser
// revokes it). Re-check status here so /api/chat refuses requests
// from non-approved users the moment the status flips, matching
// the gate authRequired runs for /console/api/*. When the user
// store isn't wired (test harness etc.) we fall back to the bare
// session check.
func (h *Handler) UserIDFromRequest(r *http.Request) (string, bool) {
	uid, ok := h.authenticatedUser(r)
	if !ok {
		return "", false
	}
	if h.users == nil {
		return uid, true
	}
	u, err := h.users.GetUser(r.Context(), uid)
	if err != nil || u == nil || u.Status != identity.StatusApproved {
		return "", false
	}
	return uid, true
}

// authenticatedSession reads the session cookie and returns the
// validated row with PrincipalType + PrincipalID populated. Used
// by authRequired (which needs to branch on principal kind) and
// by /auth/status (which surfaces the principal type to the
// frontend).
func (h *Handler) authenticatedSession(r *http.Request) (*AdminSession, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, false
	}
	sess, err := h.sessions.Validate(r.Context(), cookie.Value)
	if err != nil {
		return nil, false
	}
	return sess, true
}

// ─────────────────────────────── registration ───────────────────────────────

// registerStatus tells the frontend whether any credential exists.
// When count==0 the UI shows the "set up your key" flow without
// requiring prior auth; otherwise registration is gated behind login.
func (h *Handler) registerStatus(w http.ResponseWriter, r *http.Request) {
	n, err := h.credentials.Count(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"credentials_registered": n,
		"requires_auth":          n > 0,
	})
}

// registerBegin starts a WebAuthn registration ceremony. Two entry
// modes:
//
//  1. First-run bootstrap — credentials table is empty. The caller
//     supplies {email, display_name} and we provision the canonical
//     admin row (id=admin.first_user_id from gateway.toml, role="admin",
//     status="approved") then proceed with the ceremony. This is the
//     ONE path that's reachable without a session, and it only works
//     while no credentials exist on the deployment.
//  2. Authenticated add-key — credentials exist (deployed instance).
//     The caller MUST hold a valid session cookie; the target user
//     is taken from the session (NOT body.email), so a request for
//     a different canonical id is impossible. Anyone without a
//     session (a fresh user being invited, cross-domain enrollment
//     onto a new RP) must use the /console/api/auth/enroll/* flow,
//     which is properly token-gated.
//
// The previous "email-keyed unauth add-key" path was an
// authentication bypass: looking up users.email and trusting the
// caller to be that user let anyone register a passkey under any
// approved email, including admin's. Closed; replaced with the
// session-gated branch above. See SECURITY-WEBAUTHN.md for details.
func (h *Handler) registerBegin(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		writeJSONError(w, http.StatusServiceUnavailable,
			"user management not configured — registration disabled")
		return
	}

	var body struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body.Email = strings.TrimSpace(body.Email)

	count, err := h.credentials.Count(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var userID, displayName string
	if count == 0 {
		// First-run bootstrap.
		//
		// OWNER-MIGRATION: the bootstrap canonical_id is now driven by
		// admin.first_user_id in the gateway TOML rather than being
		// hard-coded to "owner". Refuse the ceremony when it's blank
		// — silently inventing a default is the exact bug the
		// migration is closing.
		if body.Email == "" {
			writeJSONError(w, http.StatusBadRequest,
				"email required for first-time setup")
			return
		}
		userID = strings.TrimSpace(h.cfg.FirstUserID)
		if userID == "" {
			writeJSONError(w, http.StatusServiceUnavailable,
				"admin.first_user_id is not configured — set it in gateway.toml before the first registration")
			return
		}
		displayName = body.DisplayName
		if displayName == "" {
			displayName = userID
		}
		if err := h.users.CreateFirstRun(r.Context(), userID, displayName, body.Email); err != nil {
			writeJSONError(w, http.StatusInternalServerError,
				"create first-run admin: "+err.Error())
			return
		}
	} else {
		// Authenticated add-key path. Identity comes from the
		// session cookie, NEVER from the request body. Anyone who
		// isn't already logged in must use the enrollment-token
		// flow (/console/api/auth/enroll/*).
		callerID, ok := h.authenticatedUser(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized,
				"authentication required to register a new key; ask an admin for an enrollment link if you don't have an existing passkey")
			return
		}
		u, err := h.users.GetUser(r.Context(), callerID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError,
				"load user: "+err.Error())
			return
		}
		if u == nil {
			writeJSONError(w, http.StatusForbidden, "user not found")
			return
		}
		if u.Status != identity.StatusApproved {
			writeJSONError(w, http.StatusForbidden, "user not approved")
			return
		}
		userID = u.ID
		displayName = u.DisplayName
	}

	existing, err := h.credentials.ListByUser(r.Context(), userID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// New registration: the handle baked into the authenticator is
	// the user's id at this moment. Pinning handle == id here keeps
	// WebAuthnID() stable even if the canonical id is renamed later.
	user := &adminUser{
		id:          userID,
		handle:      userID,
		displayName: displayName,
		credentials: toCredentials(existing),
	}

	wa, err := h.webauthnFor(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// FAMILIAR-PASSKEY-SPEC: request a discoverable credential with
	// user verification. AuthenticatorAttachment is intentionally
	// unset so the browser offers both platform authenticators
	// (Touch ID, Windows Hello, iCloud Keychain) AND cross-platform
	// ones (YubiKey). residentKey=preferred upgrades the credential
	// to a passkey when the authenticator supports resident keys;
	// older YubiKeys (firmware <5.2) ignore the preference and stay
	// non-discoverable, which is still fine for the allowCredentials
	// login flow.
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

	token, err := randomToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.pending.put(token, *sessionData, PendingKindRegister, userID)
	h.setPendingCookie(w, token)

	writeJSON(w, http.StatusOK, creation)
}

// registerFinish parses the client's credential.create() response and
// stores the resulting Credential.
func (h *Handler) registerFinish(w http.ResponseWriter, r *http.Request) {
	token, err := r.Cookie(pendingCookieName)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing pending cookie")
		return
	}
	entry, ok := h.pending.take(token.Value)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "pending ceremony expired")
		return
	}
	// Refuse a pending entry that wasn't minted by /register/begin —
	// otherwise an attacker holding a leaked enrollment token could
	// complete the ceremony here (which doesn't consume the token)
	// instead of through /enroll/finish, replaying the token until
	// its natural TTL expires.
	if entry.kind != PendingKindRegister {
		writeJSONError(w, http.StatusBadRequest, "pending ceremony is not a registration")
		return
	}
	h.clearPendingCookie(w)

	parsed, err := protocol.ParseCredentialCreationResponseBody(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse credential: "+err.Error())
		return
	}

	existing, err := h.credentials.ListByUser(r.Context(), entry.userID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	user := &adminUser{
		id:          entry.userID,
		handle:      entry.userID, // handle == id at registration
		displayName: entry.userID,
		credentials: toCredentials(existing),
	}

	wa, err := h.webauthnFor(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	cred, err := wa.CreateCredential(user, entry.data, parsed)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "create credential: "+err.Error())
		return
	}

	// Label the credential by type so the Profile panel can distinguish
	// passkeys ("Passkey — owner (2026-04-24)") from security keys
	// ("Security key — …"). Authenticator.Attachment is populated by
	// the library from the attestation response; legacy credentials
	// registered before this change had an empty attachment and fall
	// back to the generic "Key" label.
	label := "Key"
	switch cred.Authenticator.Attachment {
	case protocol.Platform:
		label = "Passkey"
	case protocol.CrossPlatform:
		label = "Security key"
	}
	displayName := fmt.Sprintf("%s — %s (%s)", label, entry.userID, time.Now().Format("2006-01-02"))
	if err := h.credentials.Insert(r.Context(), entry.userID, displayName, cred); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store credential: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "registered",
		"credential_id": encodeCredentialID(cred.ID),
		"user_id":       entry.userID,
	})
}

// ─────────────────────────────── login ──────────────────────────────────────

// loginBegin kicks off a discoverable-credential assertion. The
// browser's platform authenticator or security key picks the right
// credential from the allowCredentials empty list, which hands us back
// a userHandle so we know which row to verify against.
//
// SHARD-AUTH-SPEC Phase 1: the allowCredentials list is the union of
// user passkeys (webauthn_credentials) and shard passkeys
// (shard_passkeys). loginFinish tells them apart by re-looking-up the
// presented rawId in each table.
func (h *Handler) loginBegin(w http.ResponseWriter, r *http.Request) {
	// Load all registered credentials so the browser knows which keys
	// to offer. This avoids requiring resident/discoverable keys.
	userCreds, lErr := h.credentials.ListAll(r.Context())
	if lErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "list credentials: "+lErr.Error())
		return
	}
	var shardCreds []StoredShardPasskey
	if h.shardPasskeys != nil {
		shardCreds, lErr = h.shardPasskeys.ListAllActive(r.Context())
		if lErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "list shard passkeys: "+lErr.Error())
			return
		}
	}
	if len(userCreds) == 0 && len(shardCreds) == 0 {
		writeJSONError(w, http.StatusBadRequest, "no credentials registered")
		return
	}
	creds := toCredentials(userCreds)
	for _, p := range shardCreds {
		creds = append(creds, p.Credential)
	}
	// Drop unusable credentials before BeginLogin. A row whose blob
	// failed to carry a credential ID (corruption, a half-written
	// record) deserializes to a zero Credential with an empty ID; left
	// in the allow-list it can break the assertion the library builds —
	// and login is a shared ceremony, so ONE bad row would brick sign-in
	// for every user. A credential with no ID can't be asserted against
	// anyway, so skipping it is strictly safe.
	var dropped int
	creds, dropped = usableCredentials(creds)
	if dropped > 0 {
		log.Printf("[admin] loginBegin: skipped %d credential(s) with empty id (corrupt blob?)", dropped)
	}
	if len(creds) == 0 {
		writeJSONError(w, http.StatusBadRequest, "no credentials registered")
		return
	}
	// Placeholder ID — the WebAuthn library requires user.WebAuthnID()
	// to be non-empty for BeginLogin, but we re-patch session.UserID
	// in loginFinish once we've identified which table the chosen
	// credential lives in. Prefer a user ID so the legacy path
	// (no shard passkeys deployed) is byte-for-byte unchanged.
	var placeholderID string
	switch {
	case len(userCreds) > 0:
		placeholderID = userCreds[0].UserID
	default:
		placeholderID = shardCreds[0].ShardID
	}
	user := &adminUser{
		id:          placeholderID,
		displayName: "login",
		credentials: creds,
	}
	wa, err := h.webauthnFor(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	assertion, sessionData, err := wa.BeginLogin(user)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin login: "+err.Error())
		return
	}
	token, err := randomToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.pending.put(token, *sessionData, PendingKindLogin, "")
	h.setPendingCookie(w, token)
	writeJSON(w, http.StatusOK, assertion)
}

// loginFinish verifies the assertion, looks up the matching credential
// row, updates its sign_count, and mints an admin session cookie.
func (h *Handler) loginFinish(w http.ResponseWriter, r *http.Request) {
	token, err := r.Cookie(pendingCookieName)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing pending cookie")
		return
	}
	entry, ok := h.pending.take(token.Value)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "pending ceremony expired")
		return
	}
	// Refuse cross-flow pending entries — only /login/begin entries
	// reach /login/finish.
	if entry.kind != PendingKindLogin {
		writeJSONError(w, http.StatusBadRequest, "pending ceremony is not a login")
		return
	}
	h.clearPendingCookie(w)

	// Buffer the request body so we can peek at the credential ID
	// before FinishLogin consumes it. We need to identify the actual
	// user (not just "the first credential in the DB") to pass the
	// library's userHandle check.
	body, bErr := io.ReadAll(r.Body)
	if bErr != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+bErr.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	all, lErr := h.credentials.ListAll(r.Context())
	if lErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "list credentials: "+lErr.Error())
		return
	}

	// Peek at the rawId to decide whether this is a user passkey or a
	// shard passkey. The two tables are partitioned by credential_id
	// so a single rawId resolves to exactly one row in one table.
	// SHARD-AUTH-SPEC: shard_passkeys takes precedence when matched
	// (a credential can only live in one of the two tables, but the
	// lookup order is stable for tests).
	var partial struct {
		RawID string `json:"rawId"`
	}
	var rawID []byte
	if json.Unmarshal(body, &partial) == nil && partial.RawID != "" {
		if decoded, decErr := base64.RawURLEncoding.DecodeString(partial.RawID); decErr == nil {
			rawID = decoded
		}
	}
	var matchedShard *StoredShardPasskey
	if h.shardPasskeys != nil && len(rawID) > 0 {
		if sp, _ := h.shardPasskeys.GetByRawID(r.Context(), rawID); sp != nil {
			matchedShard = sp
		}
	}
	if matchedShard != nil {
		h.finishShardLogin(w, r, body, entry, matchedShard)
		return
	}

	// User-credential branch — original behavior.
	//
	// The go-webauthn library checks TWO things in FinishLogin:
	//   1. session.UserID == user.WebAuthnID()  ("ID mismatch for User and Session")
	//   2. response.userHandle == user.WebAuthnID()  ("User handle and User ID do not match")
	// WebAuthnID() is the stored handle, NOT the canonical user_id —
	// the device echoes back the handle it was registered with, which
	// survives a canonical-id rename (OWNER-MIGRATION). We patch the
	// session UserID + the adminUser handle to that stored handle so
	// both checks pass; the session itself is still created under the
	// canonical user_id.
	if len(all) == 0 {
		writeJSONError(w, http.StatusUnauthorized, "no matching credential")
		return
	}
	ownerID := all[0].UserID         // canonical id — for the session
	ownerHandle := all[0].UserHandle // WebAuthn handle — for verification
	if len(rawID) > 0 {
		if stored, _ := h.credentials.GetByRawID(r.Context(), rawID); stored != nil {
			ownerID = stored.UserID
			ownerHandle = stored.UserHandle
		}
	}

	// Patch session data to the handle — fixes check 1 (it must equal
	// WebAuthnID(), which is the handle).
	entry.data.UserID = []byte(ownerHandle)

	user := &adminUser{
		id:          ownerID,
		handle:      ownerHandle,
		displayName: "login",
		credentials: toCredentials(all),
	}

	// Trim AllowedCredentialIDs the same way finishShardLogin does:
	// loginBegin offered user passkeys + shard passkeys, but the
	// adminUser here only owns user passkeys. Without the trim, the
	// library's "user owns every allowed credential" check would
	// fail the moment any shard passkey exists on the instance.
	allowedUser := make([][]byte, 0, len(all))
	for _, c := range all {
		allowedUser = append(allowedUser, c.Credential.ID)
	}
	entry.data.AllowedCredentialIDs = allowedUser

	wa, err := h.webauthnFor(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	cred, err := wa.FinishLogin(user, entry.data, r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "verify: "+err.Error())
		return
	}

	// Credential owner already identified above (ownerID). Use the
	// library's credential match to double-check if available.
	foundUserID := ownerID
	if stored, _ := h.credentials.GetByRawID(r.Context(), cred.ID); stored != nil {
		foundUserID = stored.UserID
	}

	if err := h.credentials.UpdateSignCount(r.Context(), cred.ID, cred.Authenticator.SignCount); err != nil {
		log.Printf("[admin] warning: sign_count update: %v", err)
	}

	// Refuse to mint a session for a non-approved user. authRequired
	// would block them from /console/api/* anyway, but the same
	// session cookie is also accepted by /api/chat (via the native
	// adapter's UserIDFromRequest), which doesn't run authRequired.
	// Mint-time refusal closes that branch too.
	if h.users != nil {
		u, err := h.users.GetUser(r.Context(), foundUserID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "load user: "+err.Error())
			return
		}
		if u == nil {
			writeJSONError(w, http.StatusForbidden, "user not approved")
			return
		}
		if u.Status != identity.StatusApproved {
			// Status-specific so the login UI can show the right copy:
			// a new beta user awaiting approval needs different words
			// than a disabled or denied one. The string is matched
			// case-insensitively on the client (app.js setError).
			msg := "user not approved"
			switch u.Status {
			case identity.StatusPending:
				msg = "account pending approval"
			case identity.StatusDisabled:
				msg = "account disabled"
			case identity.StatusDenied:
				msg = "account access denied"
			}
			writeJSONError(w, http.StatusForbidden, msg)
			return
		}
	}

	sessionToken, err := h.sessions.Create(r.Context(), foundUserID, h.sessionMaxAge)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	h.setSessionCookie(w, sessionToken)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "authenticated",
		"user":           foundUserID,
		"principal_type": PrincipalTypeUser,
	})
}

// finishShardLogin completes a login ceremony where the matched
// credential lives in shard_passkeys. The shard's ID is the WebAuthn
// userHandle (set by shardWebAuthnUser.WebAuthnID), so we patch the
// session.UserID accordingly and present a shardWebAuthnUser whose
// credentials list is just this one shard's passkeys.
//
// Beyond the standard verify, this also re-checks that the shard is
// still active and console-enabled — disabling a shard or flipping
// console_access off must reject login immediately, not wait for the
// next request inside the established session.
func (h *Handler) finishShardLogin(w http.ResponseWriter, r *http.Request, body []byte, entry pendingEntry, matched *StoredShardPasskey) {
	if h.shards == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "shard sessions not configured")
		return
	}
	sh, err := h.shards.GetShard(r.Context(), matched.ShardID)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "shard not found")
		return
	}
	if !sh.Active() || !sh.ConsoleAccess {
		writeJSONError(w, http.StatusUnauthorized, "shard disabled")
		return
	}
	// Pull the full set of this shard's passkeys for the
	// shardWebAuthnUser — the library iterates them looking for the
	// one whose ID matches the assertion.
	existing, err := h.shardPasskeys.ListByShard(r.Context(), sh.ID, false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list shard passkeys: "+err.Error())
		return
	}
	user := newShardWebAuthnUser(sh, existing)

	// Patch session.UserID to the shard's webauthn ID so check 1 in
	// FinishLogin passes. Restore r.Body for the library to read.
	entry.data.UserID = user.WebAuthnID()
	r.Body = io.NopCloser(bytes.NewReader(body))

	// Trim session.AllowedCredentialIDs to just this shard's
	// credentials. loginBegin offered the union of every user
	// passkey and every shard passkey so the browser could pick
	// any of them; the library's FinishLogin then verifies that
	// `user.WebAuthnCredentials()` covers every entry in the
	// allow-list. shardWebAuthnUser only owns this one shard's
	// credentials by design, so without this trim the check fires
	// "user does not own all credentials from the allowed
	// credential list."
	allowed := make([][]byte, 0, len(existing))
	for _, p := range existing {
		allowed = append(allowed, p.Credential.ID)
	}
	entry.data.AllowedCredentialIDs = allowed

	wa, err := h.webauthnFor(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	cred, err := wa.FinishLogin(user, entry.data, r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "verify: "+err.Error())
		return
	}
	if err := h.shardPasskeys.UpdateSignCount(r.Context(), cred.ID, cred.Authenticator.SignCount); err != nil {
		log.Printf("[admin] warning: shard sign_count update: %v", err)
	}

	// Pick the session TTL: shard.session_max_age overrides the
	// gateway default when set (for kiosk shards that should re-auth
	// daily, say). Falls back to h.sessionMaxAge otherwise.
	ttl := h.sessionMaxAge
	if sh.SessionMaxAge != nil && *sh.SessionMaxAge > 0 {
		ttl = time.Duration(*sh.SessionMaxAge) * time.Second
	}

	token, err := h.sessions.CreatePrincipal(r.Context(), PrincipalTypeShard, sh.ID, sh.OwnerID, ttl)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	h.setSessionCookieTTL(w, token, ttl)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "authenticated",
		"user":           sh.OwnerID,
		"principal_type": PrincipalTypeShard,
		"shard_id":       sh.ID,
		"shard_name":     sh.Name,
	})
}

// logout deletes the session row and clears the cookie.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		_ = h.sessions.Delete(r.Context(), cookie.Value)
	}
	h.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out"})
}

// authStatus returns 200 with the authed user or 401 if the cookie is
// missing/expired. Used by the dashboard to decide whether to render
// the login screen or the app shell.
//
// SHARD-AUTH-SPEC: when the session is a shard session, the response
// also carries principal_type="shard", shard_id, shard_name, and a
// permissions envelope with the panels/books/chat/invoke kill switches
// the frontend uses to hide what's off-limits. The backend still
// enforces these regardless — the payload is purely so the SPA can
// avoid rendering panels the shard can never load.
func (h *Handler) authStatus(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.authenticatedSession(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	// Sessions renew server-side on activity (sliding idle window) —
	// roll the cookie's Max-Age along with the (possibly just
	// renewed) server expiry so the browser doesn't drop the cookie
	// while the session is still alive. The frontend watchdog probes
	// this endpoint periodically, which keeps both in lockstep.
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if remaining := time.Until(sess.ExpiresAt); remaining > 0 {
			h.setSessionCookieTTL(w, cookie.Value, remaining)
		}
	}
	// The auth endpoint is public (not behind authRequired) so the
	// login page can probe it without triggering a 401. Still need
	// to surface role/email here so the frontend knows which panels
	// to render on boot — look them up directly.
	principalType := sess.PrincipalType
	if principalType == "" {
		principalType = PrincipalTypeUser
	}
	resp := map[string]any{
		"authenticated":  true,
		"user":           sess.UserID,
		"principal_type": principalType,
	}
	// Maintenance banner state rides along on the boot probe (and the
	// 90s watchdog re-probe), so the warning shows/hides live — and
	// auto-clears when the primary model recovers — without a
	// dedicated poll.
	if h.maintenance != nil {
		if st := h.maintenance.State(); st.Active {
			resp["maintenance"] = map[string]any{
				"active":  true,
				"reason":  st.Reason,
				"model":   st.Model,
				"message": st.Message,
			}
		}
	}
	if h.users != nil {
		if u, err := h.users.GetUser(r.Context(), sess.UserID); err == nil && u != nil {
			resp["display_name"] = u.DisplayName
			resp["role"] = u.Role
			if u.Email != nil {
				resp["email"] = *u.Email
			}
		}
	}
	if principalType == PrincipalTypeShard && h.shards != nil {
		if sh, err := h.shards.GetShard(r.Context(), sess.PrincipalID); err == nil && sh != nil {
			resp["shard_id"] = sh.ID
			resp["shard_name"] = sh.Name
			perms, err := h.loadShardPermissions(r.Context(), sh.ID, sess.UserID)
			if err == nil && perms != nil {
				envelope := map[string]any{
					"can_chat":   perms.CanChat,
					"can_invoke": perms.CanInvoke,
					"can_admin":  perms.CanAdmin,
				}
				if perms.Books != nil {
					envelope["books"] = perms.Books
				}
				if perms.Panels != nil {
					envelope["panels"] = perms.Panels
				}
				resp["permissions"] = envelope
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─────────────────────────────── helpers ────────────────────────────────────

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string) {
	h.setSessionCookieTTL(w, token, h.sessionMaxAge)
}

// setSessionCookieTTL is the same as setSessionCookie but takes a
// per-call TTL — used for shard sessions whose lifetime can be
// shorter than the gateway default (a kiosk shard configured to
// re-auth daily, for example).
func (h *Handler) setSessionCookieTTL(w http.ResponseWriter, token string, ttl time.Duration) {
	// Kill any stale cookie from the old Path=/console era so the
	// browser doesn't send two cookies with the same name.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/console",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	// Clear both paths to handle migration from /console to /.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/console",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// pendingCookiePath needs to cover both the auth ceremony endpoints
// (/console/api/auth/*) and the shard-passkey enrollment ceremony
// (/console/api/shards/{id}/passkeys/*) — narrowing further would
// drop the cookie before the finish handler can read it. Scoping
// to /console keeps the cookie out of unrelated origins while
// still covering every endpoint that participates in a ceremony.
const pendingCookiePath = "/console"

func (h *Handler) setPendingCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     pendingCookieName,
		Value:    token,
		Path:     pendingCookiePath,
		MaxAge:   int(pendingTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearPendingCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     pendingCookieName,
		Value:    "",
		Path:     pendingCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func toCredentials(stored []StoredCredential) []webauthn.Credential {
	out := make([]webauthn.Credential, len(stored))
	for i, s := range stored {
		out[i] = s.Credential
	}
	return out
}

// usableCredentials drops credentials with an empty ID — the shape a
// corrupt / half-written credential_blob deserializes to. They can't be
// asserted against, and since login is a shared ceremony, leaving one in
// the allow-list can break BeginLogin for every user. Returns the
// filtered slice and how many were dropped.
func usableCredentials(creds []webauthn.Credential) ([]webauthn.Credential, int) {
	usable := creds[:0]
	dropped := 0
	for _, c := range creds {
		if len(c.ID) > 0 {
			usable = append(usable, c)
		} else {
			dropped++
		}
	}
	return usable, dropped
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Log rather than silently swallow: an encode failure here means
	// the client got truncated/garbage JSON, which is exactly the kind
	// of thing that's invisible until someone debugs a broken
	// integration. The header + status are already written, so we
	// can't change the response — logging is all that's left.
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[admin] writeJSON encode error (status=%d): %v", status, err)
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
