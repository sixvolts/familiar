package admin

// Books + Wiki pages CRUD (BOOKS-WIKI-ARCHITECTURE Phase 1a + role
// update).
//
// A Book is a named collection of wiki pages shared between specific
// users via the book_members join table. Pages live inside exactly
// one book; slug uniqueness is per-book, not global. Membership +
// role determine authorization:
//
//   owner  — manage members + book metadata + all page CRUD.
//            Cannot remove other owners directly (demote first).
//   writer — page CRUD; no member or book-settings management.
//   reader — read-only. Can search + view revisions.
//
// Non-members get 404 from every endpoint (existence-leak
// prevention; same shape as notes / conversations). Familiar admin
// users bypass membership + role checks entirely as an operational
// escape hatch.
//
// This file owns:
//   * Types — Book, BookSummary, BookMember, WikiPage, WikiPageSummary,
//     WikiRevision
//   * WikiStore — books CRUD, member management, pages CRUD with
//     revision tracking, full-text search
//   * Handlers — /console/api/books/* surface
//   * scopeForWiki — membership scope (404 on non-member)
//   * requireOwner — gates member + book-settings endpoints
//   * requirePageWrite — gates page POST/PATCH/DELETE
//
// Phase 2 wires parent_id + sort_order for hierarchy. Phase 3 fills
// maintained_by + edit_count via revision-count aggregation. The
// columns exist in 1a but the store leaves them at defaults.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/textmerge"
)

// ──────────────────────────────────────────────────────────────────
// Types
// ──────────────────────────────────────────────────────────────────

// Book is one shared collection of wiki pages.
type Book struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	IsPersonal  bool       `json:"is_personal,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
}

// BookSummary is the list-view row — adds the caller's role on the
// book so the frontend can decide which actions to show without a
// second round-trip. PageCount is a denormalized count of live
// (non-deleted) pages; the mobile UI uses it to render a per-book
// page tally without per-row fan-out fetches.
type BookSummary struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Role        string     `json:"role"`
	PageCount   int        `json:"page_count"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
}

// BookMember is one row in book_members. The owner is the user who
// created the book (auto-added on Create); roles are owner/editor/
// viewer with viewer reserved for Phase 2 RBAC.
type BookMember struct {
	BookID   string    `json:"book_id"`
	UserID   string    `json:"user_id"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// WikiPage is one markdown document inside a book. parent_id +
// sort_order are populated in Phase 2; maintained_by + edit_count
// in Phase 3.
type WikiPage struct {
	ID           string     `json:"id"`
	BookID       string     `json:"book_id"`
	Slug         string     `json:"slug"`
	Title        string     `json:"title"`
	Content      string     `json:"content"`
	ParentID     *string    `json:"parent_id,omitempty"`
	SortOrder    int        `json:"sort_order"`
	CreatedBy    string     `json:"created_by"`
	UpdatedBy    string     `json:"updated_by"`
	MaintainedBy *string    `json:"maintained_by,omitempty"`
	EditCount    int        `json:"edit_count"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	// Pinned is per-caller (handler joins user_page_prefs before
	// serializing). Same posture as WikiPageSummary.Pinned.
	Pinned bool `json:"pinned,omitempty"`
	// Share is the current public share row for this page, if any.
	// Populated by the page-by-id GET / PATCH / share-toggle handlers
	// so the frontend can render the globe indicator + Share menu
	// state in one round-trip. omitempty so a page that's not shared
	// doesn't carry the field at all.
	Share *PageShare `json:"share,omitempty"`
	// Merged is a transient, per-response flag (never stored). UpdatePage
	// sets it true when a stale content-only save was auto-merged against
	// the base revision instead of rejected — the returned Content is the
	// three-way merge of the caller's edit and the concurrent write. The
	// client swaps this into its editor and reseeds its baseline, showing
	// a "merged with someone else's changes" notice rather than a stale
	// banner. omitempty so ordinary saves don't carry the field.
	Merged bool `json:"merged,omitempty"`
}

// WikiPageSummary drops content from list responses for the same
// reason NoteSummary does — the sidebar shouldn't drag full bodies
// across the wire when only titles are rendered.
type WikiPageSummary struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Snippet   string    `json:"snippet,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
	// Pinned is per-caller (joined from user_page_prefs by the
	// handler before serializing). The store's list query doesn't
	// populate it; that's the handler's job since pin state is
	// caller-scoped.
	Pinned bool `json:"pinned,omitempty"`
	// ParentID + SortOrder give the sidebar enough to build a tree
	// view without a second round-trip. omitempty so the omitted-
	// case carries no field.
	ParentID  *string `json:"parent_id,omitempty"`
	SortOrder int     `json:"sort_order"`
}

// WikiRevision is one historical snapshot of a page's content.
type WikiRevision struct {
	ID        string    `json:"id"`
	PageID    string    `json:"page_id"`
	Content   string    `json:"content"`
	EditedBy  string    `json:"edited_by"`
	CreatedAt time.Time `json:"created_at"`
	Summary   string    `json:"summary,omitempty"`
}

// ErrBookNotFound and ErrPageNotFound mirror the notes / conversations
// 404 pattern: returned both when the row truly doesn't exist AND
// when the caller isn't a member of the book. Same status to both
// prevents probing for existence.
var (
	ErrBookNotFound = errors.New("book: not found")
	ErrPageNotFound = errors.New("wiki page: not found")
	// ErrPageStale is returned when a conditional UpdatePage receives
	// an If-Match timestamp older than the row's current updated_at.
	// The HTTP layer maps this to 409 Conflict so the client can pull
	// fresh state and decide how to reconcile its in-flight edits.
	// Pages keep per-edit revisions in wiki_revisions; nothing is
	// lost server-side, but live last-write-wins overwrites were the
	// shape of the user-visible "my edit wiped theirs" bug.
	ErrPageStale = errors.New("wiki page: stale (precondition failed)")
)

// ──────────────────────────────────────────────────────────────────
// Store
// ──────────────────────────────────────────────────────────────────

// PageActor identifies who performed a page mutation. UserID is the
// canonical user id (the shard's owner when the write came from a
// shard session). ShardID is non-empty only when the request was
// driven by a shard session — the SSE hook uses it to swap the
// editor's "Synced — Operator" label for "Synced — Recipes Bot" so
// idle viewers know an automated agent moved the page.
type PageActor struct {
	UserID  string
	ShardID string
}

// actorCtxKey is the context key the handlers use to publish actor
// metadata for the WikiStore's save/delete hooks. Per-package
// unexported type so other packages can't accidentally clobber it.
type actorCtxKey struct{}

// WithPageActor returns ctx carrying actor metadata for downstream
// hook fires. Handlers call this before invoking UpdatePage /
// CreatePage / DeletePage so the SSE payload can carry shard info
// when the write came from a shard session.
func WithPageActor(ctx context.Context, a PageActor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

// PageActorFromContext extracts actor metadata. Returns a zero value
// (no shard id) when the context doesn't carry one.
func PageActorFromContext(ctx context.Context) PageActor {
	if a, ok := ctx.Value(actorCtxKey{}).(PageActor); ok {
		return a
	}
	return PageActor{}
}

// PageSavedHook fires after a successful CreatePage or UpdatePage
// commit. Receives the post-write page snapshot, the book's slug
// (since the page row only carries book_id), the userID that did
// the write (creator on create, updater on update), the shardID
// when the write was driven by a shard session (empty otherwise),
// and the freshly-rebuilt outbound link list. Best-effort — runs
// in a goroutine with a fresh context.Background() so a slow
// ingestion doesn't block the HTTP response, and a hook panic is
// recovered inside the goroutine so we don't take the gateway down.
type PageSavedHook func(page WikiPage, bookSlug, userID, shardID string, links []PageLink)

// PageDeletedHook fires after a successful DeletePage commit.
// Receives the identifiers needed to clean up scoped facts /
// memories without re-querying.
type PageDeletedHook func(bookID, bookSlug, pageID, pageSlug string)

// WikiStore is the persistence surface for Books and pages.
type WikiStore struct {
	db          *db.Pool
	pageSaved   PageSavedHook   // optional; SetPageSavedHook wires it
	pageDeleted PageDeletedHook // optional; SetPageDeletedHook wires it
}

// NewWikiStore constructs a store. Returns nil if pool is nil so
// the constructor mirrors the other in-admin stores.
func NewWikiStore(pool *db.Pool) *WikiStore {
	if pool == nil || pool.DB == nil {
		return nil
	}
	return &WikiStore{db: pool}
}

// SetPageSavedHook installs the post-save callback. Nil unsets.
// Called from cmd/gateway/main.go after constructing the wiki
// knowledge pipeline.
func (s *WikiStore) SetPageSavedHook(fn PageSavedHook) { s.pageSaved = fn }

// SetPageDeletedHook installs the post-delete callback.
func (s *WikiStore) SetPageDeletedHook(fn PageDeletedHook) { s.pageDeleted = fn }

// firePageSaved invokes the hook in a panic-safe goroutine so the
// HTTP response returns immediately and a malfunctioning ingestion
// pipeline can't crash the gateway.
func (s *WikiStore) firePageSaved(page WikiPage, bookSlug, userID, shardID string, links []PageLink) {
	if s.pageSaved == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[wiki] page-saved hook panic: %v\n", r)
			}
		}()
		s.pageSaved(page, bookSlug, userID, shardID, links)
	}()
}

func (s *WikiStore) firePageDeleted(bookID, bookSlug, pageID, pageSlug string) {
	if s.pageDeleted == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[wiki] page-deleted hook panic: %v\n", r)
			}
		}()
		s.pageDeleted(bookID, bookSlug, pageID, pageSlug)
	}()
}

// AttachWikiStore wires the store onto the handler.
func (h *Handler) AttachWikiStore(s *WikiStore) { h.wiki = s }

// WikiStore exposes the attached store. Lazy-resolved by the wiki
// skill so registration in main.go can predate the pool-init block
// that constructs the store.
func (h *Handler) WikiStore() *WikiStore { return h.wiki }

// ── Slug helpers ──────────────────────────────────────────────────
//
// Slugs auto-generate from a name on create. Manual override is
// allowed via the explicit `slug` field on the request body.

var slugSafeRe = regexp.MustCompile(`[^a-z0-9]+`)

// shortHex generates a 6-character lowercase hex string for slug
// disambiguation. Uses crypto/rand; falls back to timestamp-based
// if that somehow fails.
func shortHex() string {
	b := make([]byte, 3) // 3 bytes → 6 hex chars
	if _, err := rand.Read(b); err != nil {
		// Fallback: last 6 hex digits of current nanosecond timestamp.
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xFFFFFF)
	}
	return fmt.Sprintf("%02x%02x%02x", b[0], b[1], b[2])
}

// hexSuffixRe matches the -XXXXXX disambiguation suffix appended
// by uniquePageSlug / uniqueBookSlug on collision.
var hexSuffixRe = regexp.MustCompile(`-[0-9a-f]{6}$`)

// DisplaySlug strips the collision-disambiguation hex suffix from
// a slug for any context where the slug is user-visible (currently
// rare — titles are the primary display, not slugs).
func DisplaySlug(slug string) string {
	return hexSuffixRe.ReplaceAllString(slug, "")
}

// slugify normalizes a free-text label to a URL-safe slug.
// Lowercase, ASCII alphanumerics + dashes, no leading/trailing
// dash. Empty input → "untitled". Long inputs are truncated at
// 60 chars on a word boundary so URLs stay readable.
func slugify(in string) string {
	s := strings.ToLower(strings.TrimSpace(in))
	s = slugSafeRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "untitled"
	}
	if len(s) > 60 {
		s = s[:60]
		if i := strings.LastIndexByte(s, '-'); i > 30 {
			s = s[:i]
		}
	}
	return s
}

// uniqueBookSlug picks a slug that isn't already taken. Tries the
// base first; on collision appends a short random hex suffix
// (e.g. "my-book-8cb47e"). The suffix never appears in the UI —
// books and pages display their title, not slug.
func (s *WikiStore) uniqueBookSlug(ctx context.Context, base string) (string, error) {
	candidate := base
	for i := 0; i < 10; i++ {
		var exists bool
		err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM books WHERE slug = $1)`, candidate).Scan(&exists)
		if err != nil {
			return "", fmt.Errorf("books: slug check: %w", err)
		}
		if !exists {
			return candidate, nil
		}
		candidate = base + "-" + shortHex()
	}
	return "", fmt.Errorf("books: couldn't find unique slug after 10 tries")
}

