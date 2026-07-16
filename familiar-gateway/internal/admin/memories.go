package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/familiar/gateway/internal/memory"
)

// MemoryBrowser is the subset of the memory store used by the admin
// console. Kept in the admin package (not memory) so admin's
// dependencies stay explicit and the pipeline-facing MemoryStore
// interface remains focused on retrieval.
type MemoryBrowser interface {
	ListMemories(ctx context.Context, f memory.MemoryFilter, limit, offset int) ([]memory.MemoryRow, error)
	CountMemories(ctx context.Context, f memory.MemoryFilter) (int, error)
	GetMemory(ctx context.Context, id string) (*memory.MemoryRow, error)
	DeleteMemory(ctx context.Context, id string) error
	UpdateMemoryContent(ctx context.Context, id, newContent, changedBy string, embedding []float32) error
	ListVersions(ctx context.Context, memoryID string) ([]memory.MemoryVersion, error)
	// Curation (MEMORY-UI-SPEC Phase C).
	ChainForMemory(ctx context.Context, id string) ([]memory.MemoryRow, error)
	CollapseChain(ctx context.Context, id string) (int64, string, error)
	MemoryHealth(ctx context.Context, userID string) (memory.HealthStats, error)
	DistinctScopes(ctx context.Context) ([]string, error)
	DistinctSourceTypes(ctx context.Context) ([]string, error)
	DistinctUsers(ctx context.Context) ([]string, error)
	// Dashboard aggregates (Phase F). Each method takes a userID so
	// scoping is resolved at the handler layer (see dashboardScopeFor).
	CountFactsForUser(ctx context.Context, userID string, includeShards bool) (int, error)
	RecentFactsForUser(ctx context.Context, userID string, limit int, includeShards bool) ([]memory.MemoryRow, error)
	GrowthSparkline(ctx context.Context, userID string, days int) ([]memory.GrowthPoint, error)
}

// AttachMemoryBrowser wires a memory browser into the handler. Must
// be called before Mux() for the /admin/api/memories endpoints to be
// served. Nil is tolerated and leaves the handler in a "memory
// browser disabled" state; the frontend hides the section when the
// browser is missing by checking GET /admin/api/memories/facets.
func (h *Handler) AttachMemoryBrowser(mb MemoryBrowser) {
	h.memoryBrowser = mb
}

// AttachMemoryEmbedder wires the embedding function PATCH uses to
// re-embed edited content. Optional — without it, edits clear the
// stored vector (better than keeping one that describes the old
// text; FTS still matches until the next write re-embeds).
func (h *Handler) AttachMemoryEmbedder(fn func(ctx context.Context, text string) ([]float32, error)) {
	h.memoryEmbed = fn
}

