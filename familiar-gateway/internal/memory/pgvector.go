package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/db"
)

// MemoryResult represents a single memory retrieved from pgvector.
type MemoryResult struct {
	ID         string
	Content    string
	Scope      string
	Similarity float64
	CreatedAt  time.Time
	// FusedScore is the Reciprocal Rank Fusion score from HybridSearch
	// (dense + FTS arms combined). Zero on the pure-dense Search path.
	// Used to order the candidate pool before reranking; not a cosine.
	FusedScore float64
	// Embedding is populated on the Search / HybridSearch paths so a
	// caller that needs the full vector (e.g. write-time conflict
	// resolution) avoids an extra DB round-trip. Admin listing and
	// nearest-similarity probes leave it empty.
	Embedding []float32
}

// MemoryStore retrieves relevant memories for a query vector.
//
// userID scopes queries to one canonical user plus globally-shared
// memories: rows whose user_id column is NULL are always visible
// (they're the shared knowledge base), while rows with a non-NULL
// user_id are only returned when that value matches the caller's
// userID. Passing an empty userID means "no user scoping" and returns
// only the global rows — useful for system-level lookups that should
// never leak a user's private facts.
type MemoryStore interface {
	Search(ctx context.Context, vector []float32, limit int, threshold float64, userID string) ([]MemoryResult, error)
	HybridSearch(ctx context.Context, queryText string, vector []float32, limit int, threshold float64, userID string) ([]MemoryResult, error)
	NearestSimilarity(ctx context.Context, vector []float32, scope string, userID string) (float64, bool, error)
	NearestLiveFact(ctx context.Context, vector []float32, userID string) (NearestFact, bool, error)
	Close() error
}

// NearestFact is the top-1 live (non-superseded) fact returned by
// NearestLiveFact, used by the write-time conflict resolver to ask the
// sidecar whether an incoming fact UPDATEs, DUPLICATEs, or CONTRADICTs
// an existing one.
type NearestFact struct {
	ID         string
	Content    string
	Similarity float64
}

// PgVectorStore implements MemoryStore using local PostgreSQL/pgvector.
// The underlying pool is owned by internal/db and shared with sibling
// stores (session, userprofile, identity). Close is a no-op here
// because pool lifetime belongs to main().
type PgVectorStore struct {
	db *db.Pool
}

// NewPgVectorStore wraps a shared *db.Pool in a MemoryStore. The pool
// must already be open and migrated.
func NewPgVectorStore(pool *db.Pool) (*PgVectorStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("memory: nil pool")
	}
	return &PgVectorStore{db: pool}, nil
}