// uniquePageSlug picks a slug that isn't already taken WITHIN the
// given book. On collision, appends a short random hex suffix.
func (s *WikiStore) uniquePageSlug(ctx context.Context, bookID, base string) (string, error) {
	candidate := base
	for i := 0; i < 10; i++ {
		var exists bool
		err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM wiki_pages
                              WHERE book_id = $1 AND slug = $2 AND deleted_at IS NULL)`,
			bookID, candidate).Scan(&exists)
		if err != nil {
			return "", fmt.Errorf("wiki: slug check: %w", err)
		}
		if !exists {
			return candidate, nil
		}
		candidate = base + "-" + shortHex()
	}
	return "", fmt.Errorf("wiki: couldn't find unique slug after 10 tries")
}

// ── Book CRUD ─────────────────────────────────────────────────────

// ListBooks returns every book the user is a member of, ordered by
// most-recently-updated first. Admins can pass an explicit user_id
// to see another user's books (respected by the handler).
// includeArchived=false hides archived books.
func (s *WikiStore) ListBooks(ctx context.Context, userID string, includeArchived bool) ([]BookSummary, error) {
	return s.listBooksFiltered(ctx, userID, includeArchived, false)
}

// ListMemberBookIDs returns every book id the user is a member of,
// UNFILTERED by listing visibility. The page-events SSE gates on
// membership (not "is it listable"), so hidden books — personal, and
// the research evidence books — still push their events to their
// members. Using the listing query here instead would drop research
// events and break the live evidence view.
func (s *WikiStore) ListMemberBookIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT book_id::text FROM book_members WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("books: member ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("books: member id scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListBooksWithPersonal is the include-personal variant — used by
// the wiki skill when the model wants to operate on the user's
// personal book (where their notes live). The HTTP /books listing
// stays personal-free; this is a deliberate split.
func (s *WikiStore) ListBooksWithPersonal(ctx context.Context, userID string, includeArchived bool) ([]BookSummary, error) {
	return s.listBooksFiltered(ctx, userID, includeArchived, true)
}

func (s *WikiStore) listBooksFiltered(ctx context.Context, userID string, includeArchived, includePersonal bool) ([]BookSummary, error) {
	q := `
		SELECT b.id::text, b.slug, b.name, b.description, m.role,
		       (SELECT count(*) FROM wiki_pages wp
		         WHERE wp.book_id = b.id AND wp.deleted_at IS NULL) AS page_count,
		       b.updated_at, b.archived_at
		  FROM books b
		  JOIN book_members m ON m.book_id = b.id
		 WHERE m.user_id = $1`
	if !includePersonal {
		q += ` AND b.is_personal = false`
	}
	// Research evidence books are system-managed per-user scratch for
	// the research skill's workers — never surfaced in any book listing
	// (HTTP or the model's list_books), even the include-personal one.
	// The research skill addresses them directly by their deterministic
	// slug, so they never need to be discoverable.
	q += ` AND b.slug NOT LIKE 'research:%'`
	if !includeArchived {
		q += ` AND b.archived_at IS NULL`
	}
	q += ` ORDER BY b.updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("books: list: %w", err)
	}
	defer rows.Close()
	out := make([]BookSummary, 0)
	for rows.Next() {
		var b BookSummary
		if err := rows.Scan(&b.ID, &b.Slug, &b.Name, &b.Description,
			&b.Role, &b.PageCount, &b.UpdatedAt, &b.ArchivedAt); err != nil {
			return nil, fmt.Errorf("books: scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBookBySlug fetches a book by slug and verifies the caller is a
// member. Returns ErrBookNotFound if the book doesn't exist OR if
// the caller isn't a member (existence-leak prevention).
func (s *WikiStore) GetBookBySlug(ctx context.Context, slug, userID string, isAdmin bool) (*Book, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, slug, name, description, is_personal, created_by,
		       created_at, updated_at, archived_at
		  FROM books WHERE slug = $1`, slug)
	var b Book
	err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Description, &b.IsPersonal, &b.CreatedBy,
		&b.CreatedAt, &b.UpdatedAt, &b.ArchivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBookNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("books: get: %w", err)
	}
	if isAdmin {
		return &b, nil
	}
	role, err := s.MemberRole(ctx, b.ID, userID)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, ErrBookNotFound
	}
	return &b, nil
}

// CreateBook inserts a book and adds the creator as 'owner'. slug
// is generated from name when blank; manual slug is honored when
// present (with the same uniqueness check). Refuses the
// "personal:" prefix — those slugs are reserved for personal books
// and only EnsurePersonalBook can mint them.
func (s *WikiStore) CreateBook(ctx context.Context, userID, name, description, requestedSlug string) (*Book, error) {
	if userID == "" {
		return nil, fmt.Errorf("books: create: empty user_id")
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("books: create: name required")
	}
	base := slugify(requestedSlug)
	if requestedSlug == "" {
		base = slugify(name)
	}
	if base == "personal" || strings.HasPrefix(base, "personal:") || strings.HasPrefix(base, "personal-") {
		return nil, fmt.Errorf("books: create: 'personal' slug (and 'personal:' prefix) is reserved")
	}
	// The per-user research evidence book uses slug "research:{userID}"
	// (colon), minted only by EnsureResearchBook via insertBookTx.
	// CreateBook always slugifies, and slugify maps ':' → '-', so a
	// user-requested slug can never produce the colon form — it can't
	// collide with, or be hidden as, a system research book. No
	// reservation needed here (unlike 'personal', whose bare/dash forms
	// are also special).
	slug, err := s.uniqueBookSlug(ctx, base)
	if err != nil {
		return nil, err
	}
	return s.insertBookTx(ctx, userID, slug, name, description, false)
}

// insertBookTx is the shared insert path for CreateBook + Ensure-
// PersonalBook. The tx wraps the books row + the creator's owner
// row so a failed membership insert doesn't leave an orphan book.
func (s *WikiStore) insertBookTx(ctx context.Context, userID, slug, name, description string, isPersonal bool) (*Book, error) {
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("books: begin tx: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
		INSERT INTO books (slug, name, description, is_personal, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, slug, name, description, is_personal, created_by,
		          created_at, updated_at, archived_at`,
		slug, name, description, isPersonal, userID)
	var b Book
	if err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Description, &b.IsPersonal, &b.CreatedBy,
		&b.CreatedAt, &b.UpdatedAt, &b.ArchivedAt); err != nil {
		return nil, fmt.Errorf("books: insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO book_members (book_id, user_id, role)
		VALUES ($1::uuid, $2, 'owner')`, b.ID, userID); err != nil {
		return nil, fmt.Errorf("books: add owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("books: commit: %w", err)
	}
	return &b, nil
}

// EnsurePersonalBook returns the user's personal book, creating it
// idempotently on first use. Personal books are hidden from
// /console/api/books listings, can't be archived or renamed, and
// have a fixed slug pattern of "personal:{userID}". The user is
// the sole owner.
//
// Concurrency: two simultaneous calls race to insert; the slug
// uniqueness constraint serializes them. The loser falls back to
// the SELECT path and returns the existing book.
func (s *WikiStore) EnsurePersonalBook(ctx context.Context, userID string) (*Book, error) {
	if userID == "" {
		return nil, fmt.Errorf("books: ensure personal: empty user_id")
	}
	slug := personalSlug(userID)

	// Fast path — already exists.
	if b, err := s.getBookBySlugRaw(ctx, slug); err == nil {
		return b, nil
	} else if !errors.Is(err, ErrBookNotFound) {
		return nil, err
	}

	// Slow path — try to create. The uniqueness index handles a
	// race; on conflict we re-read.
	b, err := s.insertBookTx(ctx, userID, slug, "Personal", "Your personal notes", true)
	if err == nil {
		return b, nil
	}
	// Race: another goroutine got here first; re-read.
	if existing, err2 := s.getBookBySlugRaw(ctx, slug); err2 == nil {
		return existing, nil
	}
	return nil, err
}

// getBookBySlugRaw reads a book by slug WITHOUT membership checking.
// Internal-only helper for EnsurePersonalBook's race fallback.
// Never expose this path to callers — they should use
// GetBookBySlug which enforces membership.
func (s *WikiStore) getBookBySlugRaw(ctx context.Context, slug string) (*Book, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, slug, name, description, is_personal, created_by,
		       created_at, updated_at, archived_at
		  FROM books WHERE slug = $1`, slug)
	var b Book
	err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Description, &b.IsPersonal, &b.CreatedBy,
		&b.CreatedAt, &b.UpdatedAt, &b.ArchivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBookNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("books: get raw: %w", err)
	}
	return &b, nil
}

// personalSlug is the deterministic slug pattern for a user's
// personal book. Kept in one place so all callers agree.
func personalSlug(userID string) string { return "personal:" + userID }

// researchSlug is the deterministic slug for a user's research
// evidence book — the per-user, listing-hidden scratch book the
// research skill's workers write to. Kept beside personalSlug so the
// two reserved-prefix conventions stay together.
func researchSlug(userID string) string { return "research:" + userID }

// EnsureResearchBook returns the user's research evidence book,
// creating it idempotently on first use. It mirrors EnsurePersonalBook
// exactly (deterministic per-user slug, race-safe via the slug unique
// index, hidden from every book listing, system-managed) — the crucial
// difference from a normal CreateBook'd book is that it is PER-USER, so
// concurrent users never collide on a shared "research" slug. Evidence
// pages live here (not the personal book) so a prompt-injected worker
// reading hostile web content is confined by BookAccess and can't reach
// the user's personal notes (RESEARCH-SKILL-SPEC §6.2, §9).
func (s *WikiStore) EnsureResearchBook(ctx context.Context, userID string) (*Book, error) {
	if userID == "" {
		return nil, fmt.Errorf("books: ensure research: empty user_id")
	}
	slug := researchSlug(userID)
	if b, err := s.getBookBySlugRaw(ctx, slug); err == nil {
		return b, nil
	} else if !errors.Is(err, ErrBookNotFound) {
		return nil, err
	}
	b, err := s.insertBookTx(ctx, userID, slug, "Research", "Research evidence (system-managed)", false)
	if err == nil {
		return b, nil
	}
	// Race: another goroutine inserted first; re-read.
	if existing, err2 := s.getBookBySlugRaw(ctx, slug); err2 == nil {
		return existing, nil
	}
	return nil, err
}

// BookPatch is the dynamic-update payload for UpdateBook. nil fields
// are ignored, non-nil fields overwrite. Archive=true sets archived_at
// to NOW(); Archive=false clears it.
type BookPatch struct {
	Name        *string
	Description *string
	Archive     *bool
}

// UpdateBook applies a patch to a book the caller owns. Returns
// ErrBookNotFound on non-owner so a non-admin can't probe.
// Personal books refuse rename + archive (their slug is wired to
// the user id and they're not part of the book lifecycle).
func (s *WikiStore) UpdateBook(ctx context.Context, slug, userID string, isAdmin bool, p BookPatch) (*Book, error) {
	b, err := s.GetBookBySlug(ctx, slug, userID, isAdmin)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		role, _ := s.MemberRole(ctx, b.ID, userID)
		if role != "owner" {
			return nil, ErrBookNotFound
		}
	}
	if b.IsPersonal {
		if p.Name != nil || p.Archive != nil {
			return nil, fmt.Errorf("books: personal books cannot be renamed or archived")
		}
	}
	if strings.HasPrefix(b.Slug, "research:") {
		if p.Name != nil || p.Archive != nil {
			return nil, fmt.Errorf("books: research evidence books are system-managed and cannot be renamed or archived")
		}
	}
	sets := []string{"updated_at = NOW()"}
	args := []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		sets = append(sets, strings.ReplaceAll(expr, "$?", "$"+strconv.Itoa(len(args))))
	}
	if p.Name != nil {
		add("name = $?", *p.Name)
	}
	if p.Description != nil {
		add("description = $?", *p.Description)
	}
	if p.Archive != nil {
		if *p.Archive {
			sets = append(sets, "archived_at = NOW()")
		} else {
			sets = append(sets, "archived_at = NULL")
		}
	}
	args = append(args, b.ID)
	q := `UPDATE books SET ` + strings.Join(sets, ", ") +
		` WHERE id = $` + strconv.Itoa(len(args)) + `::uuid` +
		` RETURNING id::text, slug, name, description, is_personal, created_by,
		           created_at, updated_at, archived_at`
	row := s.db.QueryRowContext(ctx, q, args...)
	var out Book
	if err := row.Scan(&out.ID, &out.Slug, &out.Name, &out.Description, &out.IsPersonal,
		&out.CreatedBy, &out.CreatedAt, &out.UpdatedAt, &out.ArchivedAt); err != nil {
		return nil, fmt.Errorf("books: update: %w", err)
	}
	// Audit owner-visible changes. Best-effort: a failed audit
	// write logs but doesn't roll back the patch.
	if p.Name != nil && *p.Name != b.Name {
		s.recordAudit(ctx, b.ID, userID, "book_renamed", "", b.Name, *p.Name)
	}
	if p.Archive != nil {
		if *p.Archive && b.ArchivedAt == nil {
			s.recordAudit(ctx, b.ID, userID, "book_archived", "", "", "")
		} else if !*p.Archive && b.ArchivedAt != nil {
			s.recordAudit(ctx, b.ID, userID, "book_unarchived", "", "", "")
		}
	}
	return &out, nil
}

// ── Member CRUD ───────────────────────────────────────────────────
//
// PRECONDITION (applies to ListMembers / AddMember / RemoveMember):
// caller MUST have already proven they're an OWNER of bookID. The
// HTTP layer enforces this via h.requireOwner. These methods do not
// re-check; they trust the bookID is owner-vetted. Calling them
// with an unverified bookID would let any member modify another
// book's roster.

// MemberRole returns the caller's role on a book ("owner", "editor",
// "viewer") or "" if the caller isn't a member. Used both for auth
// scoping AND to decorate listings with the caller's role.
func (s *WikiStore) MemberRole(ctx context.Context, bookID, userID string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `
		SELECT role FROM book_members
		 WHERE book_id = $1::uuid AND user_id = $2`,
		bookID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("books: role lookup: %w", err)
	}
	return role, nil
}

