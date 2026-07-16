package admin

// Dashboard endpoints (FAMILIAR-DASHBOARD-SPEC Phase F).
//
// Seven JSON endpoints under /console/api/dashboard/* that power the
// user-facing landing page. Each endpoint is role-scoped following
// the Phase D graph pattern:
//
//   • Non-admin: every call is filtered to au.UserID; the
//     ?user_id= query param is silently ignored (NOT 403) so a
//     caller spoofing the param just sees their own data.
//   • Admin, no ?user_id=: caller's own data.
//   • Admin, ?user_id=<id>: viewing that user's data.
//
// The scope helper dashboardScopeFor encapsulates this so every
// handler reads identically.
//
// Endpoints:
//   GET /console/api/dashboard/overview
//   GET /console/api/dashboard/recent_sessions?limit=5
//   GET /console/api/dashboard/recent_writes?limit=5&include_shards=false
//   GET /console/api/dashboard/entity_breakdown
//   GET /console/api/dashboard/shard_summary
//   GET /console/api/dashboard/graph_preview?limit=15
//   GET /console/api/dashboard/growth_sparkline?days=30

import (
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/memory"
)

// dashboardScopeFor returns the userID whose dashboard data the
// caller is entitled to see. Returns (userID, true) on success; on
// missing auth returns ("", false) and the caller should 401.
//
// Mirrors graphScopeFor so admin ?user_id= override + silent
// non-admin ignore behave identically across panels.
func dashboardScopeFor(r *http.Request) (string, bool) {
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		return "", false
	}
	return adminUserScope(r, au), true
}

// ──────────────────────────────────────────────────────────────────
// Overview — header card aggregates
// ──────────────────────────────────────────────────────────────────

type dashboardOverviewDTO struct {
	UserID                string     `json:"user_id"`
	DisplayName           string     `json:"display_name"`
	LastChatAt            *time.Time `json:"last_chat_at"`
	FactCount             int        `json:"fact_count"`
	EntityCount           int        `json:"entity_count"`
	RelationshipCount     int        `json:"relationship_count"`
	SessionCountLive      int        `json:"session_count_live"`
	ShardCount            int        `json:"shard_count"`
	ShardInvocationsToday int        `json:"shard_invocations_today"`
}

// dashboardOverview serves GET /console/api/dashboard/overview.
// Fresh users land here and see zeros + null last_chat_at; the
// frontend renders a "say hi to get started" state in that case.
func (h *Handler) dashboardOverview(w http.ResponseWriter, r *http.Request) {
	targetUser, ok := dashboardScopeFor(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	ctx := r.Context()

	out := dashboardOverviewDTO{UserID: targetUser}

	// Display name — optional: a bare handler without a user manager
	// still returns valid JSON, just without the pretty name.
	if h.users != nil {
		if u, err := h.users.GetUser(ctx, targetUser); err == nil && u != nil {
			out.DisplayName = u.DisplayName
		} else if err != nil {
			// Best-effort: the dashboard still renders without the
			// pretty name, but log so a broken users store is visible
			// to an operator instead of silently dropping the field.
			log.Printf("[admin] dashboard: display-name lookup for %q failed (continuing): %v", targetUser, err)
		}
	}

	if h.memoryBrowser != nil {
		n, err := h.memoryBrowser.CountFactsForUser(ctx, targetUser, false)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "fact count: "+err.Error())
			return
		}
		out.FactCount = n
	}

	if h.graph != nil {
		ents, err := h.graph.CountEntities(ctx, targetUser)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "entity count: "+err.Error())
			return
		}
		out.EntityCount = ents

		rels, err := h.graph.CountRelationships(ctx, targetUser)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "relationship count: "+err.Error())
			return
		}
		out.RelationshipCount = rels
	}

	// Live session count + last_chat_at come from the in-memory session
	// manager snapshot. Count matches the Sessions panel's filter so
	// the two views reconcile.
	if h.chatSessions != nil {
		sessions := h.chatSessions.List()
		var last *time.Time
		live := 0
		for _, s := range sessions {
			if s == nil {
				continue
			}
			if s.UserID() != targetUser {
				continue
			}
			live++
			la := s.LastActive
			if last == nil || la.After(*last) {
				laCp := la
				last = &laCp
			}
		}
		out.SessionCountLive = live
		out.LastChatAt = last
	}

	if h.shards != nil {
		list, err := h.shards.ListShards(ctx, targetUser)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "shard list: "+err.Error())
			return
		}
		out.ShardCount = len(list)
		// shard_invocations_today requires a counter table that doesn't
		// exist yet; leave at zero per spec and fill in when the
		// counters land.
	}

	writeJSON(w, http.StatusOK, out)
}

// ──────────────────────────────────────────────────────────────────
// Recent sessions — last N live sessions
// ──────────────────────────────────────────────────────────────────

