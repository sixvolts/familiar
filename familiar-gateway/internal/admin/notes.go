package admin

// Workspace notes CRUD + full-text search (FAMILIAR-WORKSPACE-SPEC
// Phase 2a). Backs the Notes panel + the notes skill, both reading
// the same Postgres-backed table. Per-user scoping mirrors
// conversations: non-admins see their own notes, admins can pass
// ?user_id=<id>. Per-resource authz at GET/PATCH/DELETE returns 404
// (not 403) on non-owner so a non-admin can't probe for other
// users' note IDs.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/db"
)

// ──────────────────────────────────────────────────────────────────
// Types
// ──────────────────────────────────────────────────────────────────

// Note is one markdown document. Folder is a free-text grouping
// label (no enforced hierarchy — flat or "Inbox" / "Reference"
// labels both work). DeletedAt is the soft-delete marker; the
// retention-cron sweeps these later.
type Note struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Title     string     `json:"title"`
	Content   string     `json:"content"`
	Folder    string     `json:"folder,omitempty"`
	Pinned    bool       `json:"pinned"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// NoteSummary is a list-view row — drops `content` so the workspace
// can render the file tree without dragging huge bodies across the
// wire. The notes panel asks for the full row only when the user
// opens a specific note.
type NoteSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Folder    string    `json:"folder,omitempty"`
	Pinned    bool      `json:"pinned"`
	Snippet   string    `json:"snippet,omitempty"` // first ~120 chars of body
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrNoteNotFound is returned by the store + handlers when an id
// is unknown OR when the caller isn't entitled to see it. Same
// existence-leak prevention as conversations.
var ErrNoteNotFound = errors.New("note: not found")

// ──────────────────────────────────────────────────────────────────
// Store
// ──────────────────────────────────────────────────────────────────

// NotesStore is the persistence surface for the Notes panel +
// notes skill. Backed by the shared *db.Pool.
type NotesStore struct {
	db *db.Pool
}

// NewNotesStore constructs a store. Returns nil if pool is nil so
// the constructor mirrors the other in-admin stores.
func NewNotesStore(pool *db.Pool) *NotesStore {
	if pool == nil || pool.DB == nil {
		return nil
	}
	return &NotesStore{db: pool}
}

const snippetMaxRunes = 120

func snippetFromContent(content string) string {
	c := strings.TrimSpace(content)
	if c == "" {
		return ""
	}
	// Strip the simplest markdown noise so the snippet reads as
	// prose: leading #, list markers, blockquote bullets. Anything
	// fancier belongs in the rendered preview, not the snippet.
	c = strings.TrimLeft(c, "# >-*+ \t")
	if i := strings.IndexByte(c, '\n'); i >= 0 {
		c = c[:i]
	}
	runes := []rune(c)
	if len(runes) > snippetMaxRunes {
		return string(runes[:snippetMaxRunes-1]) + "…"
	}
	return c
}

// List returns notes owned by userID, ordered by pinned-first then
// most-recently-updated. includeDeleted=false hides soft-deleted
// rows. folder filters to a specific folder when non-empty.
func (s *NotesStore) List(ctx context.Context, userID, folder string, includeDeleted bool, limit, offset int) ([]NoteSummary, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	args := []any{userID}
	q := `
		SELECT id::text, title, COALESCE(folder, ''), pinned,
		       COALESCE(LEFT(content, 800), ''),
		       updated_at
		  FROM notes
		 WHERE user_id = $1`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	if folder != "" {
		args = append(args, folder)
		q += ` AND folder = $` + strconv.Itoa(len(args))
	}
	args = append(args, limit, offset)
	q += ` ORDER BY pinned DESC, updated_at DESC LIMIT $` +
		strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("notes: list: %w", err)
	}
	defer rows.Close()
	out := make([]NoteSummary, 0)
	for rows.Next() {
		var n NoteSummary
		var rawContent string
		if err := rows.Scan(&n.ID, &n.Title, &n.Folder, &n.Pinned, &rawContent, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("notes: scan: %w", err)
		}
		n.Snippet = snippetFromContent(rawContent)
		out = append(out, n)
	}
	return out, rows.Err()
}

// Folders returns the distinct folder labels in use for a user.
// Empty folders (NULL or ”) collapse out so the workspace tree
// can render an "Unfiled" bucket without a sentinel row.
func (s *NotesStore) Folders(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT folder FROM notes
		 WHERE user_id = $1 AND deleted_at IS NULL
		   AND folder IS NOT NULL AND folder <> ''
		 ORDER BY folder`, userID)
	if err != nil {
		return nil, fmt.Errorf("notes: folders: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Get returns one note by id, scoped to userID. ErrNoteNotFound on
// missing OR non-owner.
func (s *NotesStore) Get(ctx context.Context, id, userID string) (*Note, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, user_id, title, content, COALESCE(folder, ''),
		       pinned, created_at, updated_at, deleted_at
		  FROM notes
		 WHERE id = $1::uuid AND user_id = $2`, id, userID)
	var n Note
	err := row.Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &n.Folder,
		&n.Pinned, &n.CreatedAt, &n.UpdatedAt, &n.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("notes: get: %w", err)
	}
	return &n, nil
}

// Create inserts a new note. Defaults applied at the column level
// so an empty body is fine.
func (s *NotesStore) Create(ctx context.Context, userID, title, content, folder string) (*Note, error) {
	if userID == "" {
		return nil, fmt.Errorf("notes: create: empty user_id")
	}
	if title == "" {
		title = "Untitled"
	}
	folderArg := sql.NullString{String: folder, Valid: folder != ""}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO notes (user_id, title, content, folder)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, user_id, title, content, COALESCE(folder, ''),
		          pinned, created_at, updated_at, deleted_at`,
		userID, title, content, folderArg)
	var n Note
	if err := row.Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &n.Folder,
		&n.Pinned, &n.CreatedAt, &n.UpdatedAt, &n.DeletedAt); err != nil {
		return nil, fmt.Errorf("notes: create: %w", err)
	}
	return &n, nil
}