// ListMembers returns every member of a book.
func (s *WikiStore) ListMembers(ctx context.Context, bookID string) ([]BookMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT book_id::text, user_id, role, joined_at
		  FROM book_members
		 WHERE book_id = $1::uuid
		 ORDER BY (role = 'owner') DESC, joined_at ASC`, bookID)
	if err != nil {
		return nil, fmt.Errorf("books: members list: %w", err)
	}
	defer rows.Close()
	out := make([]BookMember, 0)
	for rows.Next() {
		var m BookMember
		if err := rows.Scan(&m.BookID, &m.UserID, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("books: members scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMember idempotently inserts a member at the given role. ON
// CONFLICT updates the role so calling AddMember again can be used
// to flip an existing member's role.
// AddMember idempotently sets a member's role on a book. Used both
// for first-time invites AND for role changes (the PATCH role
// handler routes through here). Three rules enforced at the store
// level so handlers don't have to reason about edge cases:
//  1. Role must be one of owner / writer / reader.
//  2. Demotion away from "owner" is blocked when the target is
//     currently the only owner — the book would be left with no
//     manage path. Caller must promote someone else to owner
//     first.
//  3. Promotion from non-member straight to writer is fine; the
//     "demote-the-last-owner" check only fires on existing owners.
func (s *WikiStore) AddMember(ctx context.Context, actorUserID, bookID, targetUserID, role string) (*BookMember, error) {
	if role == "" {
		role = "writer"
	}
	if role != "owner" && role != "writer" && role != "reader" {
		return nil, fmt.Errorf("books: invalid role %q", role)
	}
	// Look up the prior role first — needed for the demote-the-last-
	// owner guard AND so the audit log can record old→new on a
	// role change versus a fresh add.
	var currentRole sql.NullString
	var ownerCount int
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT role FROM book_members
			   WHERE book_id = $1::uuid AND user_id = $2),
			(SELECT COUNT(*) FROM book_members
			   WHERE book_id = $1::uuid AND role = 'owner')
	`, bookID, targetUserID).Scan(&currentRole, &ownerCount); err != nil {
		return nil, fmt.Errorf("books: role lookup: %w", err)
	}
	if role != "owner" && currentRole.Valid && currentRole.String == "owner" && ownerCount <= 1 {
		return nil, fmt.Errorf("books: cannot demote the last owner")
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO book_members (book_id, user_id, role)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT (book_id, user_id) DO UPDATE SET role = EXCLUDED.role
		RETURNING book_id::text, user_id, role, joined_at`,
		bookID, targetUserID, role)
	var m BookMember
	if err := row.Scan(&m.BookID, &m.UserID, &m.Role, &m.JoinedAt); err != nil {
		return nil, fmt.Errorf("books: add member: %w", err)
	}
	// Audit: distinguish a fresh add from a role change so the
	// log reads naturally.
	if currentRole.Valid {
		if currentRole.String != role {
			s.recordAudit(ctx, bookID, actorUserID, "member_role_changed", targetUserID, currentRole.String, role)
		}
	} else {
		s.recordAudit(ctx, bookID, actorUserID, "member_added", targetUserID, "", role)
	}
	return &m, nil
}

// RemoveMember deletes the (book, user) row. Refuses to remove an
// owner outright — caller must demote the owner to writer first
// (via PATCH role) and then remove. This keeps "last owner"
// protection from being a one-off special case and gives an
// auditable two-step demotion path.
func (s *WikiStore) RemoveMember(ctx context.Context, actorUserID, bookID, targetUserID string) error {
	var targetRole string
	err := s.db.QueryRowContext(ctx, `
		SELECT role FROM book_members
		 WHERE book_id = $1::uuid AND user_id = $2`,
		bookID, targetUserID).Scan(&targetRole)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already gone, idempotent
	}
	if err != nil {
		return fmt.Errorf("books: target role: %w", err)
	}
	if targetRole == "owner" {
		return fmt.Errorf("books: cannot remove an owner directly; demote to writer first")
	}
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM book_members WHERE book_id = $1::uuid AND user_id = $2`,
		bookID, targetUserID)
	if err != nil {
		return fmt.Errorf("books: remove member: %w", err)
	}
	s.recordAudit(ctx, bookID, actorUserID, "member_removed", targetUserID, targetRole, "")
	return nil
}