// memoryRowDTO is the JSON shape for a single memory in list / detail
// responses. Fields are lowercased camelCase so the JS client can
// consume them without alias maps.
type memoryRowDTO struct {
	ID           string    `json:"id"`
	Content      string    `json:"content"`
	Scope        string    `json:"scope"`
	UserID       string    `json:"user_id"`
	SourceType   string    `json:"source_type"`
	SourceRef    string    `json:"source_ref,omitempty"`
	ScopeTag     string    `json:"scope_tag,omitempty"`
	Confidence   float64   `json:"confidence"`
	Tags         []string  `json:"tags"`
	CreatedAt    time.Time `json:"created_at"`
	LastAccessed time.Time `json:"last_accessed"`
	Superseded   bool      `json:"superseded"`
	Supersedes   string    `json:"supersedes,omitempty"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	HasEmbed     bool      `json:"has_embedding"`
}

func toMemoryDTO(r memory.MemoryRow) memoryRowDTO {
	tags := r.Tags
	if tags == nil {
		tags = []string{}
	}
	return memoryRowDTO{
		ID:           r.ID,
		Content:      r.Content,
		Scope:        r.Scope,
		UserID:       r.UserID,
		SourceType:   r.SourceType,
		SourceRef:    r.SourceRef,
		ScopeTag:     r.ScopeTag,
		Confidence:   r.Confidence,
		Tags:         tags,
		CreatedAt:    r.CreatedAt,
		LastAccessed: r.LastAccessed,
		Superseded:   r.Superseded,
		Supersedes:   r.Supersedes,
		SupersededBy: r.SupersededBy,
		HasEmbed:     r.HasEmbed,
	}
}

// listMemories serves GET /admin/api/memories. Query params:
//
//	q              — content substring (ILIKE)
//	scope          — exact scope filter
//	source_type    — exact source_type filter
//	user           — exact user_id filter OR the literal "global"
//	                 (which maps to user_id IS NULL)
//	include_superseded — "1" to include superseded rows
//	limit          — 1..500, default 50
//	offset         — default 0
func (h *Handler) listMemories(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	f, limit, offset := parseMemoryFilter(r)
	// Scope enforcement, deny-by-default:
	//   no auth user → 401 (authedMux should make this unreachable,
	//                  but an unguarded fall-through here would mean
	//                  the unfiltered store).
	//   role=user    → always own rows, whatever the query said.
	//   admin        → own rows too, unless an explicit ?user=
	//                  (<id> | global | all) asks for the cross-user
	//                  view — that's the admin-section entry point,
	//                  not the personal Memory page.
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	if !au.IsAdmin() || r.URL.Query().Get("user") == "" {
		f.UserIDFilterMode = memory.UserIDFilterExact
		f.UserID = au.UserID
	}
	ctx := r.Context()
	total, err := h.memoryBrowser.CountMemories(ctx, f)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, err := h.memoryBrowser.ListMemories(ctx, f, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]memoryRowDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toMemoryDTO(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// loadScopedMemory fetches a memory row and enforces per-role
// visibility. Returns (row, true) on success. On any failure it
// writes the appropriate HTTP response and returns (_, false) so
// callers can simply `return`. 404 — not 403 — is returned when a
// non-admin asks for a row owned by someone else; leaking existence
// of other users' memories would let an attacker enumerate UUIDs.
func (h *Handler) loadScopedMemory(w http.ResponseWriter, r *http.Request, id string) (*memory.MemoryRow, bool) {
	row, err := h.memoryBrowser.GetMemory(r.Context(), id)
	if errors.Is(err, memory.ErrMemoryNotFound) {
		writeJSONError(w, http.StatusNotFound, "memory not found")
		return nil, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if au, ok := AuthUserFrom(r.Context()); ok && !au.IsAdmin() {
		// User-role: only the owner sees the row. Global rows
		// (user_id empty) are treated as hidden too — they're
		// operator-curated facts that shouldn't be mutated by
		// non-admins via the memory browser.
		if row.UserID != au.UserID {
			writeJSONError(w, http.StatusNotFound, "memory not found")
			return nil, false
		}
	}
	return row, true
}

// getMemory serves GET /admin/api/memories/{id}.
func (h *Handler) getMemory(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	row, ok := h.loadScopedMemory(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toMemoryDTO(*row))
}

// deleteMemory serves DELETE /admin/api/memories/{id}. Hard delete —
// the operator is explicitly destroying the row. No dry-run, no tomb,
// no cascade logic.
func (h *Handler) deleteMemory(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	// Ownership check before delete — returns 404 for non-owners
	// instead of leaking existence via a 403.
	if _, ok := h.loadScopedMemory(w, r, id); !ok {
		return
	}
	err := h.memoryBrowser.DeleteMemory(r.Context(), id)
	if errors.Is(err, memory.ErrMemoryNotFound) {
		writeJSONError(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
}

// patchMemory serves PATCH /admin/api/memories/{id}. Updates the
// content of an existing memory and records a version. The caller
// must be authenticated; the admin's user ID is stamped as changed_by.
func (h *Handler) patchMemory(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	if _, ok := h.loadScopedMemory(w, r, id); !ok {
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Content == "" {
		writeJSONError(w, http.StatusBadRequest, "content must not be empty")
		return
	}
	// Stamp the right actor on the version-history row. Admin edits
	// still get the "admin:" prefix; user-role self-edits get
	// "user:" so the audit trail distinguishes the two cleanly.
	changedBy := "admin"
	if au, ok := AuthUserFrom(r.Context()); ok {
		prefix := "admin"
		if !au.IsAdmin() {
			prefix = "user"
		}
		if au.UserID != "" {
			changedBy = prefix + ":" + au.UserID
		}
	} else if uid, ok := r.Context().Value(ContextKeyUserID).(string); ok && uid != "" {
		changedBy = "admin:" + uid
	}
	// Re-embed the edited content so the stored vector matches the
	// new text. Best-effort: on embedder failure (or none attached)
	// the vector is cleared rather than left stale.
	var embedding []float32
	if h.memoryEmbed != nil {
		embCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		if vec, embErr := h.memoryEmbed(embCtx, body.Content); embErr == nil {
			embedding = vec
		}
		cancel()
	}
	err := h.memoryBrowser.UpdateMemoryContent(r.Context(), id, body.Content, changedBy, embedding)
	if errors.Is(err, memory.ErrMemoryNotFound) {
		writeJSONError(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "id": id})
}

// memoryRelationships serves GET /console/api/memories/{id}/relationships:
// the triples whose source_fact provenance points at this memory.
// Replaces the browser's old substring-heuristic call to an endpoint
// that never existed.
func (h *Handler) memoryRelationships(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	if _, ok := h.loadScopedMemory(w, r, id); !ok {
		return
	}
	if h.graph == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	items, err := h.graph.ListBySourceFact(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// memoryChain serves GET /console/api/memories/{id}/chain — the full
// supersede chain containing this memory, oldest first (Phase C).
func (h *Handler) memoryChain(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	if _, ok := h.loadScopedMemory(w, r, id); !ok {
		return
	}
	rows, err := h.memoryBrowser.ChainForMemory(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Defense in depth: no writer creates cross-user supersede
	// pointers, but the chain walk itself doesn't filter by owner —
	// drop any foreign row before it reaches a non-admin.
	if au, ok := AuthUserFrom(r.Context()); ok && !au.IsAdmin() {
		kept := rows[:0]
		for _, row := range rows {
			if row.UserID == au.UserID {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	items := make([]memoryRowDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toMemoryDTO(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// collapseMemoryChain serves POST /console/api/memories/{id}/chain/collapse
// — prunes every superseded row in the chain, keeping the live tip.
func (h *Handler) collapseMemoryChain(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	if _, ok := h.loadScopedMemory(w, r, id); !ok {
		return
	}
	// Collapse deletes every superseded row in the chain — a
	// non-admin must own all of them, not just the entry row.
	if au, ok := AuthUserFrom(r.Context()); ok && !au.IsAdmin() {
		rows, err := h.memoryBrowser.ChainForMemory(r.Context(), id)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, row := range rows {
			if row.UserID != au.UserID {
				writeJSONError(w, http.StatusNotFound, "memory not found")
				return
			}
		}
	}
	deleted, tip, err := h.memoryBrowser.CollapseChain(r.Context(), id)
	if errors.Is(err, memory.ErrMemoryNotFound) {
		writeJSONError(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "collapsed", "deleted": deleted, "tip": tip})
}

// memoryHealth serves GET /console/api/memory/health — the store-
// health card: chunk volume/age, knowledge rows without embeddings,
// superseded rows awaiting collapse, and edges whose provenance
// points at deleted memories. Scoped like the dashboard.
func (h *Handler) memoryHealth(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	targetUser, ok := dashboardScopeFor(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	stats, err := h.memoryBrowser.MemoryHealth(r.Context(), targetUser)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	orphans := 0
	if h.graph != nil {
		if n, err := h.graph.OrphanEdges(r.Context(), targetUser); err == nil {
			orphans = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chunks":             stats.Chunks,
		"oldest_chunk_days":  stats.OldestChunkDays,
		"missing_embeddings": stats.MissingEmbeddings,
		"superseded_rows":    stats.SupersededRows,
		"orphan_edges":       orphans,
	})
}

// memoryVersions serves GET /admin/api/memories/{id}/versions.
// Returns the full version history newest-first so the frontend can
// render a timeline of changes.
func (h *Handler) memoryVersions(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory browser not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	// Version history inherits the parent row's ownership rules —
	// don't leak edit history to non-owners via enumerated UUIDs.
	if _, ok := h.loadScopedMemory(w, r, id); !ok {
		return
	}
	versions, err := h.memoryBrowser.ListVersions(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if versions == nil {
		versions = []memory.MemoryVersion{}
	}
	writeJSON(w, http.StatusOK, versions)
}

// memoryFacets serves GET /admin/api/memories/facets. Returns the
// dropdown values the filter form uses (distinct scopes, source
// types, user IDs) plus a simple "available" flag so the frontend
// knows whether to render the whole section at all.
func (h *Handler) memoryFacets(w http.ResponseWriter, r *http.Request) {
	if h.memoryBrowser == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	ctx := r.Context()
	scopes, err := h.memoryBrowser.DistinctScopes(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sources, err := h.memoryBrowser.DistinctSourceTypes(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The user roster is admin-only — handing every account the
	// full user-ID list was an enumeration leak (the old admin-era
	// dropdown consumed it; the personal page never needed it).
	users := []string{}
	if au.IsAdmin() {
		if list, err := h.memoryBrowser.DistinctUsers(ctx); err == nil {
			users = nilToEmpty(list)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available":    true,
		"scopes":       nilToEmpty(scopes),
		"source_types": nilToEmpty(sources),
		"users":        users,
	})
}

func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func parseMemoryFilter(r *http.Request) (memory.MemoryFilter, int, int) {
	q := r.URL.Query()
	f := memory.MemoryFilter{
		Substring:       q.Get("q"),
		Scope:           q.Get("scope"),
		SourceType:      q.Get("source_type"),
		IncludeSupersed: q.Get("include_superseded") == "1",
	}
	// kind: coarse knowledge/chunk split (unknown values ignored).
	switch k := q.Get("kind"); k {
	case "knowledge", "chunks":
		f.Kind = k
	}
	switch user := q.Get("user"); user {
	case "", "all":
		// "all" is the admin cross-user view's explicit ask; ""
		// falls through to Any here but listMemories re-scopes it
		// to the caller before the query runs.
		f.UserIDFilterMode = memory.UserIDFilterAny
	case "global":
		f.UserIDFilterMode = memory.UserIDFilterGlobal
	default:
		f.UserIDFilterMode = memory.UserIDFilterExact
		f.UserID = user
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	return f, limit, offset
}

// mustJSON is used by tests and tooling; not wired here. Kept as a
// helper so future endpoints can share encoding behaviour.
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
