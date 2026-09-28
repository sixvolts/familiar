package memory

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/safego"
	"github.com/lib/pq"
)

// Relationship is one (subject, predicate, object) triple extracted
// from a conversation. User scoping mirrors memories: a nil UserID
// means "global, visible to everyone"; a non-nil UserID scopes the
// triple to that canonical owner. SourceFact, when set, points at the
// memories.id row the triple was extracted from, so later consolidation
// passes can follow provenance.
type Relationship struct {
	Subject    string
	Predicate  string
	Object     string
	UserID     string // "" = global
	SourceFact string // optional memories.id
	Confidence float64
	ScopeTag   string // optional, e.g. "book:{id}" for wiki-scoped triples
}

// RelationshipStore persists and retrieves entity-relationship triples
// extracted from conversation facts. The interface is deliberately
// narrow: the graph augmentation on the read path needs one-hop
// lookups keyed by retrieved memory contents, and the write path
// needs an upsert that collapses (subject, predicate) pairs to one
// row so IP changes / version bumps replace the previous value
// instead of accumulating contradictory triples — except links_to,
// where a page has one edge per link (ReplacePageLinks keeps the set
// current).
type RelationshipStore interface {
	UpsertRelationships(ctx context.Context, rels []Relationship) error
	RelatedForContents(ctx context.Context, contents []string, userID string, limit int) ([]Relationship, error)
	TraverseFrom(ctx context.Context, entity string, userID string, depth int, limit int) ([]Relationship, error)
}

// PgRelationshipStore is the Postgres-backed implementation.
type PgRelationshipStore struct {
	db *db.Pool
}

// NewPgRelationshipStore wraps a shared pool. The pool must already be
// open and migrated; this store is a no-op wrapper and does not own
// pool lifetime.
func NewPgRelationshipStore(pool *db.Pool) (*PgRelationshipStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("relationships: nil pool")
	}
	return &PgRelationshipStore{db: pool}, nil
}