// NotePatch carries optional fields. Each pointer is "set when
// non-nil". Folder pointer to "" clears the folder; nil leaves it
// alone.
type NotePatch struct {
	Title   *string
	Content *string
	Folder  *string
	Pinned  *bool
}

func (s *NotesStore) Update(ctx context.Context, id, userID string, p NotePatch) (*Note, error) {
	sets := []string{"updated_at = NOW()"}
	args := []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		sets = append(sets, strings.ReplaceAll(expr, "$?", "$"+strconv.Itoa(len(args))))
	}
	if p.Title != nil {
		add("title = $?", *p.Title)
	}
	if p.Content != nil {
		add("content = $?", *p.Content)
	}
	if p.Folder != nil {
		if *p.Folder == "" {
			sets = append(sets, "folder = NULL")
		} else {
			add("folder = $?", *p.Folder)
		}
	}
	if p.Pinned != nil {
		add("pinned = $?", *p.Pinned)
	}
	args = append(args, id, userID)
	q := `UPDATE notes SET ` + strings.Join(sets, ", ") +
		` WHERE id = $` + strconv.Itoa(len(args)-1) + `::uuid` +
		` AND user_id = $` + strconv.Itoa(len(args)) +
		` AND deleted_at IS NULL` +
		` RETURNING id::text, user_id, title, content, COALESCE(folder, ''),
		           pinned, created_at, updated_at, deleted_at`
	row := s.db.QueryRowContext(ctx, q, args...)
	var n Note
	err := row.Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &n.Folder,
		&n.Pinned, &n.CreatedAt, &n.UpdatedAt, &n.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("notes: update: %w", err)
	}
	return &n, nil
}