type dashboardRecentSessionDTO struct {
	ID         string    `json:"id"`
	Platform   string    `json:"platform,omitempty"`
	ChannelID  string    `json:"channel_id"`
	Turns      int       `json:"turns"`
	LastActive time.Time `json:"last_active"`
}

// dashboardRecentSessions serves GET /console/api/dashboard/recent_sessions.
// Reuses the same in-memory snapshot as the Sessions panel, but
// trims to the N most-recent-by-LastActive sessions and keeps the
// DTO small (no summary flag, no created_at — those live on the
// full panel).
func (h *Handler) dashboardRecentSessions(w http.ResponseWriter, r *http.Request) {
	targetUser, ok := dashboardScopeFor(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	if h.chatSessions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []dashboardRecentSessionDTO{}})
		return
	}
	limit := parseDashboardLimit(r, 5, 25)

	all := h.chatSessions.List()
	out := make([]dashboardRecentSessionDTO, 0, limit)
	for _, s := range all {
		if s == nil || s.UserID() != targetUser {
			continue
		}
		_, turns := s.Snapshot()
		out = append(out, dashboardRecentSessionDTO{
			ID:         s.ID,
			Platform:   s.Platform(),
			ChannelID:  s.ChannelID,
			Turns:      turns,
			LastActive: s.LastActive,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive.After(out[j].LastActive) })
	if len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ──────────────────────────────────────────────────────────────────
// Recent writes — last N facts saved
// ──────────────────────────────────────────────────────────────────

type dashboardRecentWriteDTO struct {
	ID         string    `json:"id"`
	Snippet    string    `json:"snippet"`
	SourceType string    `json:"source_type"`
	Scope      string    `json:"scope,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

const recentWriteSnippetMax = 120

// dashboardRecentWrites serves GET /console/api/dashboard/recent_writes.
// Default excludes shard-scoped writes because they'd otherwise swamp
// the feed on shard-heavy users. Pass ?include_shards=true to see
// everything.
func (h *Handler) dashboardRecentWrites(w http.ResponseWriter, r *http.Request) {
	targetUser, ok := dashboardScopeFor(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	if h.memoryBrowser == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []dashboardRecentWriteDTO{}})
		return
	}
	limit := parseDashboardLimit(r, 5, 25)
	includeShards := r.URL.Query().Get("include_shards") == "true"

	rows, err := h.memoryBrowser.RecentFactsForUser(r.Context(), targetUser, limit, includeShards)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]dashboardRecentWriteDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, dashboardRecentWriteDTO{
			ID:         row.ID,
			Snippet:    truncateSnippet(row.Content, recentWriteSnippetMax),
			SourceType: row.SourceType,
			Scope:      row.Scope,
			CreatedAt:  row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ──────────────────────────────────────────────────────────────────
// Entity breakdown — entities grouped by type
// ──────────────────────────────────────────────────────────────────

// dashboardEntityBreakdown serves GET /console/api/dashboard/entity_breakdown.
// Returns the per-type distribution of entities the user can see.
// Schema note: the relationships table doesn't carry entity types
// yet, so the implementation returns a single "entity" bucket with
// the full count. Contract is stable so the frontend can keep the
// breakdown card shape when typing lands.
func (h *Handler) dashboardEntityBreakdown(w http.ResponseWriter, r *http.Request) {
	targetUser, ok := dashboardScopeFor(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	if h.graph == nil {
		writeJSON(w, http.StatusOK, map[string]any{"breakdown": []memory.EntityTypeCount{}})
		return
	}
	out, err := h.graph.EntityBreakdown(r.Context(), targetUser)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []memory.EntityTypeCount{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"breakdown": out})
}

// ──────────────────────────────────────────────────────────────────
// Shard summary — per-shard activity
// ──────────────────────────────────────────────────────────────────

type dashboardShardDTO struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	InvocationsToday int        `json:"invocations_today"`
	Invocations7d    int        `json:"invocations_7d"`
	InvocationsTotal int        `json:"invocations_total"`
	LastInvocationAt *time.Time `json:"last_invocation_at"`
	Disabled         bool       `json:"disabled"`
}

// dashboardShardSummary serves GET /console/api/dashboard/shard_summary.
// Per-shard row with activity counters. Per spec Phase F note, the
// invocation counters require a `shard_invocations` table that
// doesn't exist yet — this endpoint returns shard metadata with
// zero counters so the frontend can show a "2 shards, activity
// coming soon" card. Counters land when the counter table is
// wired.
func (h *Handler) dashboardShardSummary(w http.ResponseWriter, r *http.Request) {
	targetUser, ok := dashboardScopeFor(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	if h.shards == nil {
		writeJSON(w, http.StatusOK, map[string]any{"shards": []dashboardShardDTO{}})
		return
	}
	list, err := h.shards.ListShards(r.Context(), targetUser)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]dashboardShardDTO, 0, len(list))
	for _, s := range list {
		if s == nil {
			continue
		}
		out = append(out, dashboardShardDTO{
			ID:       s.ID,
			Name:     s.Name,
			Disabled: s.DisabledAt != nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"shards": out})
}

// ──────────────────────────────────────────────────────────────────
// Graph preview — top-N densest subgraph
// ──────────────────────────────────────────────────────────────────

type dashboardGraphNodeDTO struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Degree int    `json:"degree"`
}

type dashboardGraphEdgeDTO struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	Predicate string `json:"predicate"`
}

type dashboardGraphPreviewDTO struct {
	Nodes []dashboardGraphNodeDTO `json:"nodes"`
	Edges []dashboardGraphEdgeDTO `json:"edges"`
}

// dashboardGraphPreview serves GET /console/api/dashboard/graph_preview.
// Top-N densest entities + the edges between them. Reuses
// GraphStore.GraphTop directly — same underlying query as the
// graph panel's first-paint, just with a smaller default limit
// appropriate for a card-sized preview.
func (h *Handler) dashboardGraphPreview(w http.ResponseWriter, r *http.Request) {
	targetUser, ok := dashboardScopeFor(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	if h.graph == nil {
		writeJSON(w, http.StatusOK, dashboardGraphPreviewDTO{
			Nodes: []dashboardGraphNodeDTO{},
			Edges: []dashboardGraphEdgeDTO{},
		})
		return
	}
	limit := parseDashboardLimit(r, 15, 50)

	edges, err := h.graph.GraphTop(r.Context(), targetUser, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toGraphPreview(edges))
}

// toGraphPreview collapses a slice of edges into the nodes + edges
// shape the frontend expects. Degree is the count of edges touching
// each node so the preview can render denser entities larger.
func toGraphPreview(edges []memory.GraphEdge) dashboardGraphPreviewDTO {
	nodes := make(map[string]*dashboardGraphNodeDTO)
	seen := func(name string) *dashboardGraphNodeDTO {
		if n, ok := nodes[name]; ok {
			return n
		}
		n := &dashboardGraphNodeDTO{ID: name, Label: name}
		nodes[name] = n
		return n
	}
	edgeDTOs := make([]dashboardGraphEdgeDTO, 0, len(edges))
	for _, e := range edges {
		seen(e.Subject).Degree++
		seen(e.Object).Degree++
		edgeDTOs = append(edgeDTOs, dashboardGraphEdgeDTO{
			ID:        e.ID,
			Source:    e.Subject,
			Target:    e.Object,
			Predicate: e.Predicate,
		})
	}
	// Stable order for tests and rendering.
	nodeList := make([]dashboardGraphNodeDTO, 0, len(nodes))
	for _, n := range nodes {
		nodeList = append(nodeList, *n)
	}
	sort.Slice(nodeList, func(i, j int) bool {
		if nodeList[i].Degree != nodeList[j].Degree {
			return nodeList[i].Degree > nodeList[j].Degree
		}
		return nodeList[i].ID < nodeList[j].ID
	})
	return dashboardGraphPreviewDTO{Nodes: nodeList, Edges: edgeDTOs}
}

// ──────────────────────────────────────────────────────────────────
// Growth sparkline — daily memory/entity growth
// ──────────────────────────────────────────────────────────────────

type dashboardSparklineDTO struct {
	Series []memory.GrowthPoint `json:"series"`
}

// dashboardGrowthSparkline serves GET /console/api/dashboard/growth_sparkline.
// Default 30-day window, capped at 180. Empty series returned as []
// (not null) so the frontend can unconditionally iterate.
func (h *Handler) dashboardGrowthSparkline(w http.ResponseWriter, r *http.Request) {
	targetUser, ok := dashboardScopeFor(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	if h.memoryBrowser == nil {
		writeJSON(w, http.StatusOK, dashboardSparklineDTO{Series: []memory.GrowthPoint{}})
		return
	}
	days := 30
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	series, err := h.memoryBrowser.GrowthSparkline(r.Context(), targetUser, days)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if series == nil {
		series = []memory.GrowthPoint{}
	}
	writeJSON(w, http.StatusOK, dashboardSparklineDTO{Series: series})
}

// ──────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────

func parseDashboardLimit(r *http.Request, def, max int) int {
	v := strings.TrimSpace(r.URL.Query().Get("limit"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// truncateSnippet returns content truncated at maxRunes runes, appending
// a single trailing ellipsis when truncation happened. Rune-aware so
// multi-byte content doesn't get sliced mid-codepoint.
func truncateSnippet(content string, maxRunes int) string {
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}
	return string(runes[:maxRunes]) + "…"
}
