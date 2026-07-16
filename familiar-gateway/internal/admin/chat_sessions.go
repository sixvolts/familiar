package admin

// Chat sessions panel (FAMILIAR-CONSOLE-SPEC Phase B acceptance —
// "her own sessions"). Lists the sessions currently tracked by the
// process-local session.Manager — the live in-memory view, not the
// persisted `sessions` table (which stores only the rolling summary
// keyed by session_key and has no direct user_id column to filter
// on).
//
// This is intentionally a snapshot: gateway restart drops the
// manager's map, so the panel resets. Fine for Phase B — the panel
// answers "what's Familiar currently thinking about on this box"
// and that's a legitimate per-process question.
//
// Endpoint:
//
//   GET /console/api/sessions
//
// Per-user scoping inside the handler: non-admin sees only sessions
// where CanonicalID == session user; admin sees all sessions and
// can pass ?user_id=<id> to filter to a specific user.

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/session"
)

// ChatSessionLister is the narrow interface the sessions panel
// consumes. *session.Manager satisfies it directly via its List()
// method. Separate from admin.SessionStore (which manages the
// WebAuthn session-cookie table) — these are conversation sessions,
// not authentication sessions.
type ChatSessionLister interface {
	List() []*session.Session
}

// AttachChatSessionLister wires the process-local session manager
// into the handler so /console/api/sessions can return live data.
// Nil is tolerated — the endpoint returns 503 so the frontend can
// render a "sessions not available" state.
func (h *Handler) AttachChatSessionLister(s ChatSessionLister) { h.chatSessions = s }

// chatSessionDTO is the JSON shape for one row. Keeps to scalar
// fields the frontend table can render without deep shape-matching.
// Turns is the current in-memory turn count (RecentTurns len);
// SummarizedTurns is how many older turns have been folded into the
// rolling summary already.
type chatSessionDTO struct {
	ID              string    `json:"id"`
	ChannelID       string    `json:"channel_id"`
	SenderID        string    `json:"sender_id"`
	Platform        string    `json:"platform,omitempty"`
	CanonicalID     string    `json:"canonical_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	LastActive      time.Time `json:"last_active"`
	Turns           int       `json:"turns"`
	SummarizedTurns int       `json:"summarized_turns"`
	HasSummary      bool      `json:"has_summary"`
}

// listChatSessions serves GET /console/api/sessions. Returns every
// session the caller is entitled to see, sorted by LastActive desc.
// An optional ?user_id=<id> narrows further (admin-only for users
// other than the session user; non-admin's value is silently
// ignored — they're already locked to their own).
func (h *Handler) listChatSessions(w http.ResponseWriter, r *http.Request) {
	if h.chatSessions == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "chat sessions not configured on this deploy")
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}

	// Scoping:
	//   non-admin       → filter to au.UserID (ignore any ?user_id=)
	//   admin, no param → no filter (every session)
	//   admin, param    → filter to the requested user
	var wantUser string
	if au.IsAdmin() {
		wantUser = strings.TrimSpace(r.URL.Query().Get("user_id"))
	} else {
		wantUser = au.UserID
	}

	all := h.chatSessions.List()
	out := make([]chatSessionDTO, 0, len(all))
	for _, s := range all {
		if s == nil {
			continue
		}
		owner := s.UserID() // CanonicalID when resolved, else SenderID
		if wantUser != "" && owner != wantUser {
			continue
		}
		summary, turns := s.Snapshot()
		out = append(out, chatSessionDTO{
			ID:              s.ID,
			ChannelID:       s.ChannelID,
			SenderID:        s.SenderID,
			Platform:        s.Platform(),
			CanonicalID:     s.CanonicalID(),
			CreatedAt:       s.CreatedAt,
			LastActive:      s.LastActive,
			Turns:           turns,
			SummarizedTurns: s.SummarizedCountSnapshot(),
			HasSummary:      strings.TrimSpace(summary) != "",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActive.After(out[j].LastActive)
	})

	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