// SoftDelete sets deleted_at = NOW() so the row vanishes from the
// list views but remains recoverable until the retention-cron
// (TBD) hard-purges. Returns ErrNoteNotFound for missing or
// non-owner.
func (s *NotesStore) SoftDelete(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE notes SET deleted_at = NOW(), updated_at = NOW()
		 WHERE id = $1::uuid AND user_id = $2 AND deleted_at IS NULL`,
		id, userID)
	if err != nil {
		return fmt.Errorf("notes: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNoteNotFound
	}
	return nil
}

// Append concatenates text to a note's body, preceded by a blank
// line if the existing content is non-empty. Used by the notes
// skill's append_to_note tool — keeping the operation atomic at
// the DB layer means concurrent appends don't drop content.
func (s *NotesStore) Append(ctx context.Context, id, userID, text string) (*Note, error) {
	if text == "" {
		return s.Get(ctx, id, userID)
	}
	// Atomic: read content, build new body, update — one statement
	// using SQL string concat. CASE handles the "append blank line"
	// rule so the formatting stays clean even for empty notes.
	row := s.db.QueryRowContext(ctx, `
		UPDATE notes
		   SET content = CASE
		       WHEN content = '' THEN $3
		       ELSE rtrim(content, E'\n') || E'\n\n' || $3
		   END,
		       updated_at = NOW()
		 WHERE id = $1::uuid AND user_id = $2 AND deleted_at IS NULL
		 RETURNING id::text, user_id, title, content, COALESCE(folder, ''),
		           pinned, created_at, updated_at, deleted_at`,
		id, userID, text)
	var n Note
	err := row.Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &n.Folder,
		&n.Pinned, &n.CreatedAt, &n.UpdatedAt, &n.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("notes: append: %w", err)
	}
	return &n, nil
}

// Search returns notes matching q via tsvector full-text search.
// Falls back to ILIKE for short queries that don't tokenize well.
// Results are ordered by ts_rank when scoring is meaningful, by
// updated_at otherwise.
func (s *NotesStore) Search(ctx context.Context, userID, q string, limit int) ([]NoteSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return s.List(ctx, userID, "", false, limit, 0)
	}
	// websearch_to_tsquery handles user-facing query syntax (OR,
	// quotes, exclusions) without us writing a parser. ILIKE on
	// the raw text catches partial-token matches the tsvector
	// would miss.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, title, COALESCE(folder, ''), pinned,
		       COALESCE(LEFT(content, 800), ''), updated_at
		  FROM notes
		 WHERE user_id = $1 AND deleted_at IS NULL
		   AND (
		       to_tsvector('english', title || ' ' || content)
		       @@ websearch_to_tsquery('english', $2)
		     OR title   ILIKE '%' || $2 || '%'
		     OR content ILIKE '%' || $2 || '%'
		   )
		 ORDER BY pinned DESC, updated_at DESC
		 LIMIT $3`,
		userID, q, limit)
	if err != nil {
		return nil, fmt.Errorf("notes: search: %w", err)
	}
	defer rows.Close()
	out := make([]NoteSummary, 0)
	for rows.Next() {
		var n NoteSummary
		var rawContent string
		if err := rows.Scan(&n.ID, &n.Title, &n.Folder, &n.Pinned, &rawContent, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("notes: search scan: %w", err)
		}
		n.Snippet = snippetFromContent(rawContent)
		out = append(out, n)
	}
	return out, rows.Err()
}

// ──────────────────────────────────────────────────────────────────
// Wire-up
// ──────────────────────────────────────────────────────────────────

// AttachNotesStore wires the notes store onto the handler.
func (h *Handler) AttachNotesStore(s *NotesStore) { h.notes = s }

// NotesStore provides programmatic access for the notes skill.
// Returns nil when the store isn't wired.
func (h *Handler) NotesStore() *NotesStore { return h.notes }

// HTTP handlers for /console/api/notes/* are gone — the frontend
// now talks to /console/api/books/personal/pages directly. The
// NotesStore + types above stay because:
//   - The legacy notes table is retained as a backup post-migration
//     (BOOKS-WIKI-ARCHITECTURE Phase 1 step 5 backfilled it into
//     personal-book pages but did NOT drop the source table).
//   - WikiStore.ListPages reuses the snippetFromContent helper
//     defined in this file.
// Once both reasons go away, this file can be deleted entirely.