// recordAudit appends a row to book_audit. Best-effort: any failure
// is logged via fmt.Errorf but never returned, so a stale audit row
// can't roll back the privileged operation that triggered it. The
// alternative — making audit writes part of the same transaction —
// would mean a flaky audit table could lock owners out of their own
// books. Audit gaps are acceptable; permission breakage isn't.
func (s *WikiStore) recordAudit(ctx context.Context, bookID, actorUserID, action, targetUserID, oldValue, newValue string) {
	var target, old, new sql.NullString
	if targetUserID != "" {
		target = sql.NullString{String: targetUserID, Valid: true}
	}
	if oldValue != "" {
		old = sql.NullString{String: oldValue, Valid: true}
	}
	if newValue != "" {
		new = sql.NullString{String: newValue, Valid: true}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO book_audit (book_id, actor_user_id, action,
		                        target_user_id, old_value, new_value)
		VALUES ($1::uuid, $2, $3, $4, $5, $6)`,
		bookID, actorUserID, action, target, old, new,
	); err != nil {
		// Surface to the gateway log but don't propagate.
		fmt.Printf("[wiki] audit write failed (book=%s action=%s actor=%s): %v\n",
			bookID, action, actorUserID, err)
	}
}

// ── Page CRUD ─────────────────────────────────────────────────────
//
// PRECONDITION (applies to every method in this section that takes a
// bookID without a userID):
//
//   The caller MUST have already proven membership on bookID — either
//   through h.scopeForWiki on the HTTP path or through wikiskill's
//   resolveBook on the skill path. These methods do NOT re-check
//   membership; they trust the bookID is permission-vetted. Calling
//   them with an unverified bookID would expose pages across the
//   tenant boundary.
//
// If you're adding a new caller (sidecar, cron, alternate API), call
// MemberRole(bookID, userID) first and bail on "" before invoking
// these.

// ListPages returns every non-deleted page in a book, newest-edit-
// first. See PRECONDITION above.
func (s *WikiStore) ListPages(ctx context.Context, bookID string) ([]WikiPageSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, slug, title, content, updated_at, updated_by,
		       parent_id::text, sort_order
		  FROM wiki_pages
		 WHERE book_id = $1::uuid AND deleted_at IS NULL
		 ORDER BY sort_order ASC, updated_at DESC`, bookID)
	if err != nil {
		return nil, fmt.Errorf("wiki: list: %w", err)
	}
	defer rows.Close()
	out := make([]WikiPageSummary, 0)
	for rows.Next() {
		var p WikiPageSummary
		var content string
		var parentID sql.NullString
		if err := rows.Scan(&p.ID, &p.Slug, &p.Title, &content,
			&p.UpdatedAt, &p.UpdatedBy, &parentID, &p.SortOrder); err != nil {
			return nil, fmt.Errorf("wiki: scan: %w", err)
		}
		p.Snippet = snippetFromContent(content)
		if parentID.Valid {
			p.ParentID = &parentID.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPage returns the full row for a page in a book, by page slug.
// Returns ErrPageNotFound when missing or soft-deleted. See
// PRECONDITION above (caller must have verified membership on bookID).
func (s *WikiStore) GetPage(ctx context.Context, bookID, pageSlug string) (*WikiPage, error) {
	// Primary: exact slug match.
	p, err := s.getPageBySlug(ctx, bookID, pageSlug)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, ErrPageNotFound) {
		return nil, err
	}
	// Fallback: match by slugified title. Handles the common case
	// where a page was created with an auto-generated slug (e.g.
	// "untitled-4") and later renamed. Wiki links reference the
	// title ("French Toast" → slug "french-toast"), but the stored
	// slug is still "untitled-4".
	return s.getPageBySlugifiedTitle(ctx, bookID, pageSlug)
}

func (s *WikiStore) getPageBySlug(ctx context.Context, bookID, pageSlug string) (*WikiPage, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, book_id::text, slug, title, content,
		       parent_id::text, sort_order,
		       created_by, updated_by, maintained_by, edit_count,
		       created_at, updated_at, deleted_at
		  FROM wiki_pages
		 WHERE book_id = $1::uuid AND slug = $2 AND deleted_at IS NULL`,
		bookID, pageSlug)
	var p WikiPage
	var parentID sql.NullString
	var maintainedBy sql.NullString
	err := row.Scan(&p.ID, &p.BookID, &p.Slug, &p.Title, &p.Content,
		&parentID, &p.SortOrder,
		&p.CreatedBy, &p.UpdatedBy, &maintainedBy, &p.EditCount,
		&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wiki: get: %w", err)
	}
	if parentID.Valid {
		p.ParentID = &parentID.String
	}
	if maintainedBy.Valid {
		p.MaintainedBy = &maintainedBy.String
	}
	return &p, nil
}

// getPageBySlugifiedTitle scans all non-deleted pages in a book and
// returns the first whose slugified title matches the requested slug.
// This handles pages where the stored slug doesn't match the title
// (e.g. auto-generated "untitled-4" for a page later renamed
// "French Toast"). O(n) in pages-per-book; acceptable for <1000 pages.
func (s *WikiStore) getPageBySlugifiedTitle(ctx context.Context, bookID, requestedSlug string) (*WikiPage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, book_id::text, slug, title, content,
		       parent_id::text, sort_order,
		       created_by, updated_by, maintained_by, edit_count,
		       created_at, updated_at, deleted_at
		  FROM wiki_pages
		 WHERE book_id = $1::uuid AND deleted_at IS NULL`, bookID)
	if err != nil {
		return nil, fmt.Errorf("wiki: getByTitle scan: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p WikiPage
		var parentID, maintainedBy sql.NullString
		if err := rows.Scan(&p.ID, &p.BookID, &p.Slug, &p.Title, &p.Content,
			&parentID, &p.SortOrder,
			&p.CreatedBy, &p.UpdatedBy, &maintainedBy, &p.EditCount,
			&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
			return nil, fmt.Errorf("wiki: getByTitle row: %w", err)
		}
		if slugify(p.Title) == requestedSlug {
			if parentID.Valid {
				p.ParentID = &parentID.String
			}
			if maintainedBy.Valid {
				p.MaintainedBy = &maintainedBy.String
			}
			return &p, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("wiki: getByTitle iter: %w", err)
	}
	return nil, ErrPageNotFound
}

// GetPageByID is the by-id variant of GetPage. Same membership
// PRECONDITION applies — the bookID must be the result of a
// membership-vetted lookup, and the page is required to live in
// that book (cross-book lookups via raw page id would leak across
// the membership boundary).
func (s *WikiStore) GetPageByID(ctx context.Context, bookID, pageID string) (*WikiPage, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, book_id::text, slug, title, content,
		       parent_id::text, sort_order,
		       created_by, updated_by, maintained_by, edit_count,
		       created_at, updated_at, deleted_at
		  FROM wiki_pages
		 WHERE id = $1::uuid AND book_id = $2::uuid AND deleted_at IS NULL`,
		pageID, bookID)
	var p WikiPage
	var parentID sql.NullString
	var maintainedBy sql.NullString
	err := row.Scan(&p.ID, &p.BookID, &p.Slug, &p.Title, &p.Content,
		&parentID, &p.SortOrder,
		&p.CreatedBy, &p.UpdatedBy, &maintainedBy, &p.EditCount,
		&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wiki: get by id: %w", err)
	}
	if parentID.Valid {
		p.ParentID = &parentID.String
	}
	if maintainedBy.Valid {
		p.MaintainedBy = &maintainedBy.String
	}
	return &p, nil
}

// SetPagePinned upserts a (user_id, page_id) pin row. The pin is
// per-user per-page so two members can have different pinned sets
// on the same shared book. PRECONDITION: caller has read access to
// the page (i.e. is a member of its book).
func (s *WikiStore) SetPagePinned(ctx context.Context, userID, pageID string, pinned bool) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_page_prefs (user_id, page_id, pinned)
		VALUES ($1, $2::uuid, $3)
		ON CONFLICT (user_id, page_id) DO UPDATE SET pinned = EXCLUDED.pinned`,
		userID, pageID, pinned)
	if err != nil {
		return fmt.Errorf("wiki: set pin: %w", err)
	}
	return nil
}

// PinnedPageIDs returns the set of pages this user has pinned in
// the given book. Cheap one-shot read so callers can decorate a
// list view without per-row round-trips.
func (s *WikiStore) PinnedPageIDs(ctx context.Context, userID, bookID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.page_id::text
		  FROM user_page_prefs p
		  JOIN wiki_pages wp ON wp.id = p.page_id
		 WHERE p.user_id = $1
		   AND wp.book_id = $2::uuid
		   AND p.pinned = true`,
		userID, bookID)
	if err != nil {
		return nil, fmt.Errorf("wiki: pinned ids: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("wiki: pinned scan: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// PinnedPage is a flat shape used by the Home pins endpoint —
// carries enough to render a Home row and deep-link back into the
// page without follow-up fetches. IsPersonal distinguishes a note
// (a page in the user's personal book) from a wiki page so Home can
// route the two surfaces correctly.
type PinnedPage struct {
	PageID     string
	PageSlug   string
	Title      string
	BookID     string
	BookSlug   string
	BookName   string
	IsPersonal bool
	UpdatedAt  time.Time
}

// ListPinnedPages returns the wiki pages this user has pinned across
// every book they're a member of — including the personal book,
// whose pages are the workspace's "notes". Filters out deleted pages
// and archived books. Ordered newest-touched first so the Home pins
// section can merge it with chats without a re-sort.
//
// This is the single source of truth for note + wiki home pins:
// pin state lives only in user_page_prefs, so a deleted page can't
// leave a stale pin (the legacy notes.pinned column is not consulted).
func (s *WikiStore) ListPinnedPages(ctx context.Context, userID string) ([]PinnedPage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT wp.id::text, wp.slug, wp.title, wp.updated_at,
		       b.id::text, b.slug, b.name, b.is_personal
		  FROM user_page_prefs upp
		  JOIN wiki_pages wp ON wp.id = upp.page_id
		  JOIN books      b  ON b.id  = wp.book_id
		  JOIN book_members m ON m.book_id = b.id AND m.user_id = upp.user_id
		 WHERE upp.user_id = $1
		   AND upp.pinned = true
		   AND wp.deleted_at IS NULL
		   AND b.archived_at IS NULL
		 ORDER BY wp.updated_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("wiki: list pinned pages: %w", err)
	}
	defer rows.Close()
	out := make([]PinnedPage, 0)
	for rows.Next() {
		var p PinnedPage
		if err := rows.Scan(&p.PageID, &p.PageSlug, &p.Title, &p.UpdatedAt,
			&p.BookID, &p.BookSlug, &p.BookName, &p.IsPersonal); err != nil {
			return nil, fmt.Errorf("wiki: pinned pages scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// IsPagePinned reports whether one specific page is pinned by a user.
// Used by the notes facade's get-by-id path where the per-row pinned
// state matters but PinnedPageIDs would over-fetch.
func (s *WikiStore) IsPagePinned(ctx context.Context, userID, pageID string) (bool, error) {
	var pinned bool
	err := s.db.QueryRowContext(ctx, `
		SELECT pinned FROM user_page_prefs
		 WHERE user_id = $1 AND page_id = $2::uuid`,
		userID, pageID).Scan(&pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("wiki: is pinned: %w", err)
	}
	return pinned, nil
}

// CreatePage inserts a new page in a book, then writes its first
// revision. slug auto-generates from title when blank; manual slug
// is honored. The transaction guarantees there's always at least
// one revision per page. See PRECONDITION above (caller must have
// verified bookID membership AND that userID has write capability).
func (s *WikiStore) CreatePage(ctx context.Context, bookID, userID, title, content, requestedSlug string) (*WikiPage, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("wiki: create: title required")
	}
	base := slugify(requestedSlug)
	if requestedSlug == "" {
		base = slugify(title)
	}
	slug, err := s.uniquePageSlug(ctx, bookID, base)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("wiki: begin tx: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
		INSERT INTO wiki_pages (book_id, slug, title, content, created_by, updated_by, edit_count)
		VALUES ($1::uuid, $2, $3, $4, $5, $5, 1)
		RETURNING id::text, book_id::text, slug, title, content,
		          parent_id::text, sort_order,
		          created_by, updated_by, maintained_by, edit_count,
		          created_at, updated_at, deleted_at`,
		bookID, slug, title, content, userID)
	var p WikiPage
	var parentID sql.NullString
	var maintainedBy sql.NullString
	if err := row.Scan(&p.ID, &p.BookID, &p.Slug, &p.Title, &p.Content,
		&parentID, &p.SortOrder,
		&p.CreatedBy, &p.UpdatedBy, &maintainedBy, &p.EditCount,
		&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
		return nil, fmt.Errorf("wiki: insert page: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO wiki_revisions (page_id, content, edited_by)
		VALUES ($1::uuid, $2, $3)`, p.ID, content, userID); err != nil {
		return nil, fmt.Errorf("wiki: insert revision: %w", err)
	}
	// Bump the parent book's updated_at so it bubbles to top of
	// the books list. Same pattern conversations + notes use.
	if _, err := tx.ExecContext(ctx, `
		UPDATE books SET updated_at = NOW() WHERE id = $1::uuid`, bookID); err != nil {
		return nil, fmt.Errorf("wiki: bump book: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("wiki: commit: %w", err)
	}
	if parentID.Valid {
		p.ParentID = &parentID.String
	}
	if maintainedBy.Valid {
		p.MaintainedBy = &maintainedBy.String
	}
	// Sync-rebuild the [[]] link index from the new content. We do
	// this after the page tx commits so a link-resolution failure
	// never blocks the create — the page exists either way and the
	// next save will re-index. Logged for visibility.
	if err := s.ReplacePageLinks(ctx, p.ID, bookID, ParseLinks(content)); err != nil {
		fmt.Printf("[wiki] link index failed (page=%s book=%s): %v\n", p.ID, bookID, err)
	}
	s.fireSavedAfterIndex(ctx, &p, bookID, userID)
	return &p, nil
}

// fireSavedAfterIndex resolves the bookSlug + outbound link list
// and dispatches the page-saved hook. Called after the link index
// is rebuilt so the hook sees the freshly resolved links. Quiet
// on lookup failures — the hook is best-effort.
func (s *WikiStore) fireSavedAfterIndex(ctx context.Context, page *WikiPage, bookID, userID string) {
	if s.pageSaved == nil {
		return
	}
	bookSlug, err := s.bookSlugByID(ctx, bookID)
	if err != nil {
		fmt.Printf("[wiki] page-saved hook skipped (book slug lookup failed): %v\n", err)
		return
	}
	links, err := s.ListPageLinks(ctx, page.ID)
	if err != nil {
		fmt.Printf("[wiki] page-saved hook skipped (list links failed): %v\n", err)
		return
	}
	// Shard id (if any) rides in via the request context — handlers
	// stash it with WithPageActor before calling UpdatePage /
	// CreatePage. Skill-driven and other internal callers leave it
	// empty.
	shardID := PageActorFromContext(ctx).ShardID
	s.firePageSaved(*page, bookSlug, userID, shardID, links)
}

// bookSlugByID is a one-column read used by fireSavedAfterIndex
// + the delete hook. Cheap, indexed lookup.
func (s *WikiStore) bookSlugByID(ctx context.Context, bookID string) (string, error) {
	var slug string
	err := s.db.QueryRowContext(ctx,
		`SELECT slug FROM books WHERE id = $1::uuid`, bookID).Scan(&slug)
	if err != nil {
		return "", fmt.Errorf("bookSlugByID: %w", err)
	}
	return slug, nil
}

// PagePatch is the dynamic-update payload. Nil fields are ignored.
// Setting Title or Content writes a new revision.
type PagePatch struct {
	Title   *string
	Content *string
	Slug    *string // explicit rename only — auto-generation is create-time only
	Summary *string // optional change summary attached to the new revision

	// IfMatch is an optional precondition. When non-nil, UpdatePage
	// compares it against the row's current updated_at and returns
	// ErrPageStale if they differ — i.e. another writer landed an
	// edit between the caller's read and this write. The HTTP layer
	// surfaces this via the If-Match request header.
	IfMatch *time.Time
}

// UpdatePage applies a patch and writes a new revision when content
// or title actually changes. The transaction keeps page + revision
// consistent. Phase 3 will also recompute maintained_by + edit_count
// here; for now we increment edit_count only. See PRECONDITION above
// (caller must have verified bookID membership AND that userID has
// write capability).
func (s *WikiStore) UpdatePage(ctx context.Context, bookID, pageSlug, userID string, p PagePatch) (*WikiPage, error) {
	// A content-only save carrying a precondition is the merge-eligible
	// case: if the base moved under us we can three-way merge the body
	// instead of rejecting. Title/slug edits aren't line-mergeable, so a
	// stale precondition there is always a hard conflict.
	mergeEligible := p.IfMatch != nil && p.Content != nil && p.Title == nil && p.Slug == nil

	// Retry loop: each attempt re-reads the current row, resolves the
	// write (fresh, or auto-merged against the base revision), then does
	// an atomic compare-and-swap on the version it read. If a concurrent
	// writer lands between the read and the CAS, the CAS affects zero
	// rows and we loop to re-read and re-merge against the new head. The
	// bound stops a pathological hot-page livelock; exhausting it surfaces
	// as stale so the caller can fall back to a manual choice.
	const maxAttempts = 4
	for attempt := 0; ; attempt++ {
		out, retry, err := s.updatePageOnce(ctx, bookID, pageSlug, userID, p, mergeEligible)
		if err == nil {
			return out, nil
		}
		if retry && attempt < maxAttempts-1 {
			continue
		}
		return nil, err
	}
}

// updatePageOnce is one attempt of UpdatePage. It returns retry=true when
// an atomic CAS lost to a concurrent writer AND the edit is merge-eligible
// (so re-reading and re-merging is worthwhile); the caller loops. All other
// errors are terminal.
func (s *WikiStore) updatePageOnce(ctx context.Context, bookID, pageSlug, userID string, p PagePatch, mergeEligible bool) (_ *WikiPage, retry bool, _ error) {
	cur, err := s.GetPage(ctx, bookID, pageSlug)
	if err != nil {
		return nil, false, err
	}

	newTitle := cur.Title
	newContent := cur.Content
	newSlug := cur.Slug
	contentChanged := false
	merged := false

	// Optimistic concurrency. Truncate both sides to microsecond
	// precision before comparing — Postgres returns timestamps at
	// microsecond resolution, the wire format may serialise at
	// nanoseconds (Go default), and an exact-equality check after a
	// JSON round-trip would otherwise spuriously 409 on the first
	// post-load write.
	stale := p.IfMatch != nil && !microEqual(cur.UpdatedAt, *p.IfMatch)
	if stale {
		if !mergeEligible {
			// Title/slug edit against a moved base — not line-mergeable.
			return nil, false, ErrPageStale
		}
		// Recover the base revision the client edited from. Its created_at
		// equals the page's updated_at at that version (both stamped from
		// the same transaction_timestamp), so the If-Match value uniquely
		// identifies it — the anchor for a three-way merge.
		base, ok := s.revisionContentAt(ctx, cur.ID, *p.IfMatch)
		if !ok {
			// Base predates revision history or was pruned; can't merge.
			return nil, false, ErrPageStale
		}
		m, conflict := textmerge.Merge(base, *p.Content, cur.Content)
		if conflict {
			// Both sides changed the same region differently.
			return nil, false, ErrPageStale
		}
		if m != cur.Content {
			newContent = m
			contentChanged = true
		}
		merged = true
	} else {
		if p.Title != nil && *p.Title != cur.Title {
			newTitle = *p.Title
			// Auto-derive slug from the new title unless the caller
			// explicitly provided a different slug. This keeps the
			// slug in sync with the title — no hidden identifiers.
			if p.Slug == nil || *p.Slug == "" {
				base := slugify(newTitle)
				if base != cur.Slug {
					ns, err := s.uniquePageSlug(ctx, bookID, base)
					if err != nil {
						return nil, false, err
					}
					newSlug = ns
				}
			}
		}
		if p.Content != nil && *p.Content != cur.Content {
			newContent = *p.Content
			contentChanged = true
		}
		if p.Slug != nil && *p.Slug != "" && *p.Slug != cur.Slug {
			// Validate uniqueness before committing.
			base := slugify(*p.Slug)
			ns, err := s.uniquePageSlug(ctx, bookID, base)
			if err != nil {
				return nil, false, err
			}
			newSlug = ns
		}
	}
	noOp := newTitle == cur.Title && newContent == cur.Content && newSlug == cur.Slug
	if noOp {
		// Nothing to persist. If we got here via a merge (the client's
		// edit was already subsumed by the current content), still signal
		// Merged so the client reseeds its baseline to the live version.
		if merged {
			cur.Merged = true
		}
		return cur, false, nil
	}

	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("wiki: begin tx: %w", err)
	}
	defer tx.Rollback()

	bumpEdit := 0
	if contentChanged {
		bumpEdit = 1
	}
	// Compare-and-swap: when a precondition was supplied, guard the
	// UPDATE itself on the version we just read (cur.UpdatedAt — which
	// equals the client's If-Match on a fresh save, or the merge base on
	// a merged save). GetPage read the row OUTSIDE this transaction, so a
	// concurrent write landing between that read and this UPDATE would
	// otherwise be silently overwritten (the reported husband/wife
	// clobber). Guarding here makes the check-and-write atomic: zero rows
	// affected => someone beat us => retry (re-read + re-merge) or stale.
	// Micro-second truncation matches the wire round-trip (Postgres
	// stores µs; Go serialises ns), same as the fast-path compare.
	whereCAS := ""
	args := []any{newTitle, newContent, newSlug, userID, bumpEdit, cur.ID}
	if p.IfMatch != nil {
		whereCAS = " AND date_trunc('microseconds', updated_at) = date_trunc('microseconds', $7::timestamptz)"
		args = append(args, cur.UpdatedAt.UTC())
	}
	row := tx.QueryRowContext(ctx, `
		UPDATE wiki_pages
		   SET title       = $1,
		       content     = $2,
		       slug        = $3,
		       updated_by  = $4,
		       updated_at  = NOW(),
		       edit_count  = edit_count + $5
		 WHERE id = $6::uuid`+whereCAS+`
		RETURNING id::text, book_id::text, slug, title, content,
		          parent_id::text, sort_order,
		          created_by, updated_by, maintained_by, edit_count,
		          created_at, updated_at, deleted_at`,
		args...)
	var out WikiPage
	var parentID sql.NullString
	var maintainedBy sql.NullString
	if err := row.Scan(&out.ID, &out.BookID, &out.Slug, &out.Title, &out.Content,
		&parentID, &out.SortOrder,
		&out.CreatedBy, &out.UpdatedBy, &maintainedBy, &out.EditCount,
		&out.CreatedAt, &out.UpdatedAt, &out.DeletedAt); err != nil {
		if err == sql.ErrNoRows {
			// CAS lost: the row's updated_at no longer matches the version
			// we read (a concurrent write won between our read and write).
			// For a mergeable edit, retry to re-merge against the new head;
			// otherwise surface as stale.
			return nil, mergeEligible, ErrPageStale
		}
		return nil, false, fmt.Errorf("wiki: update: %w", err)
	}

	if contentChanged {
		summary := ""
		if p.Summary != nil {
			summary = *p.Summary
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO wiki_revisions (page_id, content, edited_by, summary)
			VALUES ($1::uuid, $2, $3, NULLIF($4, ''))`,
			out.ID, newContent, userID, summary); err != nil {
			return nil, false, fmt.Errorf("wiki: insert revision: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE books SET updated_at = NOW() WHERE id = $1::uuid`, bookID); err != nil {
		return nil, false, fmt.Errorf("wiki: bump book: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("wiki: commit: %w", err)
	}
	if parentID.Valid {
		out.ParentID = &parentID.String
	}
	if maintainedBy.Valid {
		out.MaintainedBy = &maintainedBy.String
	}
	out.Merged = merged
	// Re-index [[]] links when the body changed. Title-only edits
	// don't need a re-pass since titles aren't part of the link
	// graph (only the slug is). Log on failure but don't propagate.
	if contentChanged {
		if err := s.ReplacePageLinks(ctx, out.ID, bookID, ParseLinks(newContent)); err != nil {
			fmt.Printf("[wiki] link index failed (page=%s book=%s): %v\n", out.ID, bookID, err)
		}
	}
	// Fire the saved hook on every successful update — even title-
	// only edits matter for the hook (sidecar wants the title in
	// context for fact extraction even if the body didn't move).
	s.fireSavedAfterIndex(ctx, &out, bookID, userID)
	return &out, false, nil
}

// microEqual compares two timestamps at microsecond precision — the
// resolution Postgres stores and the wire round-trip preserves. Used for
// If-Match / CAS checks where an exact ns-level compare would spuriously
// fail after a JSON round-trip.
func microEqual(a, b time.Time) bool {
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// revisionContentAt returns the content of the revision whose created_at
// matches `at` at microsecond precision, and whether one was found. A
// page's updated_at and the revision written in that same transaction share
// one transaction_timestamp, so a past updated_at (an If-Match value)
// uniquely identifies the base revision a client edited from — the anchor
// for a server-side three-way merge. Returns ("", false) when no such
// revision exists (base predates history, or was pruned).
func (s *WikiStore) revisionContentAt(ctx context.Context, pageID string, at time.Time) (string, bool) {
	var content string
	err := s.db.DB.QueryRowContext(ctx, `
		SELECT content FROM wiki_revisions
		 WHERE page_id = $1::uuid
		   AND date_trunc('microseconds', created_at) = date_trunc('microseconds', $2::timestamptz)
		 ORDER BY created_at DESC
		 LIMIT 1`, pageID, at.UTC()).Scan(&content)
	if err != nil {
		return "", false
	}
	return content, true
}

// AppendPage atomically appends a paragraph to the bottom of a page's
// body in a SINGLE UPDATE — no read-modify-write, so two concurrent
// appends can't clobber each other (each is an independent in-DB
// concatenation, and neither needs a precondition). Records a full
// revision and fires the saved hook, like UpdatePage. Used by the
// /append endpoint and the agent append_to_page tool, both of which
// previously composed GetPage+UpdatePage with a lost-update window.
func (s *WikiStore) AppendPage(ctx context.Context, bookID, pageID, userID, text string) (*WikiPage, error) {
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("wiki: begin tx: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
		UPDATE wiki_pages
		   SET content = CASE
		           WHEN content IS NULL OR content = '' THEN $1
		           ELSE rtrim(content, E'\n') || E'\n\n' || $1
		       END,
		       updated_by = $2,
		       updated_at = NOW(),
		       edit_count = edit_count + 1
		 WHERE id = $3::uuid AND book_id = $4::uuid AND deleted_at IS NULL
		RETURNING id::text, book_id::text, slug, title, content,
		          parent_id::text, sort_order,
		          created_by, updated_by, maintained_by, edit_count,
		          created_at, updated_at, deleted_at`,
		text, userID, pageID, bookID)
	var out WikiPage
	var parentID, maintainedBy sql.NullString
	if err := row.Scan(&out.ID, &out.BookID, &out.Slug, &out.Title, &out.Content,
		&parentID, &out.SortOrder, &out.CreatedBy, &out.UpdatedBy, &maintainedBy,
		&out.EditCount, &out.CreatedAt, &out.UpdatedAt, &out.DeletedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrPageNotFound
		}
		return nil, fmt.Errorf("wiki: append: %w", err)
	}
	if parentID.Valid {
		out.ParentID = &parentID.String
	}
	if maintainedBy.Valid {
		out.MaintainedBy = &maintainedBy.String
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO wiki_revisions (page_id, content, edited_by, summary)
		VALUES ($1::uuid, $2, $3, NULL)`, out.ID, out.Content, userID); err != nil {
		return nil, fmt.Errorf("wiki: insert revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE books SET updated_at = NOW() WHERE id = $1::uuid`, bookID); err != nil {
		return nil, fmt.Errorf("wiki: bump book: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("wiki: commit: %w", err)
	}

	if err := s.ReplacePageLinks(ctx, out.ID, bookID, ParseLinks(out.Content)); err != nil {
		fmt.Printf("[wiki] link index failed (page=%s book=%s): %v\n", out.ID, bookID, err)
	}
	s.fireSavedAfterIndex(ctx, &out, bookID, userID)
	return &out, nil
}

// ErrInvalidParent is returned when a MovePage request names a
// parent that doesn't exist in the same book, is soft-deleted, is
// the page itself, or would create a cycle (an ancestor of the new
// parent IS the page being moved).
var ErrInvalidParent = errors.New("wiki: invalid parent")

// MovePage reparents a page and/or changes its position among its
// siblings. newParentID is the destination parent ("" unparents to
// top-level, a uuid moves under that page). sortOrder is optional;
// nil appends to the end of the new sibling list.
//
// Validation chain (every check 4xx-shaped, not a 500):
//
//   - The page must exist in this book and be live (not deleted).
//   - newParentID != pageID — a page can't be its own parent.
//   - The new parent (if any) must exist in this book and be live.
//   - The new parent must not be a descendant of the page being
//     moved — otherwise the move would orphan a subtree by closing
//     the loop on itself. Checked with a recursive CTE up the new
//     parent's ancestor chain; if it hits pageID we reject.
//
// All checks + the UPDATE run in a single transaction so a parallel
// move can't slip in between validation and write.
//
// See PRECONDITION above (caller must have verified bookID
// membership AND that userID has write capability).
func (s *WikiStore) MovePage(ctx context.Context, bookID, pageID, newParentID string, sortOrder *int) (*WikiPage, error) {
	if pageID == "" {
		return nil, ErrPageNotFound
	}
	if newParentID == pageID {
		return nil, ErrInvalidParent
	}
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("wiki: move: begin: %w", err)
	}
	defer tx.Rollback()

	// Pin the page we're moving — also acts as our existence +
	// book-scoping + live-status check.
	var (
		curBookID  string
		curDeleted sql.NullTime
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT book_id::text, deleted_at FROM wiki_pages
		 WHERE id = $1::uuid AND book_id = $2::uuid`,
		pageID, bookID,
	).Scan(&curBookID, &curDeleted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPageNotFound
		}
		return nil, fmt.Errorf("wiki: move: lookup page: %w", err)
	}
	if curDeleted.Valid {
		return nil, ErrPageNotFound
	}

	// Validate the new parent, if any. Empty = unparent → no check.
	if newParentID != "" {
		var (
			parentBookID  string
			parentDeleted sql.NullTime
		)
		if err := tx.QueryRowContext(ctx, `
			SELECT book_id::text, deleted_at FROM wiki_pages
			 WHERE id = $1::uuid`,
			newParentID,
		).Scan(&parentBookID, &parentDeleted); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrInvalidParent
			}
			return nil, fmt.Errorf("wiki: move: lookup parent: %w", err)
		}
		if parentBookID != bookID || parentDeleted.Valid {
			return nil, ErrInvalidParent
		}
		// Cycle check: would pageID be an ancestor of newParentID
		// after this move? Walk up newParentID's ancestor chain via
		// a recursive CTE and look for pageID. A hit means the move
		// would create a cycle.
		var cycle bool
		if err := tx.QueryRowContext(ctx, `
			WITH RECURSIVE ancestors AS (
			    SELECT id, parent_id FROM wiki_pages WHERE id = $1::uuid
			    UNION
			    SELECT wp.id, wp.parent_id
			      FROM wiki_pages wp
			      JOIN ancestors a ON wp.id = a.parent_id
			)
			SELECT EXISTS(SELECT 1 FROM ancestors WHERE id = $2::uuid)`,
			newParentID, pageID,
		).Scan(&cycle); err != nil {
			return nil, fmt.Errorf("wiki: move: cycle check: %w", err)
		}
		if cycle {
			return nil, ErrInvalidParent
		}
	}

	// Resolve the sort_order. nil → append to the new sibling list.
	// Siblings are identified by parent_id (NULL for top-level),
	// scoped to this book.
	resolvedSort := 0
	if sortOrder != nil {
		resolvedSort = *sortOrder
	} else {
		row := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(sort_order), -1) + 1
			  FROM wiki_pages
			 WHERE book_id = $1::uuid
			   AND deleted_at IS NULL
			   AND id <> $2::uuid
			   AND ((parent_id IS NULL AND $3 = '')
			     OR  parent_id::text = NULLIF($3, ''))`,
			bookID, pageID, newParentID,
		)
		if err := row.Scan(&resolvedSort); err != nil {
			return nil, fmt.Errorf("wiki: move: max sort: %w", err)
		}
	}

	// Apply. Empty newParentID sets parent_id NULL; otherwise cast
	// to uuid. NULLIF + ::uuid keeps the single statement readable.
	row := tx.QueryRowContext(ctx, `
		UPDATE wiki_pages
		   SET parent_id  = NULLIF($1, '')::uuid,
		       sort_order = $2,
		       updated_at = NOW()
		 WHERE id = $3::uuid
		RETURNING id::text, book_id::text, slug, title, content,
		          parent_id::text, sort_order,
		          created_by, updated_by, maintained_by, edit_count,
		          created_at, updated_at, deleted_at`,
		newParentID, resolvedSort, pageID,
	)
	var out WikiPage
	var parentID sql.NullString
	var maintainedBy sql.NullString
	if err := row.Scan(&out.ID, &out.BookID, &out.Slug, &out.Title, &out.Content,
		&parentID, &out.SortOrder,
		&out.CreatedBy, &out.UpdatedBy, &maintainedBy, &out.EditCount,
		&out.CreatedAt, &out.UpdatedAt, &out.DeletedAt); err != nil {
		return nil, fmt.Errorf("wiki: move: update: %w", err)
	}
	if parentID.Valid {
		out.ParentID = &parentID.String
	}
	if maintainedBy.Valid {
		out.MaintainedBy = &maintainedBy.String
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("wiki: move: commit: %w", err)
	}
	return &out, nil
}

// DeletePage soft-deletes a page. The retention cron is responsible
// for hard-purging. Phase 1d's stale-fact cleanup hangs off this
// same path. See PRECONDITION above (caller must have verified
// bookID membership AND write capability).
func (s *WikiStore) DeletePage(ctx context.Context, bookID, pageSlug string) error {
	// RETURNING so we can hand the page id to the deletion hook
	// without a second round-trip. ErrPageNotFound on no rows.
	var pageID string
	err := s.db.QueryRowContext(ctx, `
		UPDATE wiki_pages
		   SET deleted_at = NOW(), updated_at = NOW()
		 WHERE book_id = $1::uuid AND slug = $2 AND deleted_at IS NULL
		 RETURNING id::text`,
		bookID, pageSlug).Scan(&pageID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPageNotFound
	}
	if err != nil {
		return fmt.Errorf("wiki: delete: %w", err)
	}
	if s.pageDeleted != nil {
		bookSlug, slugErr := s.bookSlugByID(ctx, bookID)
		if slugErr != nil {
			fmt.Printf("[wiki] page-deleted hook skipped (book slug lookup failed): %v\n", slugErr)
		} else {
			s.firePageDeleted(bookID, bookSlug, pageID, pageSlug)
		}
	}
	return nil
}

// SweepResearchEvidence soft-deletes evidence pages in the hidden
// per-user research books whose updated_at is older than olderThan.
// Evidence pages are transient worker scratch — once a run's note is
// written (or the run is abandoned) the page has no further use, and
// because the research books are hidden from every listing they can't
// be cleaned up by hand. This sweep is therefore the only bound on
// their growth: without it, one page per deep-research run would pile
// up invisibly forever. It fires the page-deleted hook per page so the
// wikiknowledge facts extracted from the evidence go too. Returns the
// number of pages swept.
func (s *WikiStore) SweepResearchEvidence(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		UPDATE wiki_pages p
		   SET deleted_at = NOW(), updated_at = NOW()
		  FROM books b
		 WHERE b.id = p.book_id
		   AND b.slug LIKE 'research:%'
		   AND p.deleted_at IS NULL
		   AND p.updated_at < NOW() - make_interval(secs => $1)
		 RETURNING p.book_id::text, b.slug, p.id::text, p.slug`,
		olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("wiki: sweep research evidence: %w", err)
	}
	type swept struct{ bookID, bookSlug, pageID, pageSlug string }
	var got []swept
	for rows.Next() {
		var sv swept
		if err := rows.Scan(&sv.bookID, &sv.bookSlug, &sv.pageID, &sv.pageSlug); err != nil {
			rows.Close()
			return 0, fmt.Errorf("wiki: sweep scan: %w", err)
		}
		got = append(got, sv)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	// Fire hooks after the UPDATE commits (the scan drained the rows),
	// so extracted facts get cleaned for each reaped page.
	if s.pageDeleted != nil {
		for _, sv := range got {
			s.firePageDeleted(sv.bookID, sv.bookSlug, sv.pageID, sv.pageSlug)
		}
	}
	return len(got), nil
}

// ── Link parser + index ──────────────────────────────────────────
//
// Wiki links use [[]] syntax. The parser is a regex pass over the
// page body, run synchronously after every Create/Update so the
// graph stays consistent with the latest content. See
// BOOKS-WIKI-ARCHITECTURE Phase 1 step 3.

// ParsedLink is one [[]] occurrence after parsing. TargetBookSlug
// is empty for same-book links. DisplayText is empty unless the
// author wrote [[target|display].
type ParsedLink struct {
	TargetBookSlug string
	TargetPageSlug string
	DisplayText    string
}

// PageLink is one persisted row — the subset of wiki_page_links
// we surface back to API callers. target_page_id can be NULL when
// the link points at a page that doesn't exist yet (broken link),
// so the resolved fields are pointers.
type PageLink struct {
	SourcePageID    string  `json:"source_page_id"`
	TargetBookSlug  string  `json:"target_book_slug,omitempty"` // empty = same book
	TargetPageSlug  string  `json:"target_page_slug"`
	TargetPageID    *string `json:"target_page_id,omitempty"` // null = broken link
	TargetPageTitle string  `json:"target_page_title,omitempty"`
	DisplayText     string  `json:"display_text,omitempty"`
}

// Backlink is one inbound link, joined back to the source page +
// book so callers can render a "Linked from" list without
// additional round-trips.
type Backlink struct {
	SourcePageID    string `json:"source_page_id"`
	SourcePageSlug  string `json:"source_page_slug"`
	SourcePageTitle string `json:"source_page_title"`
	SourceBookSlug  string `json:"source_book_slug"`
	SourceBookName  string `json:"source_book_name"`
	DisplayText     string `json:"display_text,omitempty"`
}

// wikiLinkRe matches [[content]] non-greedily. Disallows ] inside
// the brackets so adjacent links don't merge. Empty / whitespace
// links are filtered out at parse time.
var wikiLinkRe = regexp.MustCompile(`\[\[([^\]]+)\]\]`)

// ParseLinks extracts every [[]] occurrence from the markdown body
// and normalizes targets to lowercase. Pure function — no DB. Four
// link forms are supported per the architecture doc:
//
//	[[slug]]                 — same-book link
//	[[slug|Display]]         — same-book link with display text
//	[[book/slug]]            — cross-book link
//	[[book/slug|Display]]    — cross-book link with display text
//
// Duplicate (book, page) pairs in the same body are collapsed to
// the last occurrence so the wiki_page_links unique index
// (idx_wiki_page_links_pk on source + COALESCE(book_slug, ”) +
// page_slug) stays happy.
//
// Known limitation: this does not skip [[]] occurrences inside
// fenced code blocks. Authors who need to discuss the link syntax
// itself can escape with surrounding code spans on the rendered
// side; the index treats every match the same way. We can layer
// a code-block scan in later if it proves to be a real problem.
func ParseLinks(body string) []ParsedLink {
	matches := wikiLinkRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	// Use a map to de-dupe by (book, page) — last entry wins so
	// the most recent display text in the body is kept.
	type key struct{ book, page string }
	seen := make(map[key]ParsedLink, len(matches))
	order := make([]key, 0, len(matches))
	for _, m := range matches {
		inner := strings.TrimSpace(m[1])
		if inner == "" {
			continue
		}
		var target, display string
		if pipe := strings.Index(inner, "|"); pipe >= 0 {
			target = strings.TrimSpace(inner[:pipe])
			display = strings.TrimSpace(inner[pipe+1:])
		} else {
			target = inner
		}
		var book, page string
		if slash := strings.Index(target, "/"); slash >= 0 {
			book = strings.TrimSpace(target[:slash])
			page = strings.TrimSpace(target[slash+1:])
		} else {
			page = target
		}
		book = strings.ToLower(book)
		page = strings.ToLower(page)
		if page == "" {
			continue
		}
		k := key{book, page}
		if _, ok := seen[k]; !ok {
			order = append(order, k)
		}
		seen[k] = ParsedLink{TargetBookSlug: book, TargetPageSlug: page, DisplayText: display}
	}
	out := make([]ParsedLink, 0, len(order))
	for _, k := range order {
		out = append(out, seen[k])
	}
	return out
}

// ReplacePageLinks replaces every wiki_page_links row for source-
// PageID with the supplied list. Resolves each link's target_-
// page_id by looking up the target book + page slug — a NULL
// target_page_id is the canonical signal for a broken link. Atomic
// (DELETE + INSERT in one tx) so backlinks queries never observe
// a half-rebuilt index.
//
// PRECONDITION: caller has verified write access on sourceBookID
// (via the same path that protects UpdatePage). This method does
// not re-check.
func (s *WikiStore) ReplacePageLinks(ctx context.Context, sourcePageID, sourceBookID string, links []ParsedLink) error {
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("wiki: links begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM wiki_page_links WHERE source_page_id = $1::uuid`,
		sourcePageID); err != nil {
		return fmt.Errorf("wiki: links delete: %w", err)
	}

	for _, l := range links {
		// Resolve the target book id. Same-book (empty
		// TargetBookSlug) reuses sourceBookID; cross-book looks
		// up the slug. A miss yields a broken link (NULL
		// target_page_id) — same outcome as an unresolved page.
		var targetBookID sql.NullString
		if l.TargetBookSlug == "" {
			targetBookID = sql.NullString{String: sourceBookID, Valid: true}
		} else {
			var id string
			err := tx.QueryRowContext(ctx,
				`SELECT id::text FROM books WHERE slug = $1`,
				l.TargetBookSlug).Scan(&id)
			if err == nil {
				targetBookID = sql.NullString{String: id, Valid: true}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("wiki: links resolve book: %w", err)
			}
			// errors.Is(err, sql.ErrNoRows) → leave targetBookID
			// invalid; target_page_id will be NULL below.
		}

		var targetPageID sql.NullString
		if targetBookID.Valid {
			var id string
			err := tx.QueryRowContext(ctx,
				`SELECT id::text FROM wiki_pages
				 WHERE book_id = $1::uuid AND slug = $2 AND deleted_at IS NULL`,
				targetBookID.String, l.TargetPageSlug).Scan(&id)
			if err == nil {
				targetPageID = sql.NullString{String: id, Valid: true}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("wiki: links resolve page: %w", err)
			}
		}

		var bookSlug sql.NullString
		if l.TargetBookSlug != "" {
			bookSlug = sql.NullString{String: l.TargetBookSlug, Valid: true}
		}
		var display sql.NullString
		if l.DisplayText != "" {
			display = sql.NullString{String: l.DisplayText, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO wiki_page_links
			    (source_page_id, target_book_slug, target_page_slug,
			     target_page_id, display_text)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5)`,
			sourcePageID, bookSlug, l.TargetPageSlug, targetPageID, display,
		); err != nil {
			return fmt.Errorf("wiki: links insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("wiki: links commit: %w", err)
	}
	return nil
}

// ListPageLinks returns the outbound links of a page, joining to
// wiki_pages so callers can render the resolved target title.
// Broken links keep TargetPageID = nil and TargetPageTitle = "".
//
// PRECONDITION: caller has verified read access on the SOURCE
// book via the page's bookID. This method does not re-check.
func (s *WikiStore) ListPageLinks(ctx context.Context, sourcePageID string) ([]PageLink, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.source_page_id::text,
		       COALESCE(l.target_book_slug, ''),
		       l.target_page_slug,
		       l.target_page_id::text,
		       COALESCE(wp.title, ''),
		       COALESCE(l.display_text, '')
		  FROM wiki_page_links l
		  LEFT JOIN wiki_pages wp ON wp.id = l.target_page_id
		   AND wp.deleted_at IS NULL
		 WHERE l.source_page_id = $1::uuid
		 ORDER BY l.target_page_slug`, sourcePageID)
	if err != nil {
		return nil, fmt.Errorf("wiki: list links: %w", err)
	}
	defer rows.Close()
	out := make([]PageLink, 0)
	for rows.Next() {
		var pl PageLink
		var targetID sql.NullString
		if err := rows.Scan(&pl.SourcePageID, &pl.TargetBookSlug,
			&pl.TargetPageSlug, &targetID, &pl.TargetPageTitle, &pl.DisplayText,
		); err != nil {
			return nil, fmt.Errorf("wiki: link scan: %w", err)
		}
		if targetID.Valid {
			pl.TargetPageID = &targetID.String
		}
		out = append(out, pl)
	}
	return out, rows.Err()
}

// ListBacklinks returns every page that links INTO targetPageID,
// with source page + book metadata for rendering. Used by the
// page-view "Linked from" section.
//
// PRECONDITION: caller has verified read access on the TARGET
// page's book. This method does not re-check — and notably, the
// returned source pages may live in books the caller is NOT a
// member of (cross-book inbound link). That's intentional per the
// architecture doc: backlinks reveal that a page is referenced
// elsewhere; whether the caller can navigate to that source is a
// separate read check the handler / frontend performs. We surface
// only the source book/page slug + title here, no body content,
// so a non-member of the source book learns only that an inbound
// link exists. Phase 3 may swap to "locked link" rendering.
func (s *WikiStore) ListBacklinks(ctx context.Context, targetPageID string) ([]Backlink, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT wp.id::text, wp.slug, wp.title,
		       b.slug, b.name,
		       COALESCE(l.display_text, '')
		  FROM wiki_page_links l
		  JOIN wiki_pages wp ON wp.id = l.source_page_id
		   AND wp.deleted_at IS NULL
		  JOIN books b ON b.id = wp.book_id
		 WHERE l.target_page_id = $1::uuid
		 ORDER BY b.name, wp.title`, targetPageID)
	if err != nil {
		return nil, fmt.Errorf("wiki: backlinks: %w", err)
	}
	defer rows.Close()
	out := make([]Backlink, 0)
	for rows.Next() {
		var bl Backlink
		if err := rows.Scan(&bl.SourcePageID, &bl.SourcePageSlug, &bl.SourcePageTitle,
			&bl.SourceBookSlug, &bl.SourceBookName, &bl.DisplayText,
		); err != nil {
			return nil, fmt.Errorf("wiki: backlink scan: %w", err)
		}
		out = append(out, bl)
	}
	return out, rows.Err()
}

// ── Revisions ─────────────────────────────────────────────────────
//
// PRECONDITION: revisions are scoped to a page that lives in a book.
// Callers MUST resolve the page through bookID-scoped GetPage first
// (which itself requires a membership-vetted bookID — see Page CRUD
// PRECONDITION). The revision SQL below filters by page_id only and
// would otherwise leak across books if called with an unverified
// pageID.

// ListRevisions returns the change history for a page, newest first.
// See PRECONDITION above.
func (s *WikiStore) ListRevisions(ctx context.Context, pageID string) ([]WikiRevision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, page_id::text, content, edited_by, created_at, COALESCE(summary, '')
		  FROM wiki_revisions
		 WHERE page_id = $1::uuid
		 ORDER BY created_at DESC`, pageID)
	if err != nil {
		return nil, fmt.Errorf("wiki: revisions: %w", err)
	}
	defer rows.Close()
	out := make([]WikiRevision, 0)
	for rows.Next() {
		var r WikiRevision
		if err := rows.Scan(&r.ID, &r.PageID, &r.Content, &r.EditedBy,
			&r.CreatedAt, &r.Summary); err != nil {
			return nil, fmt.Errorf("wiki: revision scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRevision fetches one specific revision by id, scoped to the
// page (so the caller can't peek at revisions across books).
func (s *WikiStore) GetRevision(ctx context.Context, pageID, revID string) (*WikiRevision, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, page_id::text, content, edited_by, created_at, COALESCE(summary, '')
		  FROM wiki_revisions
		 WHERE id = $1::uuid AND page_id = $2::uuid`, revID, pageID)
	var r WikiRevision
	err := row.Scan(&r.ID, &r.PageID, &r.Content, &r.EditedBy, &r.CreatedAt, &r.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wiki: revision get: %w", err)
	}
	return &r, nil
}

// SearchPages full-text-searches a book's pages by title + content.
// See Page CRUD PRECONDITION (caller must have verified bookID
// membership).
func (s *WikiStore) SearchPages(ctx context.Context, bookID, query string, limit int) ([]WikiPageSummary, error) {
	if strings.TrimSpace(query) == "" {
		return []WikiPageSummary{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, slug, title, content, updated_at, updated_by
		  FROM wiki_pages
		 WHERE book_id = $1::uuid
		   AND deleted_at IS NULL
		   AND to_tsvector('english', title || ' ' || content) @@ plainto_tsquery('english', $2)
		 ORDER BY ts_rank(to_tsvector('english', title || ' ' || content),
		                  plainto_tsquery('english', $2)) DESC,
		          updated_at DESC
		 LIMIT $3`, bookID, query, limit)
	if err != nil {
		return nil, fmt.Errorf("wiki: search: %w", err)
	}
	defer rows.Close()
	out := make([]WikiPageSummary, 0)
	for rows.Next() {
		var p WikiPageSummary
		var content string
		if err := rows.Scan(&p.ID, &p.Slug, &p.Title, &content,
			&p.UpdatedAt, &p.UpdatedBy); err != nil {
			return nil, fmt.Errorf("wiki: search scan: %w", err)
		}
		p.Snippet = snippetFromContent(content)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── Public-link shares ────────────────────────────────────────────
//
// One page can have at most one live public share (enforced by a
// partial unique index on (page_id) WHERE visibility='public'). The
// share_key is the public URL fragment — random alphanumeric, 16
// chars, generated here. visibility is open-ended so a future
// "authed-only" flavor can land without another migration.

// PageShare is one row from wiki_page_shares.
type PageShare struct {
	ShareKey   string    `json:"share_key"`
	PageID     string    `json:"page_id"`
	Visibility string    `json:"visibility"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	PublicURL  string    `json:"public_url,omitempty"`
}

// SharedPage is the bundle a public viewer renders: the live page +
// the share row + the sharer's user id (caller resolves the display
// name). Page is excluded when soft-deleted or the book is archived.
type SharedPage struct {
	PageID     string
	Title      string
	Content    string
	UpdatedAt  time.Time
	SharedBy   string
	Visibility string
}

// GetPageShare returns the current public share row for a page, or
// (nil, nil) when no share exists. Callers can check the result for
// whether a globe indicator should render.
func (s *WikiStore) GetPageShare(ctx context.Context, pageID string) (*PageShare, error) {
	var p PageShare
	err := s.db.QueryRowContext(ctx, `
		SELECT share_key, page_id::text, visibility, created_by, created_at
		  FROM wiki_page_shares
		 WHERE page_id = $1::uuid AND visibility = 'public'`,
		pageID).Scan(&p.ShareKey, &p.PageID, &p.Visibility, &p.CreatedBy, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wiki: get share: %w", err)
	}
	return &p, nil
}

// EnablePageShare upserts a public share row for a page. Idempotent:
// when a share already exists the existing row is returned unchanged
// (the share key is stable, so callers' copied links don't break on
// a re-toggle).
func (s *WikiStore) EnablePageShare(ctx context.Context, pageID, userID string) (*PageShare, error) {
	if existing, err := s.GetPageShare(ctx, pageID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	key, err := newShareKey()
	if err != nil {
		return nil, fmt.Errorf("wiki: share key: %w", err)
	}
	var p PageShare
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO wiki_page_shares (share_key, page_id, visibility, created_by)
		VALUES ($1, $2::uuid, 'public', $3)
		RETURNING share_key, page_id::text, visibility, created_by, created_at`,
		key, pageID, userID,
	).Scan(&p.ShareKey, &p.PageID, &p.Visibility, &p.CreatedBy, &p.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("wiki: enable share: %w", err)
	}
	return &p, nil
}

// DisablePageShare removes the public share row for a page. Safe to
// call when no share exists.
func (s *WikiStore) DisablePageShare(ctx context.Context, pageID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM wiki_page_shares
		 WHERE page_id = $1::uuid AND visibility = 'public'`, pageID)
	if err != nil {
		return fmt.Errorf("wiki: disable share: %w", err)
	}
	return nil
}

// LookupSharedPage resolves a public share key to the live page it
// points at. Returns ErrPageNotFound when the key is unknown, the
// page is soft-deleted, or the page's book is archived — the same
// signal so a probe can't tell the three apart.
func (s *WikiStore) LookupSharedPage(ctx context.Context, shareKey string) (*SharedPage, error) {
	var sp SharedPage
	err := s.db.QueryRowContext(ctx, `
		SELECT wp.id::text, wp.title, wp.content, wp.updated_at,
		       sh.created_by, sh.visibility
		  FROM wiki_page_shares sh
		  JOIN wiki_pages       wp ON wp.id = sh.page_id
		  JOIN books            b  ON b.id  = wp.book_id
		 WHERE sh.share_key = $1
		   AND sh.visibility = 'public'
		   AND wp.deleted_at IS NULL
		   AND b.archived_at IS NULL`,
		shareKey,
	).Scan(&sp.PageID, &sp.Title, &sp.Content, &sp.UpdatedAt, &sp.SharedBy, &sp.Visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wiki: lookup shared page: %w", err)
	}
	return &sp, nil
}

// newShareKey returns a 16-char alphanumeric share key sourced from
// crypto/rand. Alphabet is base62 (digits + upper + lower) so the
// key reads cleanly in URLs without any encoding.
//
// Rejection sampling eliminates the modulus bias the naive byte%62
// path introduces (8 of 256 byte values would skew the first 8
// alphabet positions to ~25% higher probability). With rejection
// the distribution is uniform across all 62 positions — 16 chars
// give the full log2(62^16) ≈ 95.3 bits of entropy, well above
// any practical brute-force budget against a public URL.
func newShareKey() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	const n = byte(len(alphabet))
	const limit = byte(256 - (256 % int(n))) // 248
	out := make([]byte, 16)
	buf := make([]byte, 1)
	for i := 0; i < len(out); {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if buf[0] >= limit {
			continue
		}
		out[i] = alphabet[buf[0]%n]
		i++
	}
	return string(out), nil
}

// ──────────────────────────────────────────────────────────────────
// Handlers
// ──────────────────────────────────────────────────────────────────

// pageActorCtx returns r.Context() with the calling principal's
// actor metadata attached (PageActor{UserID, ShardID}). Handlers
// pass the returned context to UpdatePage / CreatePage / DeletePage
// so the saved-hook fires with shard provenance for the SSE
// payload — viewers see "Synced — <shard name>" for shard writes.
func (h *Handler) pageActorCtx(r *http.Request) context.Context {
	au, _ := AuthUserFrom(r.Context())
	return WithPageActor(r.Context(), PageActor{
		UserID:  au.UserID,
		ShardID: au.ShardID,
	})
}

func (h *Handler) ensureWiki(w http.ResponseWriter) bool {
	if h.wiki == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "books not configured on this deploy")
		return false
	}
	return true
}

// scopeForWiki resolves the caller and verifies they're a member of
// the book at the {slug} path param. Returns the (book, userID,
// isAdmin) triple plus a handled flag. Membership check returns
// 404 (not 403) on non-member, matching scopeForNotes /
// scopeForConversations to avoid existence probing.
//
// Magic slug "personal": resolves to the caller's own personal
// book (auto-created if it doesn't exist yet) so the frontend
// Notes panel can address it without first round-tripping to
// learn its actual personal:{user_id} slug. Admins with the
// ?user_id= override get the OTHER user's personal book.
//
// On any failure the helper has already written the response — the
// caller just returns. ok=true means it's safe to proceed.
func (h *Handler) scopeForWiki(w http.ResponseWriter, r *http.Request) (book *Book, userID string, isAdmin bool, ok bool) {
	au, authOK := AuthUserFrom(r.Context())
	if !authOK || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return nil, "", false, false
	}
	isAdmin = au.IsAdmin()
	userID = adminUserScope(r, au)
	slug := r.PathValue("slug")
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "missing book slug")
		return nil, "", false, false
	}
	if slug == "personal" {
		b, err := h.wiki.EnsurePersonalBook(r.Context(), userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "personal book: "+err.Error())
			return nil, "", false, false
		}
		// Shard sessions: enforce the book_access envelope. 404
		// (not 403) so the surface looks the same as a non-member.
		if !au.CanAccessBook(b.ID) {
			writeJSONError(w, http.StatusNotFound, "book not found")
			return nil, "", false, false
		}
		return b, userID, isAdmin, true
	}
	b, err := h.wiki.GetBookBySlug(r.Context(), slug, userID, isAdmin)
	if errors.Is(err, ErrBookNotFound) {
		writeJSONError(w, http.StatusNotFound, "book not found")
		return nil, "", false, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return nil, "", false, false
	}
	if !au.CanAccessBook(b.ID) {
		writeJSONError(w, http.StatusNotFound, "book not found")
		return nil, "", false, false
	}
	return b, userID, isAdmin, true
}

// ── Books endpoints ───────────────────────────────────────────────

func (h *Handler) listBooks(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	userID := adminUserScope(r, au)
	includeArchived := r.URL.Query().Get("archived") == "true"
	rows, err := h.wiki.ListBooks(r.Context(), userID, includeArchived)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Shard sessions: drop books that aren't in the permission
	// envelope. CanAccessBook short-circuits true for user sessions
	// (and for shards with no book_access set), so this is a no-op
	// for the common path.
	if au.IsShardSession() && au.Permissions != nil && au.Permissions.Books != nil {
		filtered := make([]BookSummary, 0, len(rows))
		for _, b := range rows {
			if au.CanAccessBook(b.ID) {
				filtered = append(filtered, b)
			}
		}
		rows = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (h *Handler) createBook(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Slug        string `json:"slug"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	b, err := h.wiki.CreateBook(r.Context(), au.UserID, body.Name, body.Description, body.Slug)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// getPersonalBook returns the caller's personal book, creating it
// idempotently on first call. Endpoint is GET — no body needed —
// because callers shouldn't have to know whether they've ever
// touched their personal book before. Admins can pass ?user_id= to
// fetch another user's personal book (operational escape hatch);
// without that flag, the caller's own session userID is used.
func (h *Handler) getPersonalBook(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	userID := adminUserScope(r, au)
	b, err := h.wiki.EnsurePersonalBook(r.Context(), userID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *Handler) getBook(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *Handler) patchBook(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        *string `json:"name,omitempty"`
		Description *string `json:"description,omitempty"`
		Archive     *bool   `json:"archive,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	updated, err := h.wiki.UpdateBook(r.Context(), b.Slug, userID, isAdmin, BookPatch{
		Name:        body.Name,
		Description: body.Description,
		Archive:     body.Archive,
	})
	if errors.Is(err, ErrBookNotFound) {
		writeJSONError(w, http.StatusNotFound, "book not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteBook(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	t := true
	_, err := h.wiki.UpdateBook(r.Context(), b.Slug, userID, isAdmin, BookPatch{Archive: &t})
	if errors.Is(err, ErrBookNotFound) {
		writeJSONError(w, http.StatusNotFound, "book not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Member endpoints ──────────────────────────────────────────────

func (h *Handler) listBookMembers(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	rows, err := h.wiki.ListMembers(r.Context(), b.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// requireOwner re-checks that the caller is the book's owner. Used
// by membership management endpoints. scopeForWiki only confirms
// membership; this gates owner-only actions.
func (h *Handler) requireOwner(w http.ResponseWriter, r *http.Request, b *Book, userID string, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	role, _ := h.wiki.MemberRole(r.Context(), b.ID, userID)
	if role != "owner" {
		writeJSONError(w, http.StatusForbidden, "owner only")
		return false
	}
	return true
}

// requirePageWrite gates page CRUD endpoints to owners + writers.
// Readers can read pages but can't create / edit / delete them.
// Admins bypass the role check (operational escape hatch).
// scopeForWiki has already validated membership at the book level
// so a non-member never reaches this helper.
func (h *Handler) requirePageWrite(w http.ResponseWriter, r *http.Request, b *Book, userID string, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	role, _ := h.wiki.MemberRole(r.Context(), b.ID, userID)
	if role != "owner" && role != "writer" {
		writeJSONError(w, http.StatusForbidden, "owner or writer role required")
		return false
	}
	return true
}

func (h *Handler) addBookMember(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requireOwner(w, r, b, userID, isAdmin) {
		return
	}
	// Accept either {user_id} for the canonical Familiar user id
	// or {email} for the email-keyed registration value (which is
	// what most operators type). Email is resolved via the
	// UserManager so the FK insert always fires against a real id.
	var body struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	resolvedID := strings.TrimSpace(body.UserID)
	email := strings.TrimSpace(body.Email)
	if resolvedID == "" && email == "" {
		writeJSONError(w, http.StatusBadRequest, "user_id or email required")
		return
	}
	if resolvedID == "" {
		if h.users == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "user lookup not configured; pass user_id directly")
			return
		}
		u, err := h.users.GetByEmail(r.Context(), email)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if u == nil {
			writeJSONError(w, http.StatusNotFound, "no user with email "+email)
			return
		}
		resolvedID = u.ID
	}
	m, err := h.wiki.AddMember(r.Context(), userID, b.ID, resolvedID, body.Role)
	if err != nil {
		// Surface FK violations as a friendlier 404 — it almost
		// always means the operator typed a user_id that isn't
		// in the users table.
		if strings.Contains(err.Error(), "book_members_user_id_fkey") {
			writeJSONError(w, http.StatusNotFound, "no user with id "+resolvedID)
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) patchBookMember(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requireOwner(w, r, b, userID, isAdmin) {
		return
	}
	targetID := r.PathValue("user_id")
	if targetID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing user_id")
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	m, err := h.wiki.AddMember(r.Context(), userID, b.ID, targetID, body.Role)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) deleteBookMember(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	targetID := r.PathValue("user_id")
	if targetID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing user_id")
		return
	}
	// Self-removal is allowed without owner role (any member can
	// leave a book they're in). Owner removal of others requires
	// the owner role.
	if targetID != userID && !h.requireOwner(w, r, b, userID, isAdmin) {
		return
	}
	if err := h.wiki.RemoveMember(r.Context(), userID, b.ID, targetID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Page endpoints ────────────────────────────────────────────────

func (h *Handler) listBookPages(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	rows, err := h.wiki.ListPages(r.Context(), b.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pinned, err := h.wiki.PinnedPageIDs(r.Context(), userID, b.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range rows {
		rows[i].Pinned = pinned[rows[i].ID]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (h *Handler) createBookPage(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requirePageWrite(w, r, b, userID, isAdmin) {
		return
	}
	var body struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		Slug    string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if body.Title == "" {
		body.Title = "Untitled"
	}
	p, err := h.wiki.CreatePage(h.pageActorCtx(r), b.ID, userID, body.Title, body.Content, body.Slug)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) getBookPage(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageSlug := r.PathValue("page_slug")
	p, err := h.wiki.GetPage(r.Context(), b.ID, pageSlug)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pinned, err := h.wiki.IsPagePinned(r.Context(), userID, p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.Pinned = pinned
	// Attach public-share state so the editor can show the globe
	// indicator on load — consistent with getBookPageByID. (This is
	// the GET wiki.js loadPage uses.)
	if share, err := h.wiki.GetPageShare(r.Context(), p.ID); err == nil && share != nil {
		share.PublicURL = h.publicShareURL(share.ShareKey)
		p.Share = share
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) patchBookPage(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requirePageWrite(w, r, b, userID, isAdmin) {
		return
	}
	pageSlug := r.PathValue("page_slug")
	var body struct {
		Title   *string `json:"title,omitempty"`
		Content *string `json:"content,omitempty"`
		Slug    *string `json:"slug,omitempty"`
		Summary *string `json:"summary,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	patch := PagePatch{
		Title:   body.Title,
		Content: body.Content,
		Slug:    body.Slug,
		Summary: body.Summary,
	}
	// Optimistic concurrency. Clients send the updated_at they
	// loaded with as If-Match; UpdatePage returns ErrPageStale if
	// the row has moved since. RFC 7232 §3.1 says If-Match takes
	// a quoted opaque tag, but we accept either a quoted or bare
	// RFC3339 timestamp since the workspace is the only client.
	if im := strings.Trim(r.Header.Get("If-Match"), `" `); im != "" {
		t, perr := time.Parse(time.RFC3339Nano, im)
		if perr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid If-Match: "+perr.Error())
			return
		}
		patch.IfMatch = &t
	}
	// A content edit MUST carry a precondition. Without one the write is
	// unconditional last-write-wins — the exact clobber the If-Match
	// machinery exists to stop. All first-party clients already send it;
	// this closes the omit-header hole (scripts, stale SWs, future
	// callers). Title/slug-only patches are exempt: they don't race body
	// edits.
	if patch.Content != nil && patch.IfMatch == nil {
		writeJSONError(w, http.StatusPreconditionRequired, "If-Match (the page's loaded updated_at) is required for content edits")
		return
	}
	updated, err := h.wiki.UpdatePage(h.pageActorCtx(r), b.ID, pageSlug, userID, patch)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if errors.Is(err, ErrPageStale) {
		// Return the current page so the client can show the
		// remote version without a second round-trip.
		if cur, gerr := h.wiki.GetPage(r.Context(), b.ID, pageSlug); gerr == nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "stale",
				"message": "page was updated by another writer; reload before saving",
				"current": cur,
			})
			return
		}
		writeJSONError(w, http.StatusConflict, "page was updated by another writer; reload before saving")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteBookPage(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requirePageWrite(w, r, b, userID, isAdmin) {
		return
	}
	pageSlug := r.PathValue("page_slug")
	if err := h.wiki.DeletePage(h.pageActorCtx(r), b.ID, pageSlug); errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	} else if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Link endpoints ────────────────────────────────────────────────

// listBookPageLinks serves GET .../pages/{page_slug}/links — the
// outbound [[]] links from the page. Any role can read.
func (h *Handler) listBookPageLinks(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageSlug := r.PathValue("page_slug")
	p, err := h.wiki.GetPage(r.Context(), b.ID, pageSlug)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	links, err := h.wiki.ListPageLinks(r.Context(), p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": links})
}

// listBookPageBacklinks serves GET .../pages/{page_slug}/backlinks
// — the inbound links into this page from any book. Any role can
// read. See ListBacklinks PRECONDITION for the cross-book
// visibility note.
func (h *Handler) listBookPageBacklinks(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageSlug := r.PathValue("page_slug")
	p, err := h.wiki.GetPage(r.Context(), b.ID, pageSlug)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	links, err := h.wiki.ListBacklinks(r.Context(), p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": links})
}

// listBookPageLinksByID + listBookPageBacklinksByID are the
// id-keyed siblings used by the notes panel (which keeps page
// IDs in its model). Same scoping + read posture as the slug
// versions.
func (h *Handler) listBookPageLinksByID(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageID := r.PathValue("page_id")
	p, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	links, err := h.wiki.ListPageLinks(r.Context(), p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": links})
}

func (h *Handler) listBookPageBacklinksByID(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageID := r.PathValue("page_id")
	p, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	links, err := h.wiki.ListBacklinks(r.Context(), p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": links})
}

// ── Revision endpoints ────────────────────────────────────────────

func (h *Handler) listBookPageRevisions(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageSlug := r.PathValue("page_slug")
	p, err := h.wiki.GetPage(r.Context(), b.ID, pageSlug)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, err := h.wiki.ListRevisions(r.Context(), p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (h *Handler) getBookPageRevision(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageSlug := r.PathValue("page_slug")
	revID := r.PathValue("rev_id")
	p, err := h.wiki.GetPage(r.Context(), b.ID, pageSlug)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rev, err := h.wiki.GetRevision(r.Context(), p.ID, revID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "revision not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rev)
}

// ── Search endpoint ───────────────────────────────────────────────

func (h *Handler) searchBookPages(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.wiki.SearchPages(r.Context(), b.ID, q, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pinned, err := h.wiki.PinnedPageIDs(r.Context(), userID, b.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range rows {
		rows[i].Pinned = pinned[rows[i].ID]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// ── By-ID page endpoints ──────────────────────────────────────────
//
// Slug-based URLs are the canonical shape — they're stable and
// human-readable. The frontend Notes panel keeps note IDs in its
// model (matching wiki_pages.id post-migration), so it needs an
// id-keyed sibling for GET / PATCH / DELETE / append / pin without
// pre-listing to resolve a slug. Same enforcement chain through
// scopeForWiki + requirePageWrite.

func (h *Handler) getBookPageByID(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageID := r.PathValue("page_id")
	p, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pinned, err := h.wiki.IsPagePinned(r.Context(), userID, p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.Pinned = pinned
	if share, err := h.wiki.GetPageShare(r.Context(), p.ID); err == nil && share != nil {
		share.PublicURL = h.publicShareURL(share.ShareKey)
		p.Share = share
	}
	writeJSON(w, http.StatusOK, p)
}

// patchBookPageByID accepts the page-edit fields (title, content,
// slug, summary) AND the per-user pin toggle in one body so the
// frontend can mirror the legacy notes PATCH semantics. Page-level
// edits require write capability; pin toggles do not (any member
// can pin), so the write check only fires when there's something
// content-shaped to write. Pin-only patches skip it.
func (h *Handler) patchBookPageByID(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageID := r.PathValue("page_id")
	cur, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var body struct {
		Title   *string `json:"title,omitempty"`
		Content *string `json:"content,omitempty"`
		Slug    *string `json:"slug,omitempty"`
		Summary *string `json:"summary,omitempty"`
		Pinned  *bool   `json:"pinned,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	hasContentEdit := body.Title != nil || body.Content != nil || body.Slug != nil || body.Summary != nil
	if hasContentEdit && !h.requirePageWrite(w, r, b, userID, isAdmin) {
		return
	}
	updated := cur
	if hasContentEdit {
		patch := PagePatch{
			Title: body.Title, Content: body.Content,
			Slug: body.Slug, Summary: body.Summary,
		}
		if im := strings.Trim(r.Header.Get("If-Match"), `" `); im != "" {
			t, perr := time.Parse(time.RFC3339Nano, im)
			if perr != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid If-Match: "+perr.Error())
				return
			}
			patch.IfMatch = &t
		}
		// Content edits require a precondition — see patchBookPage.
		if patch.Content != nil && patch.IfMatch == nil {
			writeJSONError(w, http.StatusPreconditionRequired, "If-Match (the page's loaded updated_at) is required for content edits")
			return
		}
		updated, err = h.wiki.UpdatePage(h.pageActorCtx(r), b.ID, cur.Slug, userID, patch)
		if errors.Is(err, ErrPageNotFound) {
			writeJSONError(w, http.StatusNotFound, "page not found")
			return
		}
		if errors.Is(err, ErrPageStale) {
			if cur2, gerr := h.wiki.GetPage(r.Context(), b.ID, cur.Slug); gerr == nil {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":   "stale",
					"message": "page was updated by another writer; reload before saving",
					"current": cur2,
				})
				return
			}
			writeJSONError(w, http.StatusConflict, "page was updated by another writer; reload before saving")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Pinned != nil {
		if err := h.wiki.SetPagePinned(r.Context(), userID, updated.ID, *body.Pinned); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	pinned, err := h.wiki.IsPagePinned(r.Context(), userID, updated.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated.Pinned = pinned
	if share, err := h.wiki.GetPageShare(r.Context(), updated.ID); err == nil && share != nil {
		updated.Share = share
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteBookPageByID(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requirePageWrite(w, r, b, userID, isAdmin) {
		return
	}
	pageID := r.PathValue("page_id")
	cur, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.wiki.DeletePage(h.pageActorCtx(r), b.ID, cur.Slug); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appendBookPageByID adds a fresh paragraph to the bottom of the page
// body via WikiStore.AppendPage — a single atomic in-DB concatenation,
// so two concurrent appends can't clobber each other (the old
// GetPage+compose+UpdatePage version had a lost-update window).
func (h *Handler) appendBookPageByID(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requirePageWrite(w, r, b, userID, isAdmin) {
		return
	}
	pageID := r.PathValue("page_id")
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSONError(w, http.StatusBadRequest, "text is required")
		return
	}
	updated, err := h.wiki.AppendPage(h.pageActorCtx(r), b.ID, pageID, userID, body.Text)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pinned, err := h.wiki.IsPagePinned(r.Context(), userID, updated.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated.Pinned = pinned
	writeJSON(w, http.StatusOK, updated)
}

// pinBookPageByID upserts the (user, page, pinned) row in
// user_page_prefs. Any role can pin (it's a per-user display
// preference, not a content edit). Body: {"pinned": bool}.
func (h *Handler) pinBookPageByID(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageID := r.PathValue("page_id")
	// Confirm the page lives in this book before we touch the pref
	// table (otherwise an attacker who knew a page id could pin
	// pages outside their books).
	p, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := h.wiki.SetPagePinned(r.Context(), userID, p.ID, body.Pinned); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.Pinned = body.Pinned
	writeJSON(w, http.StatusOK, p)
}

// moveBookPageByID reparents a page and/or changes its sibling
// position. Body: {"parent_id": "<uuid>" | "", "sort_order": <int>}.
// parent_id="" unparents to top-level; sort_order is optional and
// appends to the end when omitted.
//
// Requires page-write capability on the book — moving a page is a
// structural edit. The MovePage store method handles cycle / cross-
// book / soft-delete validation and returns ErrInvalidParent for
// the 4xx-shaped cases.
func (h *Handler) moveBookPageByID(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requirePageWrite(w, r, b, userID, isAdmin) {
		return
	}
	pageID := r.PathValue("page_id")

	// Distinguish "parent_id omitted" from "parent_id: null" via
	// json.RawMessage. Omitted → no parent change requested (but
	// MovePage requires *some* signal; we treat omission as "no
	// change" by reading current parent first). Explicit null or
	// "" → unparent. String → reparent.
	var body struct {
		ParentID  *string `json:"parent_id"`
		SortOrder *int    `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if body.ParentID == nil && body.SortOrder == nil {
		writeJSONError(w, http.StatusBadRequest, "parent_id or sort_order required")
		return
	}

	// When parent_id was omitted (nil pointer), keep the current
	// parent. Read current page to recover it. This also confirms
	// the page lives in this book before MovePage runs.
	cur, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	newParent := ""
	if body.ParentID != nil {
		newParent = *body.ParentID
	} else if cur.ParentID != nil {
		newParent = *cur.ParentID
	}

	updated, err := h.wiki.MovePage(r.Context(), b.ID, pageID, newParent, body.SortOrder)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if errors.Is(err, ErrInvalidParent) {
		writeJSONError(w, http.StatusBadRequest, "invalid parent (missing, deleted, cross-book, or would create a cycle)")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
