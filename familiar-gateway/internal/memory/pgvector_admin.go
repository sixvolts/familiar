package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// BackfillItem is a minimal memory row used by the relationship
// backfill loop. It only carries the columns the sidecar extractor
// actually inspects (ID + content + owner) so a full backfill scan
// does not allocate the entire MemoryRow surface for 750+ rows.
type BackfillItem struct {
	ID       string
	Content  string
	UserID   string
	ScopeTag string
}

// ListForBackfill returns a user's curated memories eligible for the
// relationship backfill, with their scope tags, oldest first. A user
// is required: memories always have an owner, and the old "global
// rows" scan for an empty user matched nothing.
func (s *PgVectorStore) ListForBackfill(ctx context.Context, userID string) ([]BackfillItem, error) {
	if userID == "" {
		return nil, fmt.Errorf("memory: list for backfill: user required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id::text, m.content, COALESCE(m.user_id, ''), COALESCE(m.scope_tag, '')
		FROM memories m
		WHERE NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		  AND COALESCE(m.scope,'') <> 'session'
		  AND COALESCE(m.source_type,'') <> 'conversation'
		  AND m.content <> ''
		  AND m.user_id = $1
		ORDER BY m.created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("memory: list for backfill: %w", err)
	}
	defer rows.Close()
	var out []BackfillItem
	for rows.Next() {
		var it BackfillItem
		if err := rows.Scan(&it.ID, &it.Content, &it.UserID, &it.ScopeTag); err != nil {
			return nil, fmt.Errorf("memory: scan backfill row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MemoryRow is the expanded representation of a memories row used by
// the admin browser. It surfaces every column the operator might want
// to see on a detail page — id, tags, confidence, source_type — that
// the hot-path MemoryResult intentionally hides.
//
// Keeping this separate from MemoryResult means the retrieval
// pipeline's Search signature stays narrow while the management console
// gets a richer view without forcing every caller to ignore half the
// fields.
type MemoryRow struct {
	ID           string
	Content      string
	Scope        string
	UserID       string // empty string when the row's user_id column is NULL
	SourceType   string
	SourceRef    string // provenance: session id / "page:<id>" for wiki facts, "" when unset
	ScopeTag     string // shard:<id> / book:<id> isolation tag, "" = top-level
	Confidence   float64
	Tags         []string
	CreatedAt    time.Time
	LastAccessed time.Time
	Superseded   bool   // true when another row supersedes this one
	Supersedes   string // id of the row THIS row replaced, "" when none
	SupersededBy string // id of the (newest) row replacing this one, "" when live
	HasEmbed     bool
}

// MemoryFilter is the set of narrowing criteria the admin browser
// offers. All fields are optional; zero values mean "no filter on that
// dimension". Substring matches content ILIKE; filtering by scope /
// source_type is exact; UserID has three modes encoded via
// UserIDFilterMode.
type MemoryFilter struct {
	Substring  string
	Scope      string
	SourceType string
	// Kind is the coarse knowledge/chunk split. "" = no filter,
	// "knowledge" = everything except raw conversation chunks (the
	// same set retrieval sees — pgvector.go excludes
	// source_type='conversation'), "chunks" = only the raw chunks.
	Kind             string
	UserID           string // effective when UserIDFilterMode == UserIDFilterExact
	UserIDFilterMode UserIDFilterMode
	IncludeSupersed  bool // true = also return superseded rows (default false)
	// ScopeTag, when set, keeps only rows carrying exactly that
	// scope_tag: a shard's own memory. TopLevelOnly instead drops rows
	// that belong to one of the row owner's isolated shards: the view
	// the owner's assistant has. The chat memory tools set one or the
	// other; the admin browser sets neither and sees everything.
	ScopeTag     string
	TopLevelOnly bool
}

// isolatedRowPredicate is true for a memories row (alias m) that belongs
// to one of its owner's isolated shards. Owner-scoped: shards are only
// unique per (owner_id, scope_tag), so matching the tag alone let one
// user's shard hide another user's rows.
const isolatedRowPredicate = `EXISTS (SELECT 1 FROM shards sh
	WHERE sh.scope_tag = m.scope_tag
	  AND sh.owner_id = m.user_id
	  AND sh.visibility = 'isolated')`

// UserIDFilterMode controls how the filter treats the user_id column.
type UserIDFilterMode int

const (
	// UserIDFilterAny returns rows regardless of their user_id value.
	UserIDFilterAny UserIDFilterMode = iota
	// UserIDFilterGlobal returns only rows with user_id IS NULL.
	UserIDFilterGlobal
	// UserIDFilterExact returns only rows whose user_id equals the
	// filter's UserID field.
	UserIDFilterExact
)

// ErrMemoryNotFound is returned by GetMemory / DeleteMemory when the
// requested id does not exist.
var ErrMemoryNotFound = errors.New("memory: not found")

// ErrDuplicateContent is returned when an edit would give a row the
// same identity (owner, scope and content; see FactHash) as another
// row. The edit is refused rather than silently merging two facts.
var ErrDuplicateContent = errors.New("memory: another memory already says exactly that")

// isUniqueViolation reports a Postgres unique_violation (23505).
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// memoryRowCols is the shared SELECT list for MemoryRow scans.
// superseded_by picks the NEWEST replacing row when several point at
// this one (chained corrections).
const memoryRowCols = `m.id, m.content, COALESCE(m.scope,''), COALESCE(m.user_id,''),
	       COALESCE(m.source_type,''), COALESCE(m.source_ref,''), COALESCE(m.scope_tag,''),
	       COALESCE(m.confidence,0),
	       COALESCE(m.tags, '{}'::text[]),
	       COALESCE(m.created_at, NOW()),
	       COALESCE(m.last_accessed, m.created_at, NOW()),
	       COALESCE(m.supersedes::text, ''),
	       COALESCE((SELECT s.id::text FROM memories s WHERE s.supersedes = m.id
	                  ORDER BY s.created_at DESC LIMIT 1), '') AS superseded_by,
	       (m.embedding IS NOT NULL) AS has_embed`

func scanMemoryRow(sc interface{ Scan(...any) error }) (MemoryRow, error) {
	var r MemoryRow
	var tags pq.StringArray
	err := sc.Scan(&r.ID, &r.Content, &r.Scope, &r.UserID, &r.SourceType,
		&r.SourceRef, &r.ScopeTag, &r.Confidence, &tags, &r.CreatedAt,
		&r.LastAccessed, &r.Supersedes, &r.SupersededBy, &r.HasEmbed)
	if err != nil {
		return r, err
	}
	r.Tags = []string(tags)
	r.Superseded = r.SupersededBy != ""
	return r, nil
}

// ListMemories returns a page of memories matching the filter.
// Results are ordered newest-first so the browser opens on recent
// activity. Use CountMemories with the same filter to compute total
// pages.
func (s *PgVectorStore) ListMemories(ctx context.Context, f MemoryFilter, limit, offset int) ([]MemoryRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	where, args := buildMemoryWhere(f)
	query := `
		SELECT ` + memoryRowCols + `
		FROM memories m
		` + where + `
		ORDER BY m.created_at DESC
		LIMIT $` + itoa(len(args)+1) + ` OFFSET $` + itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: list: %w", err)
	}
	defer rows.Close()

	var out []MemoryRow
	for rows.Next() {
		r, err := scanMemoryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("memory: scan row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountMemories returns the total number of rows matching the filter.
// Mirrors ListMemories' WHERE construction so pagination stays in sync
// with the visible rows.
func (s *PgVectorStore) CountMemories(ctx context.Context, f MemoryFilter) (int, error) {
	where, args := buildMemoryWhere(f)
	query := `SELECT COUNT(*) FROM memories m ` + where
	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("memory: count: %w", err)
	}
	return n, nil
}

// GetMemory looks up one memory by primary key. Returns
// ErrMemoryNotFound when the row is absent.
func (s *PgVectorStore) GetMemory(ctx context.Context, id string) (*MemoryRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+memoryRowCols+`
		FROM memories m
		WHERE m.id = $1::uuid
	`, id)
	r, err := scanMemoryRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMemoryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("memory: get: %w", err)
	}
	return &r, nil
}

// DeleteVersionsSQL deletes row $1 together with every older version
// it replaced (its supersedes ancestry), and detaches any other row
// that pointed at one of them. $2 confines the whole walk to one
// owner; NULL means any owner (the operator's console).
//
// Deleting only the row itself brought back the stale fact it had
// replaced: a row counts as superseded only while another row points
// at it, so once the newest version ("I live in NYC") was gone, nothing
// hid the older one ("I live in Boston") and the next turn recalled it
// as current. Forgetting a fact has to forget its history with it.
//
// The walk stays in the deleted row's scope. A shard's row that
// superseded one of the owner's top-level facts must not take that fact
// with it when the shard forgets its own: the older row in another
// scope is left, and live again (a shard's row should never have hidden
// it). Rows that pointed INTO the deleted set are detached, not
// deleted: a newer version of a middle row stays live. UNION (not UNION ALL) makes
// the walk terminate on a hand-corrupted cycle. The memengine's
// DeleteFact runs the same statement.
const DeleteVersionsSQL = `
	WITH RECURSIVE doomed AS (
		SELECT id, supersedes, scope_tag FROM memories
		 WHERE id = $1::uuid AND ($2::text IS NULL OR user_id = $2::text)
		UNION
		SELECT m.id, m.supersedes, m.scope_tag
		  FROM memories m JOIN doomed d ON m.id = d.supersedes
		 WHERE ($2::text IS NULL OR m.user_id = $2::text)
		   AND m.scope_tag IS NOT DISTINCT FROM d.scope_tag
	),
	detach AS (
		UPDATE memories SET supersedes = NULL
		 WHERE supersedes IN (SELECT id FROM doomed)
		   AND id NOT IN (SELECT id FROM doomed)
	)
	DELETE FROM memories WHERE id IN (SELECT id FROM doomed)`

// DeleteMemory hard-deletes a memory row by ID, along with the older
// versions it replaced (see DeleteVersionsSQL). The admin console
// treats this as a destructive operator-level action — no soft-delete.
// Callers should prefer semantic UPDATE for routine cleanup; this is
// the escape hatch for bad data.
func (s *PgVectorStore) DeleteMemory(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, DeleteVersionsSQL, id, nil)
	if err != nil {
		return fmt.Errorf("memory: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: delete rows affected: %w", err)
	}
	if n == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// DeleteMemoryOwned deletes a memory row, and the older versions it
// replaced, only if it belongs to userID. It returns (true, nil) when a
// row was removed and (false, nil) when the id doesn't exist or is
// owned by someone else — the two are deliberately indistinguishable
// so a caller can't probe another user's memory space by UUID. Strict
// user_id match (no NULL/global rows): the chat-facing forget_fact tool
// must never delete operator-curated global facts. This is the
// owner-scoped counterpart to DeleteMemory, which is unscoped and
// reserved for the admin browser path that gates ownership at the
// handler (loadScopedMemory).
func (s *PgVectorStore) DeleteMemoryOwned(ctx context.Context, id, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	// A non-owning caller matches no row, so it neither deletes nor
	// detaches anything.
	res, err := s.db.ExecContext(ctx, DeleteVersionsSQL, id, userID)
	if err != nil {
		return false, fmt.Errorf("memory: delete owned: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SearchInScope is semantic search confined to one shard's own memory:
// the user's live rows carrying exactly scopeTag. It is the shard-turn
// counterpart of Search, which serves the owner's top-level view and so
// must never answer a shard's memory tools.
func (s *PgVectorStore) SearchInScope(ctx context.Context, vector []float32, limit int, threshold float64, userID, scopeTag string) ([]MemoryResult, error) {
	if len(vector) == 0 || userID == "" || scopeTag == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id::text, m.content, m.scope, 1 - (`+vecDist("m.embedding")+`) AS similarity, m.created_at
		 FROM memories m
		 WHERE m.embedding IS NOT NULL
		   AND 1 - (`+vecDist("m.embedding")+`) > $2
		   AND m.source_type != 'conversation'
		   AND NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		   AND m.user_id = $4
		   AND m.scope_tag = $5
		 ORDER BY `+vecDist("m.embedding")+`
		 LIMIT $3`,
		vectorToString(vector), threshold, limit, userID, scopeTag)
	if err != nil {
		return nil, fmt.Errorf("pgvector search in scope: %w", err)
	}
	defer rows.Close()
	var out []MemoryResult
	for rows.Next() {
		var r MemoryResult
		var scope sql.NullString
		if err := rows.Scan(&r.ID, &r.Content, &scope, &r.Similarity, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("pgvector search in scope scan: %w", err)
		}
		r.Scope = scope.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// InView reports whether memory id is one the chat memory tools may act
// on for userID: owned by the user, and inside the caller's view. An
// empty scopeTag is the trusted view (not in any of the user's isolated
// shards); a non-empty one is that shard's own scope. The tools accept a
// model-supplied id, so this is what stops a shard turn from deleting or
// rewriting the owner's facts, and the owner's assistant from editing an
// isolated shard's private rows.
func (s *PgVectorStore) InView(ctx context.Context, id, userID, scopeTag string) (bool, error) {
	if id == "" || userID == "" {
		return false, nil
	}
	q := `SELECT EXISTS (SELECT 1 FROM memories m
	       WHERE m.id = $1::uuid AND m.user_id = $2 AND `
	args := []any{id, userID}
	if scopeTag != "" {
		q += `m.scope_tag = $3)`
		args = append(args, scopeTag)
	} else {
		q += `NOT ` + isolatedRowPredicate + `)`
	}
	var ok bool
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&ok); err != nil {
		// A malformed id is a model mistake, not an error: treat it as
		// not in view rather than surfacing a SQL error.
		if strings.Contains(err.Error(), "invalid input syntax for type uuid") {
			return false, nil
		}
		return false, fmt.Errorf("memory: in view: %w", err)
	}
	return ok, nil
}

// DeleteMemoriesBySource removes every memory row that matches both
// source_ref AND scope_tag. Used by the wiki knowledge pipeline to
// clear out a page's stale facts before re-ingesting on save (or to
// fully clean up on page delete). Returns the deleted row count;
// zero is fine and not an error.
//
// Goes around the engine — the engine's RAM cache may briefly serve
// the just-deleted rows until its next flush eviction. For wiki
// re-saves the staleness window is tolerable because the next
// CommitFacts immediately writes fresh rows; reads pre-eviction
// see both old and new but don't lose the new data.
func (s *PgVectorStore) DeleteMemoriesBySource(ctx context.Context, sourceType, sourceRef, scopeTag string) (int64, error) {
	// Detach any children pointing at the rows we're about to remove
	// before deleting (self-FK). Without this, a page re-save whose
	// stale facts were superseded by later extraction would 23503 and
	// the wiki clean-replace would fail.
	res, err := s.db.ExecContext(ctx, `
		WITH victims AS (
			SELECT id FROM memories
			 WHERE source_type = $1 AND source_ref = $2 AND scope_tag = $3
		),
		detach AS (
			UPDATE memories SET supersedes = NULL
			 WHERE supersedes IN (SELECT id FROM victims)
		)
		DELETE FROM memories WHERE id IN (SELECT id FROM victims)`,
		sourceType, sourceRef, scopeTag)
	if err != nil {
		return 0, fmt.Errorf("memory: delete by source: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DistinctScopes returns every scope string currently present in the
// table, sorted. Used to populate the admin browser's scope filter
// dropdown without hard-coding the (open-ended) scope enum.
func (s *PgVectorStore) DistinctScopes(ctx context.Context) ([]string, error) {
	return s.distinctColumn(ctx, "scope")
}

// DistinctSourceTypes mirrors DistinctScopes for source_type.
func (s *PgVectorStore) DistinctSourceTypes(ctx context.Context) ([]string, error) {
	return s.distinctColumn(ctx, "source_type")
}

// DistinctUsers returns every non-null user_id currently in the table.
func (s *PgVectorStore) DistinctUsers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT user_id FROM memories
		WHERE user_id IS NOT NULL AND user_id <> ''
		ORDER BY user_id
	`)
	if err != nil {
		return nil, fmt.Errorf("memory: distinct users: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MemoryVersion is one row from the memory_versions audit table. Each
// version records what the memory's content looked like at a given
// point, what triggered the change, and who (or what system) made it.
type MemoryVersion struct {
	ID         string    `json:"id"`
	MemoryID   string    `json:"memory_id"`
	Content    string    `json:"content"`
	Scope      string    `json:"scope,omitempty"`
	SourceType string    `json:"source_type,omitempty"`
	Version    int       `json:"version"`
	ChangedBy  string    `json:"changed_by,omitempty"`
	ChangeType string    `json:"change_type"`
	CreatedAt  time.Time `json:"created_at"`
}

// RecordVersion inserts a new row in memory_versions. The version
// number is auto-derived as MAX(version)+1 for the given memory_id
// so callers don't have to track the sequence themselves.
func (s *PgVectorStore) RecordVersion(ctx context.Context, memoryID, content, scope, sourceType, changedBy, changeType string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_versions (memory_id, content, scope, source_type, version, changed_by, change_type)
		SELECT $1::uuid, $2, $3, $4,
		       COALESCE((SELECT MAX(version) FROM memory_versions WHERE memory_id = $1::uuid), 0) + 1,
		       $5, $6`,
		memoryID, content, scope, sourceType, changedBy, changeType)
	if err != nil {
		return fmt.Errorf("memory: record version: %w", err)
	}
	return nil
}

// ListVersions returns every version for a memory, newest first.
func (s *PgVectorStore) ListVersions(ctx context.Context, memoryID string) ([]MemoryVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, memory_id, content, COALESCE(scope,''), COALESCE(source_type,''),
		       version, COALESCE(changed_by,''), change_type, created_at
		FROM memory_versions
		WHERE memory_id = $1::uuid
		ORDER BY version DESC`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("memory: list versions: %w", err)
	}
	defer rows.Close()
	var out []MemoryVersion
	for rows.Next() {
		var v MemoryVersion
		if err := rows.Scan(&v.ID, &v.MemoryID, &v.Content, &v.Scope, &v.SourceType,
			&v.Version, &v.ChangedBy, &v.ChangeType, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("memory: scan version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpdateMemoryContent patches the content column on a live memory and
// records a version. The caller supplies the new content's embedding;
// nil CLEARS the stored vector rather than silently keeping the old
// one — a stale embedding makes the row retrieve like its former text,
// which is worse than temporarily dropping out of dense search (FTS
// still matches) — and queues the row for re-embedding. Returns
// ErrMemoryNotFound if the row doesn't exist.
func (s *PgVectorStore) UpdateMemoryContent(ctx context.Context, id, newContent, changedBy string, embedding []float32) error {
	// Fetch current state for the version snapshot.
	row, err := s.GetMemory(ctx, id)
	if err != nil {
		return err
	}

	// Record the old content as the pre-change version if this memory
	// has no versions yet (seed version 1).
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_versions WHERE memory_id = $1::uuid`, id).Scan(&count); err != nil {
		return fmt.Errorf("memory: count versions: %w", err)
	}
	if count == 0 {
		if err := s.RecordVersion(ctx, id, row.Content, row.Scope, row.SourceType, "", "created"); err != nil {
			return err
		}
	}

	// Update the live row — content and embedding together, so the
	// vector can never describe text the row no longer holds.
	var vec any
	if len(embedding) > 0 {
		vec = vectorToString(embedding)
	}
	// The content_hash moves with the content: it is the row's dedup
	// identity, and a stale one made the next commit of the OLD text
	// land on this (edited) row while the new text could be stored a
	// second time.
	hash := FactHash(row.UserID, row.ScopeTag, row.SourceType, row.SourceRef, newContent)
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET content = $1, embedding = $2::vector, content_hash = $4, updated_at = NOW() WHERE id = $3::uuid`,
		newContent, vec, id, hash)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateContent
		}
		return fmt.Errorf("memory: update content: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMemoryNotFound
	}
	// No vector for the new text (the embedder was down): queue the row
	// for the re-embed sweep, as a fact written without one is. Only the
	// queue is swept, so without this the edited row had no vector for
	// good and dense search never found it again.
	if vec == nil {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO pending_embeds (memory_id) VALUES ($1::uuid)
			ON CONFLICT (memory_id) DO UPDATE SET attempts = 0`, id); err != nil {
			return fmt.Errorf("memory: queue re-embed: %w", err)
		}
	}

	// Record the new content as the next version.
	return s.RecordVersion(ctx, id, newContent, row.Scope, row.SourceType, changedBy, "updated")
}

// ChainForMemory returns the full supersede chain containing id,
// oldest first: recursive walk down the `supersedes` pointers (what
// this row replaced, transitively) and up (what replaced it). Depth
// is bounded so a hand-corrupted cycle terminates instead of
// spinning the recursion.
func (s *PgVectorStore) ChainForMemory(ctx context.Context, id string) ([]MemoryRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE older AS (
			SELECT m.id, m.supersedes, 0 AS depth FROM memories m WHERE m.id = $1::uuid
			UNION ALL
			SELECT m.id, m.supersedes, o.depth - 1
			  FROM memories m JOIN older o ON o.supersedes = m.id
			 WHERE o.depth > -50
		), newer AS (
			SELECT m.id, 0 AS depth FROM memories m WHERE m.id = $1::uuid
			UNION ALL
			SELECT m.id, n.depth + 1
			  FROM memories m JOIN newer n ON m.supersedes = n.id
			 WHERE n.depth < 50
		), chain AS (
			SELECT id, depth FROM older
			UNION
			SELECT id, depth FROM newer
		)
		SELECT `+memoryRowCols+`
		  FROM memories m JOIN chain c ON m.id = c.id
		 ORDER BY c.depth, m.created_at`, id)
	if err != nil {
		return nil, fmt.Errorf("memory: chain: %w", err)
	}
	defer rows.Close()
	out := make([]MemoryRow, 0)
	for rows.Next() {
		row, err := scanMemoryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("memory: scan chain row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CollapseChain deletes the superseded rows in id's chain (the ones
// another row points at), keeping every live row. The version history
// already carries the lineage; collapse is for pruning a long chain
// once its intermediate states stop mattering. Returns the number of
// rows deleted and a surviving live row's id (id itself if it's live).
//
// A chain can branch: sleep dedup or two concurrent extractions can
// point two newer rows at one older row, so it has several live heads.
// Treating "the last live row" as the only survivor deleted the other
// live heads, and a collapse started from one head left the other's
// pointer at a deleted row (a foreign-key error, 500).
func (s *PgVectorStore) CollapseChain(ctx context.Context, id string) (int64, string, error) {
	chain, err := s.ChainForMemory(ctx, id)
	if err != nil {
		return 0, "", err
	}
	if len(chain) == 0 {
		return 0, "", ErrMemoryNotFound
	}
	tip := ""
	var doomed []string
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Superseded {
			doomed = append(doomed, chain[i].ID)
		} else if tip == "" || chain[i].ID == id {
			tip = chain[i].ID
		}
	}
	if tip == "" || len(doomed) == 0 {
		// Nothing superseded to prune (single live row, or a cycle
		// where every row reads as replaced — leave that for repair).
		return 0, tip, nil
	}
	// One transaction for the whole collapse: the supersedes FK is
	// self-referential, so pointers into the doomed rows must be
	// cleared before they can be deleted, and a failure between the
	// unlink and the deletes must not leave hidden facts resurfaced.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, tip, fmt.Errorf("memory: collapse begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Every row pointing at a doomed row, in the chain or not (a sibling
	// head the walk from here didn't reach).
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories SET supersedes = NULL, updated_at = NOW()
		 WHERE supersedes = ANY($1::uuid[])`, pq.Array(doomed)); err != nil {
		return 0, tip, fmt.Errorf("memory: collapse unlink: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ANY($1::uuid[])`, pq.Array(doomed))
	if err != nil {
		return 0, tip, fmt.Errorf("memory: collapse delete: %w", err)
	}
	deleted, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, tip, fmt.Errorf("memory: collapse commit: %w", err)
	}
	return deleted, tip, nil
}

// HealthStats is the store-health card: the numbers that say whether
// maintenance is keeping up (MEMORY-UI-SPEC Phase C §5).
type HealthStats struct {
	Chunks            int `json:"chunks"`
	OldestChunkDays   int `json:"oldest_chunk_days"`
	MissingEmbeddings int `json:"missing_embeddings"`
	SupersededRows    int `json:"superseded_rows"`
}

// MemoryHealth aggregates one user's store-health counters in a
// single scan: raw chunk volume + age (retention feed), knowledge
// rows dense search can't find (embedding IS NULL), and superseded
// rows awaiting chain collapse.
func (s *PgVectorStore) MemoryHealth(ctx context.Context, userID string) (HealthStats, error) {
	var h HealthStats
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE source_type = 'conversation'),
			COALESCE(MAX(EXTRACT(EPOCH FROM NOW() - created_at) / 86400)
				FILTER (WHERE source_type = 'conversation'), 0)::int,
			COUNT(*) FILTER (WHERE embedding IS NULL
				AND COALESCE(source_type, '') <> 'conversation'),
			COUNT(*) FILTER (WHERE EXISTS (
				SELECT 1 FROM memories s WHERE s.supersedes = memories.id))
		FROM memories
		WHERE user_id = $1`, userID).
		Scan(&h.Chunks, &h.OldestChunkDays, &h.MissingEmbeddings, &h.SupersededRows)
	if err != nil {
		return HealthStats{}, fmt.Errorf("memory: health: %w", err)
	}
	return h, nil
}

func (s *PgVectorStore) distinctColumn(ctx context.Context, col string) ([]string, error) {
	// col is hard-coded via the DistinctScopes/DistinctSourceTypes
	// wrappers above — never interpolated from user input — so a
	// whitelist check is sufficient to keep this query injection-safe.
	switch col {
	case "scope", "source_type":
	default:
		return nil, fmt.Errorf("memory: distinctColumn: illegal column %q", col)
	}
	query := `SELECT DISTINCT ` + col + ` FROM memories WHERE ` + col + ` IS NOT NULL AND ` + col + ` <> '' ORDER BY ` + col
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("memory: distinct %s: %w", col, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// buildMemoryWhere turns a MemoryFilter into a SQL WHERE clause +
// positional args. Returned args are numbered starting at $1. The
// returned clause always starts with a leading space + "WHERE" (or is
// empty when no predicates apply) so callers can concatenate it
// directly into a larger query.
func buildMemoryWhere(f MemoryFilter) (string, []any) {
	var preds []string
	var args []any

	add := func(clause string, val any) {
		args = append(args, val)
		preds = append(preds, strings.ReplaceAll(clause, "$?", "$"+itoa(len(args))))
	}

	if f.Substring != "" {
		add(`m.content ILIKE $?`, "%"+f.Substring+"%")
	}
	if f.Scope != "" {
		add(`m.scope = $?`, f.Scope)
	}
	if f.SourceType != "" {
		add(`m.source_type = $?`, f.SourceType)
	}
	switch f.Kind {
	case "knowledge":
		// Mirror retrieval's semantics: raw conversation chunks are
		// transcript, not knowledge (pgvector.go excludes them too).
		preds = append(preds, `COALESCE(m.source_type,'') <> 'conversation'`)
	case "chunks":
		preds = append(preds, `m.source_type = 'conversation'`)
	}
	switch f.UserIDFilterMode {
	case UserIDFilterGlobal:
		preds = append(preds, `m.user_id IS NULL`)
	case UserIDFilterExact:
		add(`m.user_id = $?`, f.UserID)
	}
	if !f.IncludeSupersed {
		preds = append(preds, `NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)`)
	}
	if f.ScopeTag != "" {
		add(`m.scope_tag = $?`, f.ScopeTag)
	}
	if f.TopLevelOnly {
		preds = append(preds, `NOT `+isolatedRowPredicate)
	}

	if len(preds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(preds, " AND "), args
}

// itoa is a tiny int-to-decimal helper used to build positional
// parameter placeholders ($1, $2, ...). Avoids pulling in strconv
// at the call sites for what is always a single-digit-to-small-int
// conversion.
func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

// ============================================================
// Dashboard aggregates (Phase F)
// ============================================================
//
// These methods power the user-facing dashboard panel. Each method
// takes a userID so scoping is the caller's concern — the handler
// layer is responsible for translating role + ?user_id= override into
// the target userID via the Phase D pattern (see graphScopeFor).
//
// Shard scoping convention: memories written by a shard carry a
// scope_tag of the form "shard:<id>". "Top-level" rows are those with
// scope_tag IS NULL (the user's own, non-shard memory). The dashboard
// defaults to top-level only because shard writes would otherwise
// swamp the feed on shard-heavy users.

// GrowthPoint is one day in the memory-growth sparkline. FactCount
// and EntityCount are snapshots of the user's total rows as of the
// end of that UTC day.
type GrowthPoint struct {
	Date        string `json:"date"` // YYYY-MM-DD (UTC)
	FactCount   int    `json:"fact_count"`
	EntityCount int    `json:"entity_count"`
}

// CountFactsForUser returns the number of non-superseded knowledge rows
// owned by userID (conversation chunks are transcript, not facts: they
// made the header mostly a count of chat turns). includeShards controls whether scope_tag-prefixed
// rows (written by a shard, scope_tag = 'shard:<id>') count. Default
// false keeps the "933 facts" header honest on shard-heavy users.
func (s *PgVectorStore) CountFactsForUser(ctx context.Context, userID string, includeShards bool) (int, error) {
	q := `SELECT COUNT(*) FROM memories m
	      WHERE m.user_id = $1
	        AND COALESCE(m.source_type,'') <> 'conversation'
	        AND NOT EXISTS (SELECT 1 FROM memories sup WHERE sup.supersedes = m.id)`
	if !includeShards {
		q += ` AND m.scope_tag IS NULL`
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("memory: count facts for user: %w", err)
	}
	return n, nil
}

// RecentFactsForUser returns the N newest non-superseded knowledge rows
// for userID (no conversation chunks), ordered created_at DESC. Mirrors ListMemories' return shape
// so the dashboard card and the full memory panel render from the
// same DTO. Limit is clamped to [1, 50] — the dashboard card only
// renders five at a time, but callers may ask for more.
func (s *PgVectorStore) RecentFactsForUser(ctx context.Context, userID string, limit int, includeShards bool) ([]MemoryRow, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	q := `
		SELECT m.id, m.content, COALESCE(m.scope,''), COALESCE(m.user_id,''),
		       COALESCE(m.source_type,''), COALESCE(m.confidence,0),
		       COALESCE(m.tags, '{}'::text[]),
		       COALESCE(m.created_at, NOW()),
		       EXISTS(SELECT 1 FROM memories sup WHERE sup.supersedes = m.id) AS superseded,
		       (m.embedding IS NOT NULL) AS has_embed
		FROM memories m
		WHERE m.user_id = $1
		  AND COALESCE(m.source_type,'') <> 'conversation'
		  AND NOT EXISTS (SELECT 1 FROM memories sup WHERE sup.supersedes = m.id)
	`
	if !includeShards {
		q += ` AND m.scope_tag IS NULL`
	}
	q += ` ORDER BY m.created_at DESC LIMIT $2`

	rows, err := s.db.QueryContext(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("memory: recent facts: %w", err)
	}
	defer rows.Close()

	out := make([]MemoryRow, 0, limit)
	for rows.Next() {
		var r MemoryRow
		var tags pq.StringArray
		if err := rows.Scan(&r.ID, &r.Content, &r.Scope, &r.UserID, &r.SourceType,
			&r.Confidence, &tags, &r.CreatedAt, &r.Superseded, &r.HasEmbed); err != nil {
			return nil, fmt.Errorf("memory: scan recent: %w", err)
		}
		r.Tags = []string(tags)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GrowthSparkline returns a daily snapshot of fact and entity counts
// for the last `days` days. Day boundaries are UTC. Fact count is
// non-superseded top-level knowledge rows (scope_tag IS NULL, no
// conversation chunks) at end-of-day;
// entity count is distinct entities across the user's relationship
// rows. The series always runs from (today-days+1) to today, inclusive,
// with zero-filled rows for days where the user had no data yet.
//
// Computed as one windowed query per table so we do not make 30×2
// round trips. Each day's count is "rows where created_at <= end of
// that day"; for memories we also respect the supersedes chain.
func (s *PgVectorStore) GrowthSparkline(ctx context.Context, userID string, days int) ([]GrowthPoint, error) {
	if days <= 0 {
		days = 30
	}
	if days > 180 {
		days = 180
	}

	// days×1 fact-count series, computed by counting rows with
	// created_at <= end-of-day for every day in the window.
	factQ := `
		WITH day_series AS (
			SELECT generate_series(
				(CURRENT_DATE AT TIME ZONE 'UTC') - make_interval(days => $2 - 1),
				(CURRENT_DATE AT TIME ZONE 'UTC'),
				interval '1 day'
			)::date AS d
		)
		SELECT to_char(d, 'YYYY-MM-DD'),
		       (SELECT COUNT(*) FROM memories m
		        WHERE m.user_id = $1
		          AND m.scope_tag IS NULL
		          AND COALESCE(m.source_type,'') <> 'conversation'
		          AND m.created_at < (d + interval '1 day')
		          AND NOT EXISTS (SELECT 1 FROM memories sup WHERE sup.supersedes = m.id))
		FROM day_series
		ORDER BY d ASC
	`
	rows, err := s.db.QueryContext(ctx, factQ, userID, days)
	if err != nil {
		return nil, fmt.Errorf("memory: growth sparkline facts: %w", err)
	}
	out := make([]GrowthPoint, 0, days)
	for rows.Next() {
		var p GrowthPoint
		if err := rows.Scan(&p.Date, &p.FactCount); err != nil {
			rows.Close()
			return nil, fmt.Errorf("memory: scan sparkline: %w", err)
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Entity count per day — DISTINCT subject + object counted across
	// relationships created on-or-before end-of-day. Runs against the
	// same day series so the two columns align.
	entQ := `
		WITH day_series AS (
			SELECT generate_series(
				(CURRENT_DATE AT TIME ZONE 'UTC') - make_interval(days => $2 - 1),
				(CURRENT_DATE AT TIME ZONE 'UTC'),
				interval '1 day'
			)::date AS d
		)
		SELECT to_char(d, 'YYYY-MM-DD'),
		       (SELECT COUNT(DISTINCT ent) FROM (
		            SELECT subject AS ent FROM relationships
		            WHERE (user_id IS NULL OR user_id = $1)
		              AND created_at < (d + interval '1 day')
		            UNION
		            SELECT object  AS ent FROM relationships
		            WHERE (user_id IS NULL OR user_id = $1)
		              AND created_at < (d + interval '1 day')
		        ) x)
		FROM day_series
		ORDER BY d ASC
	`
	rows2, err := s.db.QueryContext(ctx, entQ, userID, days)
	if err != nil {
		return nil, fmt.Errorf("memory: growth sparkline entities: %w", err)
	}
	defer rows2.Close()
	i := 0
	for rows2.Next() {
		var date string
		var n int
		if err := rows2.Scan(&date, &n); err != nil {
			return nil, fmt.Errorf("memory: scan sparkline entities: %w", err)
		}
		if i < len(out) && out[i].Date == date {
			out[i].EntityCount = n
		}
		i++
	}
	return out, rows2.Err()
}