// UpsertRelationships stores each triple, updating the object (and
// source_fact) on conflict with an existing (subject, predicate,
// user_id) row. Confidence is always taken from the incoming row —
// the sidecar's latest extraction is assumed authoritative.
//
// A write from a different scope may only take over an existing triple
// when it comes from the trusted path (NULL scope). Otherwise a shard
// restating one of the owner's triples rewrote its object and stamped
// the shard's tag on it, and an isolated tag hid it from the owner's
// retrieval. Such a write is dropped; the fact row it came from still
// lands.
//
// Rows with empty subject, predicate, or object are silently dropped
// because the extraction prompt sometimes emits partial triples for
// short turns and forcing the upsert to fail would lose the whole
// batch.
func (s *PgRelationshipStore) UpsertRelationships(ctx context.Context, rels []Relationship) error {
	if len(rels) == 0 {
		return nil
	}
	now := time.Now()
	for _, r := range rels {
		if r.Subject == "" || r.Predicate == "" || r.Object == "" {
			continue
		}
		var userArg any
		if r.UserID != "" {
			userArg = r.UserID
		} else {
			userArg = nil
		}
		var sourceArg any
		if r.SourceFact != "" {
			sourceArg = r.SourceFact
		} else {
			sourceArg = nil
		}
		conf := r.Confidence
		if conf <= 0 {
			conf = 1.0
		}
		var scopeArg any
		if r.ScopeTag != "" {
			scopeArg = r.ScopeTag
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO relationships (subject, predicate, object, user_id, source_fact, confidence, scope_tag, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $8)
			ON CONFLICT `+conflictTarget(r.Predicate)+` DO UPDATE
			SET object      = EXCLUDED.object,
			    source_fact = EXCLUDED.source_fact,
			    confidence  = EXCLUDED.confidence,
			    scope_tag   = EXCLUDED.scope_tag,
			    updated_at  = EXCLUDED.updated_at
			WHERE relationships.scope_tag IS NOT DISTINCT FROM EXCLUDED.scope_tag
			   OR EXCLUDED.scope_tag IS NULL`,
			strings.ToLower(r.Subject), strings.ToLower(r.Predicate), strings.ToLower(r.Object),
			userArg, sourceArg, conf, scopeArg, now)
		if err != nil {
			return fmt.Errorf("upsert relationship (%s, %s): %w", r.Subject, r.Predicate, err)
		}
	}
	return nil
}

// linksTo is the one multi-valued predicate: a page links to many
// pages, one edge each.
const linksTo = "links_to"

// conflictTarget is the ON CONFLICT target matching the unique index
// that covers predicate (relationships_links_multi).
func conflictTarget(predicate string) string {
	if strings.EqualFold(predicate, linksTo) {
		return `(subject, object, user_id_key) WHERE predicate = 'links_to'`
	}
	return `(subject, predicate, user_id_key) WHERE predicate <> 'links_to'`
}

// ReplacePageLinks makes subject's links_to edges in scopeTag exactly
// objects, owned by userID: edges to pages no longer linked are
// deleted (whoever saved them; a page's links are the page's, not a
// member's), and the rest upserted. A page saved with no links keeps
// none.
func (s *PgRelationshipStore) ReplacePageLinks(ctx context.Context, subject, userID, scopeTag string, objects []string) error {
	subject = strings.ToLower(subject)
	lower := make([]string, 0, len(objects))
	for _, o := range objects {
		if o != "" {
			lower = append(lower, strings.ToLower(o))
		}
	}
	var scopeArg any
	if scopeTag != "" {
		scopeArg = scopeTag
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM relationships
		 WHERE subject = $1 AND predicate = 'links_to'
		   AND scope_tag IS NOT DISTINCT FROM $2
		   AND NOT (object = ANY($3::text[]) AND user_id_key = $4)`,
		subject, scopeArg, pq.Array(lower), userID); err != nil {
		return fmt.Errorf("replace links of %s: %w", subject, err)
	}
	rels := make([]Relationship, 0, len(lower))
	for _, o := range lower {
		rels = append(rels, Relationship{Subject: subject, Predicate: linksTo, Object: o, UserID: userID, ScopeTag: scopeTag, Confidence: 1.0})
	}
	return s.UpsertRelationships(ctx, rels)
}

// InsertRelationshipsIfAbsent adds the triples that aren't stored yet
// (by subject, predicate and owner, lowercased like
// UpsertRelationships) and leaves existing ones alone, reporting how
// many it added. The relationship backfill uses it: a re-run
// upserting its extraction reset every edge the user had re-weighted
// or re-pointed in the graph editor.
func (s *PgRelationshipStore) InsertRelationshipsIfAbsent(ctx context.Context, rels []Relationship) (int, error) {
	added := 0
	for _, r := range rels {
		if r.Subject == "" || r.Predicate == "" || r.Object == "" {
			continue
		}
		var userArg, sourceArg, scopeArg any
		if r.UserID != "" {
			userArg = r.UserID
		}
		if r.SourceFact != "" {
			sourceArg = r.SourceFact
		}
		if r.ScopeTag != "" {
			scopeArg = r.ScopeTag
		}
		conf := r.Confidence
		if conf <= 0 {
			conf = 1.0
		}
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO relationships (subject, predicate, object, user_id, source_fact, confidence, scope_tag, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, NOW(), NOW())
			ON CONFLICT `+conflictTarget(r.Predicate)+` DO NOTHING`,
			strings.ToLower(r.Subject), strings.ToLower(r.Predicate), strings.ToLower(r.Object),
			userArg, sourceArg, conf, scopeArg)
		if err != nil {
			return added, fmt.Errorf("insert relationship (%s, %s): %w", r.Subject, r.Predicate, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, nil
}

// RelatedForContents returns triples whose subject appears as a whole
// word (or words) in any content in the provided slice. Used by the
// retrieval path to attach one-hop structured context to the
// vector-search results. Matching is case-insensitive, on word
// boundaries, for subjects of at least 3 characters (the entity
// vocabulary's floor). A plain substring test matched "ai" inside
// "said" and "art" inside "start", filling every prompt's graph budget
// with unrelated triples.
//
// limit caps the number of triples returned; callers should use a
// small value (10-20) because the block is injected into the LLM
// prompt and competes with the memory zone budget.
func (s *PgRelationshipStore) RelatedForContents(ctx context.Context, contents []string, userID string, limit int) ([]Relationship, error) {
	if len(contents) == 0 || limit <= 0 {
		return nil, nil
	}

	// Flatten all content strings into one lowercased haystack — the
	// unnest-based variants race against Postgres's string_agg
	// treatment of NULL, and the single-haystack approach gives us
	// one parameter binding for any number of memories.
	haystack := strings.ToLower(strings.Join(contents, "\n"))

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.subject, r.predicate, r.object
		FROM relationships r
		WHERE `+recallVisible("r", "$1")+`
		  AND length(r.subject) >= 3
		  AND $2 ~* ('\m' || regexp_replace(r.subject, '([^[:alnum:]_ ])', '\\\1', 'g') || '\M')
		ORDER BY r.updated_at DESC
		LIMIT $3`,
		userID, haystack, limit)
	if err != nil {
		return nil, fmt.Errorf("relationships: related query: %w", err)
	}
	defer rows.Close()

	var out []Relationship
	for rows.Next() {
		var r Relationship
		if err := rows.Scan(&r.Subject, &r.Predicate, &r.Object); err != nil {
			return nil, fmt.Errorf("relationships: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TraverseFrom walks the relationship graph outward from the given
// entity and returns every triple reachable within `depth` hops,
// scoped to the user's rows (plus global). Uses a recursive CTE that
// grows a frontier of entity names: at each step the frontier is
// extended with the "other side" of every edge whose subject or
// object is already in it. The final select joins the triples that
// touch any frontier entity, so a depth-2 query starting at "operator"
// returns both operator's direct edges AND the edges of every
// immediate neighbour.
//
// depth is clamped to [1, 3] — a 3-hop traversal already fans out
// hundreds of rows on a modest graph and anything deeper is unsafe
// to put on the inference hot path. limit caps the final row count
// so one dense entity can't blow the prompt budget.
func (s *PgRelationshipStore) TraverseFrom(ctx context.Context, entity string, userID string, depth int, limit int) ([]Relationship, error) {
	entity = strings.ToLower(strings.TrimSpace(entity))
	if entity == "" {
		return nil, nil
	}
	if depth < 1 {
		depth = 1
	}
	if depth > 3 {
		depth = 3
	}
	if limit <= 0 {
		limit = 20
	}

	// The CTE uses plain UNION (not UNION ALL) so the de-duplication
	// keeps the frontier bounded even on highly cyclic graphs —
	// Postgres stops recursing once a step adds no new rows.
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE frontier(entity, depth) AS (
			SELECT $1::text, 0
			UNION
			SELECT CASE WHEN r.subject = f.entity THEN r.object ELSE r.subject END,
			       f.depth + 1
			FROM relationships r
			JOIN frontier f ON (r.subject = f.entity OR r.object = f.entity)
			WHERE f.depth < $3
			  AND `+recallVisible("r", "$2")+`
		)
		SELECT DISTINCT r.subject, r.predicate, r.object
		FROM relationships r
		JOIN frontier f ON (r.subject = f.entity OR r.object = f.entity)
		WHERE `+recallVisible("r", "$2")+`
		ORDER BY r.subject, r.predicate
		LIMIT $4`,
		entity, userID, depth, limit)
	if err != nil {
		return nil, fmt.Errorf("relationships: traverse query: %w", err)
	}
	defer rows.Close()

	var out []Relationship
	for rows.Next() {
		var r Relationship
		if err := rows.Scan(&r.Subject, &r.Predicate, &r.Object); err != nil {
			return nil, fmt.Errorf("relationships: traverse scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListDistinctEntities returns every distinct lowercase entity name
// that appears as either a subject or an object in the user's triples,
// along with its reference count. Used by the entity-resolution pass to
// decide which fragmented names should collapse into a single canonical
// one. A userID of "" walks the global rows only; a non-empty userID
// walks both global rows and that user's rows so the resolver sees
// everything the read path would see.
func (s *PgRelationshipStore) ListDistinctEntities(ctx context.Context, userID string) (map[string]int, error) {
	var rows *sql.Rows
	var err error
	if userID == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT ent, COUNT(*) FROM (
				SELECT subject AS ent FROM relationships WHERE user_id IS NULL
				UNION ALL
				SELECT object  AS ent FROM relationships WHERE user_id IS NULL
			) t
			GROUP BY ent`)
	} else {
		// What the user's recall can see (recallVisible): their own and
		// global triples, their books' triples, no isolated shard's.
		rows, err = s.db.QueryContext(ctx, `
			SELECT ent, COUNT(*) FROM (
				SELECT r.subject AS ent FROM relationships r WHERE `+recallVisible("r", "$1")+`
				UNION ALL
				SELECT r.object  AS ent FROM relationships r WHERE `+recallVisible("r", "$1")+`
			) t
			GROUP BY ent`, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("relationships: list distinct entities: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var ent string
		var count int
		if err := rows.Scan(&ent, &count); err != nil {
			return nil, fmt.Errorf("relationships: scan distinct entity: %w", err)
		}
		ent = strings.ToLower(strings.TrimSpace(ent))
		if ent == "" {
			continue
		}
		out[ent] += count
	}
	return out, rows.Err()
}

// EntityVocab caches, per user, every distinct entity name (subject
// or object) in the relationship graph that user's recall can see. The
// pipeline uses it to spot entity mentions inside retrieved memory
// contents without a database round-trip per memory: FindIn returns
// the names that appear as words in the supplied text.
//
// It was one vocabulary, built from the first admin's graph: every
// other user's entities were never found, so their turns got no
// multi-hop graph context.
//
// A user's vocabulary loads in the background the first time it's
// asked for (FindIn doesn't wait: it finds nothing until the load
// lands, as before) and reloads, again in the background, once it's
// older than the refresh interval. At most maxVocabUsers are kept; the
// least recently used goes first.
type EntityVocab struct {
	store    *PgRelationshipStore
	interval time.Duration

	mu    sync.Mutex
	ctx   context.Context // for background loads; Start's
	users map[string]*userVocab

	stopOnce sync.Once
	stopCh   chan struct{}
}

type userVocab struct {
	names    []string // sorted by length DESC so FindIn matches long names first
	loadedAt time.Time
	usedAt   time.Time
	loading  bool
}

const maxVocabUsers = 256

// NewEntityVocab builds a vocab over a store. interval (how long a
// user's vocabulary is used before it reloads) may be zero, in which
// case a sensible default (5 minutes) is applied.
func NewEntityVocab(store *PgRelationshipStore, interval time.Duration) *EntityVocab {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &EntityVocab{
		store:    store,
		interval: interval,
		ctx:      context.Background(),
		users:    map[string]*userVocab{},
		stopCh:   make(chan struct{}),
	}
}

// Start sets the context background loads run under. Loads are lazy,
// per user, so there is nothing to load up front.
func (v *EntityVocab) Start(ctx context.Context) {
	if v == nil || v.store == nil {
		return
	}
	v.mu.Lock()
	v.ctx = ctx
	v.mu.Unlock()
}

// Stop ends background loading: a vocabulary asked for afterwards
// isn't loaded. Safe to call more than once.
func (v *EntityVocab) Stop() {
	if v == nil {
		return
	}
	v.stopOnce.Do(func() { close(v.stopCh) })
}

// Refresh reloads one user's vocabulary synchronously (tests, or after
// a known write).
func (v *EntityVocab) Refresh(ctx context.Context, userID string) error {
	if v == nil || v.store == nil {
		return nil
	}
	ents, err := v.store.ListDistinctEntities(ctx, userID)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(ents))
	for n := range ents {
		// Drop anything shorter than 3 characters — "a", "2", "is"
		// would match inside every memory and produce false-positive
		// traversals. Real entity names are always >=3 chars.
		if len(n) >= 3 {
			names = append(names, n)
		}
	}
	// Longer names first so FindIn picks the most specific match when
	// one entity name is a substring of another ("host-a" vs. "host-a-dev").
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})

	v.mu.Lock()
	defer v.mu.Unlock()
	u := v.entryLocked(userID)
	u.names = names
	u.loadedAt = time.Now()
	u.loading = false
	return nil
}

// entryLocked returns userID's cache entry, making room if needed.
func (v *EntityVocab) entryLocked(userID string) *userVocab {
	u, ok := v.users[userID]
	if !ok {
		if len(v.users) >= maxVocabUsers {
			oldest := ""
			for id, e := range v.users {
				if oldest == "" || e.usedAt.Before(v.users[oldest].usedAt) {
					oldest = id
				}
			}
			delete(v.users, oldest)
		}
		u = &userVocab{}
		v.users[userID] = u
	}
	u.usedAt = time.Now()
	return u
}

// FindIn returns the user's entity names that appear as words in the
// haystack, longest first, from what's cached now; a missing or stale
// vocabulary is (re)loaded in the background. Matching is
// case-insensitive, on word boundaries: "art" isn't in "start".
func (v *EntityVocab) FindIn(userID, haystack string) []string {
	if v == nil || v.store == nil {
		return nil
	}
	v.mu.Lock()
	u := v.entryLocked(userID)
	names := u.names
	if !u.loading && time.Since(u.loadedAt) >= v.interval {
		select {
		case <-v.stopCh:
		default:
			u.loading = true
			go v.load(v.ctx, userID)
		}
	}
	v.mu.Unlock()
	if len(names) == 0 || haystack == "" {
		return nil
	}
	lower := strings.ToLower(haystack)
	var out []string
	for _, n := range names {
		if containsWord(lower, n) {
			out = append(out, n)
		}
	}
	return out
}

func (v *EntityVocab) load(ctx context.Context, userID string) {
	safego.Do("entity-vocab load", func() {
		loadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := v.Refresh(loadCtx, userID); err != nil {
			log.Printf("[entity-vocab] load failed (continuing): %v", err)
			v.mu.Lock()
			if u, ok := v.users[userID]; ok {
				u.loading = false
				u.loadedAt = time.Now() // retry after an interval, not every turn
			}
			v.mu.Unlock()
		}
	})
}

// containsWord reports whether word occurs in s with no letter, digit
// or underscore directly before or after it.
func containsWord(s, word string) bool {
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	for from := 0; ; {
		i := strings.Index(s[from:], word)
		if i < 0 {
			return false
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[i+len(word):])
		if (i == 0 || !isWord(before)) && (i+len(word) == len(s) || !isWord(after)) {
			return true
		}
		from = i + 1
	}
}

// Size reports how many entity names are cached for the user.
func (v *EntityVocab) Size(userID string) int {
	if v == nil {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if u, ok := v.users[userID]; ok {
		return len(u.names)
	}
	return 0
}

// FormatLines renders a slice of relationships grouped by predicate.
// When multiple triples share the same predicate, they are collected
// under a markdown header so the LLM can scan an entire predicate
// class at a glance (e.g. all "has_ip" triples together).
// Returns nil if rels is empty so the caller can skip the section.
func FormatLines(rels []Relationship) []string {
	if len(rels) == 0 {
		return nil
	}

	// Group by predicate, preserving first-seen order.
	type group struct {
		predicate string
		entries   []Relationship
	}
	seen := map[string]int{}
	var groups []group
	for _, r := range rels {
		idx, ok := seen[r.Predicate]
		if !ok {
			idx = len(groups)
			seen[r.Predicate] = idx
			groups = append(groups, group{predicate: r.Predicate})
		}
		groups[idx].entries = append(groups[idx].entries, r)
	}

	// Single-predicate or single-triple: use flat format for brevity.
	if len(groups) == 1 && len(groups[0].entries) <= 2 {
		lines := make([]string, 0, len(rels))
		for _, r := range rels {
			lines = append(lines, fmt.Sprintf("%s -> %s -> %s", r.Subject, r.Predicate, r.Object))
		}
		return lines
	}

	// Grouped format: predicate header + compact subject -> object lines.
	var lines []string
	for _, g := range groups {
		lines = append(lines, fmt.Sprintf("## %s", g.predicate))
		for _, r := range g.entries {
			lines = append(lines, fmt.Sprintf("- %s -> %s", r.Subject, r.Object))
		}
	}
	return lines
}

// ============================================================
// Graph view queries (FAMILIAR-CONSOLE-SPEC Phase D)
// ============================================================
//
// These methods power the read-only memory graph panel in the
// console. They return rows including the relationships.id UUID so
// the frontend can let the user click an edge and see its supporting
// fact. Every method scopes to (user_id IS NULL OR user_id = $1) so
// per-role filtering is identical to the rest of the memory surface.

// GraphEdge is one (subject, predicate, object) triple including its
// row id, confidence, and supporting-fact provenance. A richer
// projection of Relationship for the graph API.
type GraphEdge struct {
	ID         string  `json:"id"`
	Subject    string  `json:"subject"`
	Predicate  string  `json:"predicate"`
	Object     string  `json:"object"`
	Confidence float64 `json:"confidence"`
	SourceFact string  `json:"source_fact,omitempty"`
}

// EntityMatch is one entity name + its degree (count of triples
// touching it). FactCount/LastSeen ride along for the entity-index
// views (console Entities tab, mobile list); the autocomplete
// consumers just ignore them.
type EntityMatch struct {
	Name      string     `json:"name"`
	Degree    int        `json:"degree"`
	FactCount int        `json:"fact_count"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
}

// GraphAround returns every triple within `depth` hops of `center`,
// scoped to userID's rows + global. Used when the caller has picked
// a focal entity and wants its neighborhood. depth is clamped to
// [1, 3] (anything deeper fans out too widely for an interactive
// view); limit caps the total edges returned. Same recursive-CTE
// shape as TraverseFrom but selects the row id + confidence +
// source_fact so the API can render edge provenance on click.
func (s *PgRelationshipStore) GraphAround(ctx context.Context, center, userID string, depth, limit int) ([]GraphEdge, error) {
	center = strings.ToLower(strings.TrimSpace(center))
	if center == "" {
		return nil, nil
	}
	if depth < 1 {
		depth = 1
	}
	if depth > 3 {
		depth = 3
	}
	if limit <= 0 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE frontier(entity, depth) AS (
			SELECT $1::text, 0
			UNION
			SELECT CASE WHEN r.subject = f.entity THEN r.object ELSE r.subject END,
			       f.depth + 1
			FROM relationships r
			JOIN frontier f ON (r.subject = f.entity OR r.object = f.entity)
			WHERE f.depth < $3
			  AND (r.user_id IS NULL OR r.user_id = $2)
		)
		SELECT DISTINCT r.id::text, r.subject, r.predicate, r.object,
		                r.confidence, COALESCE(r.source_fact::text, '')
		FROM relationships r
		JOIN frontier f ON (r.subject = f.entity OR r.object = f.entity)
		WHERE (r.user_id IS NULL OR r.user_id = $2)
		ORDER BY r.confidence DESC NULLS LAST, r.subject, r.predicate
		LIMIT $4`,
		center, userID, depth, limit)
	if err != nil {
		return nil, fmt.Errorf("relationships: graph around: %w", err)
	}
	defer rows.Close()
	return scanGraphEdges(rows)
}

// GraphTop returns the induced subgraph on the `limit` most-connected
// entities — the default first-paint when the user hasn't picked a
// focus. Computes degree as the count of triples touching each
// entity (subject OR object), then SELECTs every row whose subject
// AND object are both in the top-N set. Edges between top entities
// can outnumber the entities themselves; the SQL caps at limit*5 to
// guard against pathologically dense graphs.
func (s *PgRelationshipStore) GraphTop(ctx context.Context, userID string, limit int) ([]GraphEdge, error) {
	if limit <= 0 {
		limit = 50
	}
	edgeCap := limit * 5

	rows, err := s.db.QueryContext(ctx, `
		WITH degrees AS (
			SELECT entity, COUNT(*) AS degree FROM (
				SELECT subject AS entity FROM relationships
				WHERE user_id IS NULL OR user_id = $1
				UNION ALL
				SELECT object AS entity FROM relationships
				WHERE user_id IS NULL OR user_id = $1
			) e
			GROUP BY entity
			ORDER BY degree DESC
			LIMIT $2
		)
		SELECT r.id::text, r.subject, r.predicate, r.object,
		       r.confidence, COALESCE(r.source_fact::text, '')
		FROM relationships r
		WHERE (r.user_id IS NULL OR r.user_id = $1)
		  AND r.subject IN (SELECT entity FROM degrees)
		  AND r.object  IN (SELECT entity FROM degrees)
		ORDER BY r.confidence DESC NULLS LAST
		LIMIT $3`,
		userID, limit, edgeCap)
	if err != nil {
		return nil, fmt.Errorf("relationships: graph top: %w", err)
	}
	defer rows.Close()
	return scanGraphEdges(rows)
}

// SearchEntities returns entity names matching q (substring,
// case-insensitive) along with their degree count, ordered by degree
// descending so the caller can pick the most-connected match. Used
// by the entity-search box in the graph panel — caller types a
// substring, picks a result, the graph re-centers on it. An empty q
// matches everything, i.e. "top entities by degree" — the mobile
// Memory screen's whole list.
func (s *PgRelationshipStore) SearchEntities(ctx context.Context, q, userID string, limit int) ([]EntityMatch, error) {
	q = strings.TrimSpace(q)
	if limit <= 0 {
		limit = 20
	}
	pattern := "%" + strings.ToLower(q) + "%"

	rows, err := s.db.QueryContext(ctx, `
		SELECT ent,
		       COUNT(*)::int                    AS degree,
		       COUNT(DISTINCT source_fact)::int AS fact_count,
		       MAX(created_at)                  AS last_seen
		FROM (
			SELECT subject AS ent, source_fact, created_at FROM relationships
			WHERE (user_id IS NULL OR user_id = $1) AND subject ILIKE $2
			UNION ALL
			SELECT object AS ent, source_fact, created_at FROM relationships
			WHERE (user_id IS NULL OR user_id = $1) AND object ILIKE $2
		) t
		GROUP BY ent
		ORDER BY COUNT(*) DESC, ent
		LIMIT $3`,
		userID, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("relationships: search entities: %w", err)
	}
	defer rows.Close()

	out := make([]EntityMatch, 0)
	for rows.Next() {
		var m EntityMatch
		if err := rows.Scan(&m.Name, &m.Degree, &m.FactCount, &m.LastSeen); err != nil {
			return nil, fmt.Errorf("relationships: scan entity: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FactsForEntity returns the live memory rows whose triples mention
// the entity (as subject or object), newest first. This is the
// entity-detail "what do we know about X" view: provenance runs
// entity → relationships.source_fact → memories. Rows another fact
// supersedes are skipped — the chain's live tip already carries the
// current version. Both hops are visibility-filtered: a global edge
// whose source_fact points at ANOTHER user's memory must not leak
// that memory's content here.
func (s *PgRelationshipStore) FactsForEntity(ctx context.Context, entity, userID string, limit int) ([]MemoryRow, error) {
	entity = strings.ToLower(strings.TrimSpace(entity))
	if entity == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+memoryRowCols+`
		  FROM memories m
		 WHERE m.id IN (
		       SELECT DISTINCT r.source_fact FROM relationships r
		        WHERE r.source_fact IS NOT NULL
		          AND (r.user_id IS NULL OR r.user_id = $2)
		          AND (r.subject = $1 OR r.object = $1)
		 )
		   AND (m.user_id = $2 OR m.user_id IS NULL OR m.user_id = '')
		   AND NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		 ORDER BY m.created_at DESC
		 LIMIT $3`,
		entity, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("relationships: facts for entity: %w", err)
	}
	defer rows.Close()

	out := make([]MemoryRow, 0)
	for rows.Next() {
		row, err := scanMemoryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("relationships: scan entity fact: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListBySourceFact returns the triples whose provenance points at
// one memory row (relationships.source_fact) — the memory-detail
// panel's "triples extracted from this fact" view. Small result sets
// by construction (a batch pass stamps at most a handful of triples
// per fact), so no pagination.
func (s *PgRelationshipStore) ListBySourceFact(ctx context.Context, factID string) ([]GraphEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, subject, predicate, object, confidence, COALESCE(source_fact::text, '')
		  FROM relationships
		 WHERE source_fact = $1::uuid
		 ORDER BY subject, predicate`, factID)
	if err != nil {
		return nil, fmt.Errorf("relationships: by source fact: %w", err)
	}
	defer rows.Close()
	out := []GraphEdge{}
	for rows.Next() {
		var e GraphEdge
		if err := rows.Scan(&e.ID, &e.Subject, &e.Predicate, &e.Object, &e.Confidence, &e.SourceFact); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetRelationship loads one row by id, ownership-filtered. Returns
// (nil, nil) when the row does not exist or belongs to another user
// — the handler surfaces both as 404 to match the existence-hiding
// convention used by the memory and shards endpoints.
func (s *PgRelationshipStore) GetRelationship(ctx context.Context, id, userID string) (*GraphEdge, error) {
	if id == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, subject, predicate, object,
		       confidence, COALESCE(source_fact::text, '')
		FROM relationships
		WHERE id = $1::uuid
		  AND (user_id IS NULL OR user_id = $2)`, id, userID)
	var e GraphEdge
	if err := row.Scan(&e.ID, &e.Subject, &e.Predicate, &e.Object, &e.Confidence, &e.SourceFact); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("relationships: get %s: %w", id, err)
	}
	return &e, nil
}

// scanGraphEdges drains a *sql.Rows whose columns are the standard
// id / subject / predicate / object / confidence / source_fact
// projection. Shared by GraphAround and GraphTop so column ordering
// stays in lockstep.
func scanGraphEdges(rows *sql.Rows) ([]GraphEdge, error) {
	out := make([]GraphEdge, 0)
	for rows.Next() {
		var e GraphEdge
		if err := rows.Scan(&e.ID, &e.Subject, &e.Predicate, &e.Object, &e.Confidence, &e.SourceFact); err != nil {
			return nil, fmt.Errorf("relationships: scan edge: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ============================================================
// Dashboard aggregates (Phase F)
// ============================================================

// EntityTypeCount is one row of the dashboard entity breakdown —
// entities grouped by some coarse type label. The current schema
// doesn't carry an entity_type column (the engine classifies
// entities but the gateway's relationships table only stores the
// edge), so EntityBreakdown below returns a single "entity" bucket.
// When the engine starts persisting entity types, this DTO stays
// and the implementation grows.
type EntityTypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// CountEntities returns the number of distinct entity names touching
// any relationship owned by userID (subjects + objects, deduped).
// Scopes identically to the rest of the graph surface:
// (user_id IS NULL OR user_id = $1).
func (s *PgRelationshipStore) CountEntities(ctx context.Context, userID string) (int, error) {
	q := `
		SELECT COUNT(DISTINCT ent) FROM (
			SELECT subject AS ent FROM relationships
			WHERE user_id IS NULL OR user_id = $1
			UNION
			SELECT object  AS ent FROM relationships
			WHERE user_id IS NULL OR user_id = $1
		) t`
	var n int
	if err := s.db.QueryRowContext(ctx, q, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("relationships: count entities: %w", err)
	}
	return n, nil
}

// CountRelationships returns the total number of relationship rows
// visible to userID (user's own + global).
func (s *PgRelationshipStore) CountRelationships(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relationships WHERE user_id IS NULL OR user_id = $1`,
		userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("relationships: count: %w", err)
	}
	return n, nil
}

// EntityBreakdown returns entities grouped by type. Currently a
// single-bucket stub because the schema doesn't carry entity_type —
// all entities collapse into the "entity" bucket and the count is
// the total distinct entity count. When the engine persists
// EntityType alongside each relationship (or a separate entities
// table), this method shards the count across person / project /
// place / concept / other without the API contract changing.
func (s *PgRelationshipStore) EntityBreakdown(ctx context.Context, userID string) ([]EntityTypeCount, error) {
	total, err := s.CountEntities(ctx, userID)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return []EntityTypeCount{}, nil
	}
	return []EntityTypeCount{{Type: "entity", Count: total}}, nil
}

// DeleteRelationship removes a single relationship by id, scoped to
// the given user. Returns nil if the row doesn't exist (idempotent).
func (s *PgRelationshipStore) DeleteRelationship(ctx context.Context, id, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM relationships WHERE id = $1::uuid AND user_id = $2`,
		id, userID)
	if err != nil {
		return fmt.Errorf("relationships: delete %s: %w", id, err)
	}
	return nil
}

// MergeEntities folds `from` into `to` across one user's rows —
// duplicate-entity cleanup ("drew" vs "operator"). Only user-owned
// rows move (same strict user_id = $1 scope as DeleteEntity; global
// rows are admin-tooling territory). The (subject, predicate,
// user_id_key) unique index makes a blind rewrite collide, so rows
// whose (to, predicate) slot is already taken are dropped in favor
// of the existing row, and self-loops produced by rewriting an edge
// that connected the two entities are deleted. Returns the number of
// rows that now reference `to`.
func (s *PgRelationshipStore) MergeEntities(ctx context.Context, from, to, userID string) (int64, error) {
	from = strings.ToLower(strings.TrimSpace(from))
	to = strings.ToLower(strings.TrimSpace(to))
	if from == "" || to == "" || from == to {
		return 0, fmt.Errorf("relationships: merge needs two distinct entity names")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("relationships: merge begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Drop from-rows whose (to, predicate) slot already exists — the
	// established row wins over the one being folded in.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM relationships r
		 WHERE r.user_id = $1 AND r.subject = $2
		   AND EXISTS (SELECT 1 FROM relationships x
		                WHERE x.subject = $3 AND x.predicate = r.predicate
		                  AND x.user_id_key = r.user_id_key)`,
		userID, from, to); err != nil {
		return 0, fmt.Errorf("relationships: merge dedupe: %w", err)
	}
	resSub, err := tx.ExecContext(ctx, `
		UPDATE relationships SET subject = $3, updated_at = NOW()
		 WHERE user_id = $1 AND subject = $2`, userID, from, to)
	if err != nil {
		return 0, fmt.Errorf("relationships: merge subjects: %w", err)
	}
	resObj, err := tx.ExecContext(ctx, `
		UPDATE relationships SET object = $3, updated_at = NOW()
		 WHERE user_id = $1 AND object = $2`, userID, from, to)
	if err != nil {
		return 0, fmt.Errorf("relationships: merge objects: %w", err)
	}
	// An edge that connected from↔to is now to→to; drop it.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM relationships
		 WHERE user_id = $1 AND subject = $2 AND object = $2`, userID, to); err != nil {
		return 0, fmt.Errorf("relationships: merge self-loops: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("relationships: merge commit: %w", err)
	}
	nSub, _ := resSub.RowsAffected()
	nObj, _ := resObj.RowsAffected()
	return nSub + nObj, nil
}

// ErrDuplicateRelationship marks an UpdateRelationship predicate
// change that collides with the (subject, predicate, user) unique
// index — the handler maps it to 409 instead of a bare 500.
var ErrDuplicateRelationship = fmt.Errorf("relationships: a triple with this subject and predicate already exists")

// UpdateRelationship edits an edge's predicate and/or confidence.
// Empty predicate / nil confidence leave the field untouched. Same
// strict user_id scope as DeleteRelationship — global rows don't
// take console edits. Returns the updated edge, or nil when the row
// isn't visible to this user.
func (s *PgRelationshipStore) UpdateRelationship(ctx context.Context, id, userID, predicate string, confidence *float64) (*GraphEdge, error) {
	predicate = strings.ToLower(strings.TrimSpace(predicate))
	row := s.db.QueryRowContext(ctx, `
		UPDATE relationships
		   SET predicate  = COALESCE(NULLIF($3, ''), predicate),
		       confidence = COALESCE($4, confidence),
		       updated_at = NOW()
		 WHERE id = $1::uuid AND user_id = $2
		 RETURNING id::text, subject, predicate, object, confidence,
		           COALESCE(source_fact::text, '')`,
		id, userID, predicate, confidence)
	var e GraphEdge
	err := row.Scan(&e.ID, &e.Subject, &e.Predicate, &e.Object, &e.Confidence, &e.SourceFact)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, ErrDuplicateRelationship
		}
		return nil, fmt.Errorf("relationships: update %s: %w", id, err)
	}
	return &e, nil
}

// OrphanEdges counts triples whose source_fact provenance points at
// a memory row that no longer exists — the store-health card's
// "edges to nowhere" number.
func (s *PgRelationshipStore) OrphanEdges(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM relationships r
		 WHERE (r.user_id IS NULL OR r.user_id = $1)
		   AND r.source_fact IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.id = r.source_fact)`,
		userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("relationships: orphan edges: %w", err)
	}
	return n, nil
}

// DeleteEntity removes all relationships where the entity name
// appears as either subject or object, scoped to the given user.
// Returns the number of deleted rows. Entity names are lowercase, as
// every other lookup assumes.
func (s *PgRelationshipStore) DeleteEntity(ctx context.Context, name, userID string) (int64, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM relationships WHERE user_id = $1 AND (subject = $2 OR object = $2)`,
		userID, name)
	if err != nil {
		return 0, fmt.Errorf("relationships: delete entity %q: %w", name, err)
	}
	return res.RowsAffected()
}
