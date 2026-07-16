package admin

// Memory graph view (FAMILIAR-CONSOLE-SPEC Phase D).
//
// Three console-API endpoints render the user's entity-relationship
// graph as a force-directed visualization on the frontend:
//
//   GET /console/api/memory/graph?center=<entity>&depth=N&limit=M
//     — subgraph around `center`. Without `center`, returns the
//       induced subgraph on the top-N most-connected entities (the
//       default first-paint).
//
//   GET /console/api/memory/entities?q=<substr>&limit=L
//     — entity-name autocomplete. Returns matches with degree count.
//
//   GET /console/api/memory/relationship/{id}
//     — one edge with its supporting fact (the memories.id row the
//       triple was extracted from). Used when the user clicks an
//       edge to see what backs it.
//
// All three are any-role authRequired; per-user scoping happens
// inside the handler (passing session.UserID into the store query
// limits non-admins to their own + global rows). Admins may pass
// `?user_id=<id>` to view another user's graph.
//
// Out of scope for Phase D (per spec): shard-scoped subgraphs.
// The relationships table doesn't carry a scope_tag column, so
// `?scope=` is silently ignored — adding it requires a migration
// + a write-side change to extractAndCommitFacts.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/familiar/gateway/internal/memory"
)

// GraphStore is the narrow interface the graph endpoints consume.
// *memory.PgRelationshipStore satisfies it directly. Tests can drop
// in a fake without spinning up a DB.
type GraphStore interface {
	GraphAround(ctx context.Context, center, userID string, depth, limit int) ([]memory.GraphEdge, error)
	GraphTop(ctx context.Context, userID string, limit int) ([]memory.GraphEdge, error)
	SearchEntities(ctx context.Context, q, userID string, limit int) ([]memory.EntityMatch, error)
	GetRelationship(ctx context.Context, id, userID string) (*memory.GraphEdge, error)
	ListBySourceFact(ctx context.Context, factID string) ([]memory.GraphEdge, error)
	FactsForEntity(ctx context.Context, entity, userID string, limit int) ([]memory.MemoryRow, error)
	DeleteRelationship(ctx context.Context, id, userID string) error
	DeleteEntity(ctx context.Context, name, userID string) (int64, error)
	// Curation (MEMORY-UI-SPEC Phase C).
	MergeEntities(ctx context.Context, from, to, userID string) (int64, error)
	UpdateRelationship(ctx context.Context, id, userID, predicate string, confidence *float64) (*memory.GraphEdge, error)
	OrphanEdges(ctx context.Context, userID string) (int, error)
	// Dashboard aggregates (Phase F).
	CountEntities(ctx context.Context, userID string) (int, error)
	CountRelationships(ctx context.Context, userID string) (int, error)
	EntityBreakdown(ctx context.Context, userID string) ([]memory.EntityTypeCount, error)
}

// AttachGraphStore wires the relationship store into the handler so
// the /console/api/memory/graph* endpoints can serve live data. Nil
// is tolerated — the endpoints respond with 503 "graph not configured"
// so the frontend renders a "graph disabled on this deploy" state.
func (h *Handler) AttachGraphStore(g GraphStore) { h.graph = g }

// -----------------------------------------------------------------------------
// Wire types
// -----------------------------------------------------------------------------

type nodeDTO struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Degree int    `json:"degree"`
}

type edgeDTO struct {
	ID         string  `json:"id"`
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
}

type graphResponse struct {
	Nodes     []nodeDTO `json:"nodes"`
	Edges     []edgeDTO `json:"edges"`
	Truncated bool      `json:"truncated"`
}

type relationshipDetailDTO struct {
	ID         string         `json:"id"`
	Subject    string         `json:"subject"`
	Predicate  string         `json:"predicate"`
	Object     string         `json:"object"`
	Confidence float64        `json:"confidence"`
	Supporting *supportingDTO `json:"supporting_fact,omitempty"`
}