// Search finds memories similar to the given query vector.
// Returns results above the similarity threshold, ordered by relevance.
//
// Shard isolation (FAMILIAR-SHARDS-PHASE1-SPEC): top-level retrieval
// excludes rows whose `scope_tag` belongs to an `isolated` shard. Rows
// with NULL scope_tag are top-level and always visible; rows tagged to
// a `promoted` shard are also visible; only `isolated` scope_tags get
// filtered out. The `NOT EXISTS` subquery is a no-op on deployments
// that predate the shards table (it returns empty, so nothing is
// filtered) — the predicate is safe to run unconditionally.
//
// This method does not surface shard-scoped retrieval (where a shard
// would read only its own scope). That use case is deferred: Phase 1
// shards either skip retrieval entirely (ephemeral) or see their own
// memories via the session buffer without hitting pgvector.
func (s *PgVectorStore) Search(ctx context.Context, vector []float32, limit int, threshold float64, userID string) ([]MemoryResult, error) {
	if len(vector) == 0 {
		return nil, nil
	}

	vecStr := vectorToString(vector)

	// Filter out superseded memories (anything with another row pointing
	// at it via supersedes) so the UPDATE path from the extraction dedup
	// pipeline cleanly removes stale facts from retrieval without
	// physically deleting them.
	//
	// The user_id predicate enforces multi-user isolation: NULL rows are
	// global and always visible; non-NULL rows are returned only when
	// their owner matches userID. An empty userID argument collapses to
	// "global only", which is the safe default when the caller hasn't
	// resolved a canonical identity yet.
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id::text, content, scope, 1 - (embedding <=> $1::vector) AS similarity, created_at, embedding::text
		 FROM memories m
		 WHERE embedding IS NOT NULL
		   AND 1 - (embedding <=> $1::vector) > $2
		   AND source_type != 'conversation'
		   AND NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		   AND (user_id IS NULL OR user_id = $4)
		   AND (m.scope_tag IS NULL
		        OR NOT EXISTS (
		          SELECT 1 FROM shards sh
		          WHERE sh.scope_tag = m.scope_tag
		            AND sh.visibility = 'isolated'
		        ))
		 ORDER BY embedding <=> $1::vector
		 LIMIT $3`,
		vecStr, threshold, limit, userID)
	if err != nil {
		return nil, fmt.Errorf("pgvector search: %w", err)
	}
	defer rows.Close()

	var results []MemoryResult
	for rows.Next() {
		var r MemoryResult
		var scope sql.NullString
		var embText sql.NullString
		if err := rows.Scan(&r.ID, &r.Content, &scope, &r.Similarity, &r.CreatedAt, &embText); err != nil {
			return nil, fmt.Errorf("scanning memory row: %w", err)
		}
		if scope.Valid {
			r.Scope = scope.String
		}
		if embText.Valid {
			// Non-fatal parse: if pgvector returns a text form the
			// parser can't handle, leave Embedding empty. Callers that
			// need it (promote-on-access) skip the fact; retrieval
			// still worked because the server-side similarity was
			// computed from the actual vector.
			if v, perr := parseVector(embText.String); perr == nil {
				r.Embedding = v
			}
		}
		results = append(results, r)
	}

	return results, rows.Err()
}

// rrfK is the Reciprocal Rank Fusion constant. k=60 is the value from
// the original RRF paper and the de-facto default across hybrid-search
// implementations — it damps the contribution of low-ranked hits so a
// document has to rank well in at least one arm to surface.
const rrfK = 60

// HybridSearch fuses dense (pgvector cosine) and sparse (Postgres
// full-text) retrieval via Reciprocal Rank Fusion.
//
// Pure cosine retrieval misses lexical matches — exact tokens, IDs,
// names the embedder smears together — while pure FTS misses semantic
// paraphrase. RRF runs both arms independently, ranks each, and sums
// 1/(k+rank) across arms so a document strong in either arm ranks
// well overall (chat-turn context review §5).
//
// The cosine threshold gates the DENSE arm only: an FTS hit matched
// lexically and shouldn't be dropped for low vector similarity. The
// returned Similarity is the true cosine (computed for every returned
// row so display + downstream thresholds stay meaningful); FusedScore
// carries the RRF value the rows are ordered by.
//
// queryText="" or an all-stopword query collapses the sparse arm to
// empty — RRF degrades gracefully to dense-only. Shard isolation +
// user scoping match Search exactly.
func (s *PgVectorStore) HybridSearch(ctx context.Context, queryText string, vector []float32, limit int, threshold float64, userID string) ([]MemoryResult, error) {
	if len(vector) == 0 {
		return nil, nil
	}
	vecStr := vectorToString(vector)

	// Each arm pulls a pool of armLimit candidates; RRF fuses them and
	// the outer query trims to limit. A wider arm pool than the final
	// limit gives RRF room to promote a doc that's mid-ranked in both
	// arms over one that's top of a single arm.
	armLimit := limit * 4
	if armLimit < 20 {
		armLimit = 20
	}

	rows, err := s.db.QueryContext(ctx, `
		WITH dense AS (
		    SELECT m.id,
		           row_number() OVER (ORDER BY m.embedding <=> $1::vector) AS rnk
		      FROM memories m
		     WHERE m.embedding IS NOT NULL
		       AND 1 - (m.embedding <=> $1::vector) > $2
		       AND m.source_type != 'conversation'
		       AND NOT EXISTS (SELECT 1 FROM memories sup WHERE sup.supersedes = m.id)
		       AND (m.user_id IS NULL OR m.user_id = $3)
		       AND (m.scope_tag IS NULL
		            OR NOT EXISTS (SELECT 1 FROM shards sh
		                            WHERE sh.scope_tag = m.scope_tag
		                              AND sh.visibility = 'isolated'))
		     ORDER BY m.embedding <=> $1::vector
		     LIMIT $4
		),
		sparse AS (
		    SELECT m.id,
		           row_number() OVER (
		               ORDER BY ts_rank_cd(to_tsvector('english', m.content), q) DESC
		           ) AS rnk
		      FROM memories m, plainto_tsquery('english', $5) q
		     WHERE to_tsvector('english', m.content) @@ q
		       AND m.source_type != 'conversation'
		       AND NOT EXISTS (SELECT 1 FROM memories sup WHERE sup.supersedes = m.id)
		       AND (m.user_id IS NULL OR m.user_id = $3)
		       AND (m.scope_tag IS NULL
		            OR NOT EXISTS (SELECT 1 FROM shards sh
		                            WHERE sh.scope_tag = m.scope_tag
		                              AND sh.visibility = 'isolated'))
		     ORDER BY ts_rank_cd(to_tsvector('english', m.content), q) DESC
		     LIMIT $4
		),
		fused AS (
		    SELECT COALESCE(d.id, sp.id) AS id,
		           COALESCE(1.0 / ($6 + d.rnk), 0) +
		           COALESCE(1.0 / ($6 + sp.rnk), 0) AS rrf
		      FROM dense d
		      FULL OUTER JOIN sparse sp ON d.id = sp.id
		)
		SELECT m.id::text, m.content, m.scope,
		       -- COALESCE to 0: a sparse-only (FTS) hit can have a NULL
		       -- embedding (embedder was down when the row was written),
		       -- for which 1 - (NULL <=> vec) is NULL. Scanning that into
		       -- a float64 would fail the ENTIRE query, so one such row
		       -- would break hybrid retrieval for every lexically-matching
		       -- query. Cosine is undefined here anyway; the RRF fused
		       -- score still ranks it.
		       COALESCE(1 - (m.embedding <=> $1::vector), 0) AS similarity,
		       f.rrf,
		       m.created_at, m.embedding::text
		  FROM fused f
		  JOIN memories m ON m.id = f.id
		 ORDER BY f.rrf DESC
		 LIMIT $7`,
		vecStr, threshold, userID, armLimit, queryText, rrfK, limit)
	if err != nil {
		return nil, fmt.Errorf("pgvector hybrid search: %w", err)
	}
	defer rows.Close()

	var results []MemoryResult
	for rows.Next() {
		var r MemoryResult
		var scope sql.NullString
		var embText sql.NullString
		if err := rows.Scan(&r.ID, &r.Content, &scope, &r.Similarity, &r.FusedScore, &r.CreatedAt, &embText); err != nil {
			return nil, fmt.Errorf("scanning hybrid memory row: %w", err)
		}
		if scope.Valid {
			r.Scope = scope.String
		}
		if embText.Valid {
			if v, perr := parseVector(embText.String); perr == nil {
				r.Embedding = v
			}
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// parseVector decodes pgvector's text-form vector ("[1.0,2.0,...]") into
// a []float32. pgvector prints vectors in this form via ::text cast, so
// we don't need a dedicated Go driver. Returns an error on malformed
// input so callers can distinguish "embedding unavailable" from "empty
// vector."
func parseVector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("parseVector: expected [..], got %q", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return []float32{}, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("parseVector: bad float %q: %w", p, err)
		}
		out = append(out, float32(f))
	}
	return out, nil
}

// NearestSimilarity returns the cosine similarity of the single most
// similar live memory (non-superseded) to the query vector, scoped to
// an optional scope filter. Returns (0, false, nil) when the store is
// empty or no live candidates match. Used by the extraction pipeline to
// NOOP-skip facts that duplicate something already in memory.
func (s *PgVectorStore) NearestSimilarity(ctx context.Context, vector []float32, scope string, userID string) (float64, bool, error) {
	if len(vector) == 0 {
		return 0, false, nil
	}
	vecStr := vectorToString(vector)

	// user_id predicate mirrors Search: global rows + the caller's own.
	// The scope filter is orthogonal and optional.
	var query string
	var args []any
	if scope != "" {
		query = `SELECT 1 - (embedding <=> $1::vector) AS similarity
		         FROM memories m
		         WHERE embedding IS NOT NULL
		           AND source_type NOT IN ('conversation', 'wiki_page')
		           AND scope = $2
		           AND NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		           AND (user_id IS NULL OR user_id = $3)
		         ORDER BY embedding <=> $1::vector
		         LIMIT 1`
		args = []any{vecStr, scope, userID}
	} else {
		query = `SELECT 1 - (embedding <=> $1::vector) AS similarity
		         FROM memories m
		         WHERE embedding IS NOT NULL
		           AND source_type NOT IN ('conversation', 'wiki_page')
		           AND NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		           AND (user_id IS NULL OR user_id = $2)
		         ORDER BY embedding <=> $1::vector
		         LIMIT 1`
		args = []any{vecStr, userID}
	}

	var sim float64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&sim)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("pgvector nearest: %w", err)
	}
	return sim, true, nil
}

// NearestLiveFact returns the single most similar non-superseded memory
// to the query vector, including its id and content. Used by the
// extraction pipeline's write-time conflict resolver, which needs the
// existing row's content (to prompt the classifier) and id (to set
// supersedes on an UPDATE). Scope is intentionally unfiltered: a
// contradiction from a different scope still matters. source_type is
// filtered, though: raw conversation chunks and wiki-owned rows are not
// valid supersede targets — picking a wiki_page row would set a
// supersedes pointer that the page's next clean-replace then trips over
// (23503), and superseding a raw chunk is meaningless. Mirrors the
// retrieval + sleep-dedup exclusions.
func (s *PgVectorStore) NearestLiveFact(ctx context.Context, vector []float32, userID string) (NearestFact, bool, error) {
	if len(vector) == 0 {
		return NearestFact{}, false, nil
	}
	vecStr := vectorToString(vector)

	var nf NearestFact
	err := s.db.QueryRowContext(ctx,
		`SELECT m.id::text, m.content, 1 - (m.embedding <=> $1::vector) AS similarity
		 FROM memories m
		 WHERE m.embedding IS NOT NULL
		   AND m.source_type NOT IN ('conversation', 'wiki_page')
		   AND NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		   AND (m.user_id IS NULL OR m.user_id = $2)
		 ORDER BY m.embedding <=> $1::vector
		 LIMIT 1`,
		vecStr, userID).Scan(&nf.ID, &nf.Content, &nf.Similarity)
	if err == sql.ErrNoRows {
		return NearestFact{}, false, nil
	}
	if err != nil {
		return NearestFact{}, false, fmt.Errorf("pgvector nearest live fact: %w", err)
	}
	return nf, true, nil
}

// Close is a no-op: the *db.Pool is owned by main() and closed there.
func (s *PgVectorStore) Close() error { return nil }

// vectorToString formats a float32 slice as a pgvector literal: "[0.1,0.2,...]"
func vectorToString(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}
