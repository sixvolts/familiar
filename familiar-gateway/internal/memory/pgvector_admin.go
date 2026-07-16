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
	ID      string
	Content string
	UserID  string
}

// ListForBackfill returns every curated memory eligible for the
// relationship extraction backfill: non-superseded, non-session-scope,
// and not a raw conversation snippet. When userID is empty the scan
// returns global rows (user_id IS NULL) only; otherwise it returns
// rows owned by that canonical user. Results are ordered oldest-first
// so resumable runs cover the backlog deterministically.
func (s *PgVectorStore) ListForBackfill(ctx context.Context, userID string) ([]BackfillItem, error) {
	var (
		rows *sql.Rows
		err  error
	)
	base := `
		SELECT m.id::text, m.content, COALESCE(m.user_id, '')
		FROM memories m
		WHERE NOT EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		  AND COALESCE(m.scope,'') <> 'session'
		  AND COALESCE(m.source_type,'') <> 'conversation'
		  AND m.content <> ''
	`
	if userID == "" {
		rows, err = s.db.QueryContext(ctx, base+` AND m.user_id IS NULL ORDER BY m.created_at ASC`)
	} else {
		rows, err = s.db.QueryContext(ctx, base+` AND m.user_id = $1 ORDER BY m.created_at ASC`, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("memory: list for backfill: %w", err)
	}
	defer rows.Close()
	var out []BackfillItem
	for rows.Next() {
		var it BackfillItem
		if err := rows.Scan(&it.ID, &it.Content, &it.UserID); err != nil {
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
	SourceRef    string // provenance: session id / book/page slug, "" when unset
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
}

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

// DeleteMemory hard-deletes a memory row by ID. The admin console
// treats this as a destructive operator-level action — no soft-delete,
// no supersedes chain bookkeeping. Callers should prefer semantic
// UPDATE (via the previous engine's resolve_conflicts pass) for routine
// cleanup; this is the escape hatch for bad data.
func (s *PgVectorStore) DeleteMemory(ctx context.Context, id string) error {
	// Detach children first: the supersedes column is a self-FK, so
	// deleting a row another row points at raises 23503. Deleting a
	// superseded row is exactly the cleanup an operator attempts, so
	// clear the pointers in the same CTE — matching MemEngine.DeleteFact.
	res, err := s.db.ExecContext(ctx, `
		WITH detach AS (
			UPDATE memories SET supersedes = NULL WHERE supersedes = $1::uuid
		)
		DELETE FROM memories WHERE id = $1::uuid`, id)
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

// DeleteMemoryOwned deletes a memory row only if it belongs to userID,
// returning (true, nil) when a row was removed and (false, nil) when
// the id doesn't exist or is owned by someone else — the two are
// deliberately indistinguishable so a caller can't probe another
// user's memory space by UUID. Strict user_id match (no NULL/global
// rows): the chat-facing forget_fact tool must never delete
// operator-curated global facts. This is the owner-scoped counterpart
// to DeleteMemory, which is unscoped and reserved for the admin
// browser path that gates ownership at the handler (loadScopedMemory).
func (s *PgVectorStore) DeleteMemoryOwned(ctx context.Context, id, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	// Detach children (self-FK) only when the target is actually owned
	// by userID, so a non-owning caller neither deletes nor mutates
	// anything. Same one-round-trip CTE as DeleteMemory.
	res, err := s.db.ExecContext(ctx, `
		WITH detach AS (
			UPDATE memories SET supersedes = NULL
			 WHERE supersedes = $1::uuid
			   AND EXISTS (SELECT 1 FROM memories t WHERE t.id = $1::uuid AND t.user_id = $2)
		)
		DELETE FROM memories WHERE id = $1::uuid AND user_id = $2`, id, userID)
	if err != nil {
		return false, fmt.Errorf("memory: delete owned: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
// still matches). Returns ErrMemoryNotFound if the row doesn't exist.
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
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET content = $1, embedding = $2::vector, updated_at = NOW() WHERE id = $3::uuid`,
		newContent, vec, id)
	if err != nil {
		return fmt.Errorf("memory: update content: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMemoryNotFound
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

// CollapseChain deletes every superseded row in id's chain, keeping
// only the live tip (whose dangling supersedes pointer is cleared).
// The version history already carries the lineage; collapse is for
// pruning a long chain once its intermediate states stop mattering.
// Returns the number of rows deleted and the surviving tip's id.
func (s *PgVectorStore) CollapseChain(ctx context.Context, id string) (int64, string, error) {
	chain, err := s.ChainForMemory(ctx, id)
	if err != nil {
		return 0, "", err
	}
	if len(chain) == 0 {
		return 0, "", ErrMemoryNotFound
	}
	// The tip is the row nothing points at; ChainForMemory orders
	// oldest→newest so scan from the end for determinism if a
	// branchy chain has several live heads.
	tip := ""
	for i := len(chain) - 1; i >= 0; i-- {
		if !chain[i].Superseded {
			tip = chain[i].ID
			break
		}
	}
	if tip == "" || len(chain) == 1 {
		// Nothing superseded to prune (single live row, or a cycle
		// where every row reads as replaced — leave that for repair).
		return 0, tip, nil
	}
	// One transaction for the whole collapse: the supersedes FK is
	// self-referential, so pointers must be cleared across the chain
	// before any member can be deleted, and doing the unlink and the
	// deletes as separate autocommit statements meant a failure between
	// them left every pointer cleared but nothing deleted — resurfacing
	// every previously-hidden superseded fact with the chain structure
	// unrecoverable. Inside a tx the intermediate unlinked-but-present
	// state is never visible and any error rolls the whole thing back.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, tip, fmt.Errorf("memory: collapse begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deleted int64
	for _, row := range chain {
		if _, err := tx.ExecContext(ctx,
			`UPDATE memories SET supersedes = NULL, updated_at = NOW() WHERE id = $1::uuid`, row.ID); err != nil {
			return 0, tip, fmt.Errorf("memory: collapse unlink %s: %w", row.ID, err)
		}
	}
	for _, row := range chain {
		if row.ID == tip {
			continue
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = $1::uuid`, row.ID)
		if err != nil {
			return 0, tip, fmt.Errorf("memory: collapse delete %s: %w", row.ID, err)
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
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

// CountFactsForUser returns the number of non-superseded memory rows
// owned by userID. includeShards controls whether scope_tag-prefixed
// rows (written by a shard, scope_tag = 'shard:<id>') count. Default
// false keeps the "933 facts" header honest on shard-heavy users.
func (s *PgVectorStore) CountFactsForUser(ctx context.Context, userID string, includeShards bool) (int, error) {
	q := `SELECT COUNT(*) FROM memories m
	      WHERE m.user_id = $1
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

// RecentFactsForUser returns the N newest non-superseded memories for
// userID, ordered created_at DESC. Mirrors ListMemories' return shape
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
// non-superseded top-level memories (scope_tag IS NULL) at end-of-day;
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