type supportingDTO struct {
	ID         string  `json:"id"`
	Content    string  `json:"content"`
	Scope      string  `json:"scope,omitempty"`
	SourceType string  `json:"source_type,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

// graphScopeFor resolves the user_id the store should filter by.
// Non-admins always see their own graph; admins default to their own
// but can pass `?user_id=<id>` to view another user's graph (matches
// the same pattern the spec describes for shards).
func graphScopeFor(r *http.Request) string {
	au, ok := AuthUserFrom(r.Context())
	if !ok {
		return ""
	}
	return adminUserScope(r, au)
}

// listGraph serves GET /console/api/memory/graph. Branches on the
// presence of `?center=`:
//
//   - center present: subgraph around that entity, depth-limited
//   - center absent:  induced subgraph on the top-N densest entities
//
// Both shapes return the same {nodes, edges, truncated} envelope so
// the frontend doesn't have to switch on response shape.
func (h *Handler) listGraph(w http.ResponseWriter, r *http.Request) {
	if h.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "graph not configured on this deploy")
		return
	}
	q := r.URL.Query()
	center := strings.TrimSpace(q.Get("center"))

	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	depth := 2
	if v := q.Get("depth"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			depth = n
		}
	}

	ownerID := graphScopeFor(r)

	var (
		edges []memory.GraphEdge
		err   error
	)
	if center != "" {
		// Allow up to limit*5 edges around a center so a popular
		// node doesn't get its outgoing edges cut off.
		edges, err = h.graph.GraphAround(r.Context(), center, ownerID, depth, limit*5)
	} else {
		edges, err = h.graph.GraphTop(r.Context(), ownerID, limit)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, edgesToGraphResponse(edges))
}

// edgesToGraphResponse derives nodes from edges and computes per-node
// degree as the count of edges touching it within the returned set.
// Sorting nodes by degree desc then name keeps the response stable
// across calls so the cytoscape layout doesn't reshuffle on refresh.
func edgesToGraphResponse(edges []memory.GraphEdge) graphResponse {
	nodeIdx := make(map[string]int) // entity → degree count
	for _, e := range edges {
		nodeIdx[e.Subject]++
		nodeIdx[e.Object]++
	}
	nodes := make([]nodeDTO, 0, len(nodeIdx))
	for name, deg := range nodeIdx {
		nodes = append(nodes, nodeDTO{
			ID:     name,
			Label:  name,
			Degree: deg,
		})
	}
	// Stable order: degree desc, then alphabetical.
	sortNodesByDegree(nodes)

	out := graphResponse{
		Nodes: nodes,
		Edges: make([]edgeDTO, 0, len(edges)),
	}
	for _, e := range edges {
		out.Edges = append(out.Edges, edgeDTO{
			ID:         e.ID,
			Source:     e.Subject,
			Target:     e.Object,
			Label:      e.Predicate,
			Confidence: e.Confidence,
		})
	}
	return out
}

func sortNodesByDegree(nodes []nodeDTO) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0; j-- {
			a, b := nodes[j-1], nodes[j]
			if a.Degree < b.Degree || (a.Degree == b.Degree && a.ID > b.ID) {
				nodes[j-1], nodes[j] = b, a
			} else {
				break
			}
		}
	}
}

// listEntities serves GET /console/api/memory/entities?q=...&limit=...
// Returns the top matching entity names with degree count so the
// frontend's autocomplete can rank by relevance. Without q it's the
// global top-by-degree list (mobile Memory screen).
func (h *Handler) listEntities(w http.ResponseWriter, r *http.Request) {
	if h.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "graph not configured on this deploy")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}

	matches, err := h.graph.SearchEntities(r.Context(), q, graphScopeFor(r), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if matches == nil {
		matches = []memory.EntityMatch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": matches})
}

// entityFacts serves GET /console/api/memory/entity/{name}/facts —
// the live memory rows whose extracted triples mention the entity.
// Drives the console entity-detail fact column and the mobile
// entity screen (MEMORY-UI-SPEC Phase B).
func (h *Handler) entityFacts(w http.ResponseWriter, r *http.Request) {
	if h.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "graph not configured on this deploy")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing entity name")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	rows, err := h.graph.FactsForEntity(r.Context(), name, graphScopeFor(r), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]memoryRowDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toMemoryDTO(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// mergeEntity serves POST /console/api/memory/entity/{name}/merge
// with body {"into": "<target>"} — folds {name} into the target
// entity across the caller's rows (Phase C curation).
func (h *Handler) mergeEntity(w http.ResponseWriter, r *http.Request) {
	if h.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "graph not configured on this deploy")
		return
	}
	from := r.PathValue("name")
	var body struct {
		Into string `json:"into"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	to := strings.TrimSpace(body.Into)
	if from == "" || to == "" {
		writeJSONError(w, http.StatusBadRequest, "merge needs a source entity in the path and \"into\" in the body")
		return
	}
	n, err := h.graph.MergeEntities(r.Context(), from, to, graphScopeFor(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "merged", "from": from, "into": to, "rewritten": n})
}

// patchRelationship serves PATCH /console/api/memory/relationship/{id}
// — edits an edge's predicate and/or confidence. A predicate change
// that collides with an existing (subject, predicate) row returns
// 409 rather than silently merging.
func (h *Handler) patchRelationship(w http.ResponseWriter, r *http.Request) {
	if h.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "graph not configured on this deploy")
		return
	}
	id := r.PathValue("id")
	var body struct {
		Predicate  string   `json:"predicate"`
		Confidence *float64 `json:"confidence"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	if strings.TrimSpace(body.Predicate) == "" && body.Confidence == nil {
		writeJSONError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if body.Confidence != nil && (*body.Confidence < 0 || *body.Confidence > 1) {
		writeJSONError(w, http.StatusBadRequest, "confidence must be in [0, 1]")
		return
	}
	edge, err := h.graph.UpdateRelationship(r.Context(), id, graphScopeFor(r), body.Predicate, body.Confidence)
	if errors.Is(err, memory.ErrDuplicateRelationship) {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if edge == nil {
		writeJSONError(w, http.StatusNotFound, "relationship not found")
		return
	}
	writeJSON(w, http.StatusOK, edge)
}

// getRelationship serves GET /console/api/memory/relationship/{id}.
// Fetches one edge by id (404 if not visible to the caller) and
// joins to the memories table when source_fact is set so the
// supporting-fact content is included for the right-sidebar render.
func (h *Handler) getRelationship(w http.ResponseWriter, r *http.Request) {
	if h.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "graph not configured on this deploy")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	scope := graphScopeFor(r)
	edge, err := h.graph.GetRelationship(r.Context(), id, scope)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if edge == nil {
		writeJSONError(w, http.StatusNotFound, "relationship not found")
		return
	}

	out := relationshipDetailDTO{
		ID:         edge.ID,
		Subject:    edge.Subject,
		Predicate:  edge.Predicate,
		Object:     edge.Object,
		Confidence: edge.Confidence,
	}

	// Supporting fact is best-effort. If the source_fact column was
	// never populated (older triples), or the fact has been deleted,
	// or the memory browser isn't wired, just omit it — the frontend
	// renders without that section instead of 500ing.
	//
	// Visibility-gated like FactsForEntity: a GLOBAL edge is visible
	// to everyone, but if its provenance points at another user's
	// memory, that memory's content must not ride along.
	if edge.SourceFact != "" && h.memoryBrowser != nil {
		row, err := h.memoryBrowser.GetMemory(r.Context(), edge.SourceFact)
		if err == nil && row != nil && (row.UserID == "" || row.UserID == scope) {
			out.Supporting = &supportingDTO{
				ID:         edge.SourceFact,
				Content:    row.Content,
				Scope:      row.Scope,
				SourceType: row.SourceType,
				Confidence: row.Confidence,
			}
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// deleteRelationship serves DELETE /console/api/memory/relationship/{id}.
func (h *Handler) deleteRelationship(w http.ResponseWriter, r *http.Request) {
	if h.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "graph not configured on this deploy")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing id")
		return
	}
	if err := h.graph.DeleteRelationship(r.Context(), id, graphScopeFor(r)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// deleteEntity serves DELETE /console/api/memory/entity/{name}.
// Removes all relationships touching the named entity (subject or
// object). Returns the count of deleted relationships.
func (h *Handler) deleteEntity(w http.ResponseWriter, r *http.Request) {
	if h.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "graph not configured on this deploy")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing entity name")
		return
	}
	n, err := h.graph.DeleteEntity(r.Context(), name, graphScopeFor(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "deleted": n})
}

// _ = errors.Is — defensive against future error sentinel checks
// being added to the graph handlers; kept here so refactors don't
// have to chase import.
var _ = errors.Is
