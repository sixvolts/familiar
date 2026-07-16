// Package wiki provides a Familiar skill backed by Familiar's books
// + wiki_pages tables. Nine tools — list_books, list_pages,
// search_pages, read_page, create_page, update_page, append_to_page,
// patch_page, pin_page — let the model navigate the user's books and
// read or write individual pages during a conversation.
//
// Membership scoping: every operation resolves the book by slug and
// then checks the caller's role on that book via WikiStore.MemberRole.
// Read tools require any role (owner/writer/reader). Write tools
// require owner or writer; readers get a friendly 403-style error.
//
// Like the notes skill, delete_page is intentionally absent —
// destructive operations belong in the wiki UI where the user can
// see what they're about to nuke before confirming.
package wiki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/skills"
)

const (
	// previewMaxRunes caps each per-result preview rendered in
	// search/list responses so the rendered context block stays
	// compact.
	previewMaxRunes = 150

	// searchHitMax is the ceiling on hits returned from search_pages.
	searchHitMax = 10

	// defaultListLimit is the default for list_books / list_pages
	// when the model doesn't specify.
	defaultListLimit = 25

	// listLimitMax is the upper bound declared in the schema.
	listLimitMax = 100
)

// WikiBackend is the slice of admin.WikiStore the skill uses. Defining
// the interface here keeps the test surface narrow and lets a future
// refactor swap implementations without touching the skill.
type WikiBackend interface {
	ListBooks(ctx context.Context, userID string, includeArchived bool) ([]admin.BookSummary, error)
	ListBooksWithPersonal(ctx context.Context, userID string, includeArchived bool) ([]admin.BookSummary, error)
	GetBookBySlug(ctx context.Context, slug, userID string, isAdmin bool) (*admin.Book, error)
	// EnsurePersonalBook backs the "personal" book_slug alias — the
	// deterministic path to the caller's personal notes.
	EnsurePersonalBook(ctx context.Context, userID string) (*admin.Book, error)
	MemberRole(ctx context.Context, bookID, userID string) (string, error)
	ListPages(ctx context.Context, bookID string) ([]admin.WikiPageSummary, error)
	GetPage(ctx context.Context, bookID, pageSlug string) (*admin.WikiPage, error)
	CreatePage(ctx context.Context, bookID, userID, title, content, requestedSlug string) (*admin.WikiPage, error)
	UpdatePage(ctx context.Context, bookID, pageSlug, userID string, p admin.PagePatch) (*admin.WikiPage, error)
	// AppendPage atomically appends a paragraph (no read-modify-write),
	// so an agent append can't lose a concurrent human edit.
	AppendPage(ctx context.Context, bookID, pageID, userID, text string) (*admin.WikiPage, error)
	SearchPages(ctx context.Context, bookID, query string, limit int) ([]admin.WikiPageSummary, error)
	SetPagePinned(ctx context.Context, userID, pageID string, pinned bool) error
}

// Skill implements the eight wiki tools. Backend resolution is lazy
// for the same reason the notes skill is lazy — main.go registers
// the skill before the admin pool block constructs the store.
type Skill struct {
	resolve func() WikiBackend
}

// New constructs a wiki skill. resolve is a getter that returns the
// WikiBackend or nil. nil resolve is treated as "permanently no
// backend" — every tool invocation returns the not-configured error.
func New(resolve func() WikiBackend) *Skill {
	return &Skill{resolve: resolve}
}

func (s *Skill) backend() WikiBackend {
	if s.resolve == nil {
		return nil
	}
	return s.resolve()
}

func (s *Skill) Name() string { return "wiki" }
func (s *Skill) Description() string {
	return "Read and write the user's wiki books and pages (Familiar Postgres)"
}
func (s *Skill) Version() string { return "1.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

// ──────────────────────────────────────────────────────────────────
// Tool schemas
// ──────────────────────────────────────────────────────────────────

var listBooksParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "include_archived": {
      "type": "boolean",
      "description": "Include archived books in the result. Default false."
    },
    "include_personal": {
      "type": "boolean",
      "description": "Include the user's personal book (where their notes live). Default false because shared-book questions usually want shared books only."
    }
  }
}`)

var listPagesParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "book_slug": {
      "type": "string",
      "description": "Slug of the book whose pages to list. Get the slug from list_books."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of pages to return (1-100, default 25).",
      "minimum": 1,
      "maximum": 100
    }
  },
  "required": ["book_slug"]
}`)

var searchPagesParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "book_slug": {
      "type": "string",
      "description": "Slug of the book to search within."
    },
    "query": {
      "type": "string",
      "description": "Text to search for in page titles and content."
    }
  },
  "required": ["book_slug", "query"]
}`)

var readPageParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "book_slug": {"type": "string", "description": "Slug of the book the page lives in."},
    "page_slug": {"type": "string", "description": "Slug of the page. Get from list_pages or search_pages."}
  },
  "required": ["book_slug", "page_slug"]
}`)

var createPageParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "book_slug": {"type": "string", "description": "Slug of the book to add the page to. Pass the literal \"personal\" to write to the user's personal notes without looking the book up."},
    "title":     {"type": "string", "description": "Page title."},
    "content":   {"type": "string", "description": "Markdown body of the page."}
  },
  "required": ["book_slug", "title"]
}`)

var updatePageParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "book_slug": {"type": "string", "description": "Slug of the book."},
    "page_slug": {"type": "string", "description": "Slug of the page to replace."},
    "content":   {"type": "string", "description": "New markdown body. REPLACES the existing content. Use append_to_page or patch_page to add or edit text without losing anything."},
    "title":     {"type": "string", "description": "Optional new title. When omitted the existing title is preserved."}
  },
  "required": ["book_slug", "page_slug", "content"]
}`)

var appendPageParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "book_slug": {"type": "string", "description": "Slug of the book."},
    "page_slug": {"type": "string", "description": "Slug of the page to append to."},
    "text":      {"type": "string", "description": "Text to add to the bottom of the page as a new paragraph."}
  },
  "required": ["book_slug", "page_slug", "text"]
}`)

var patchPageParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "book_slug": {"type": "string", "description": "Slug of the book."},
    "page_slug": {"type": "string", "description": "Slug of the page to patch."},
    "find":      {"type": "string", "description": "Exact text to find in the page body. Must match verbatim (case-sensitive). Include enough surrounding context to be unique — typically the full line or table row."},
    "replace":   {"type": "string", "description": "Replacement text. The find match is swapped for this string. To delete, set to empty string. To insert after a line, set find to that line and replace to the line plus the new content."}
  },
  "required": ["book_slug", "page_slug", "find", "replace"]
}`)

var pinPageParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "book_slug": {"type": "string", "description": "Slug of the book the page lives in."},
    "page_slug": {"type": "string", "description": "Slug of the page to pin. Get from list_pages or search_pages."},
    "pinned":    {"type": "boolean", "description": "true (default) pins the page; false removes the pin."}
  },
  "required": ["book_slug", "page_slug"]
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name:        "list_books",
			Description: "List the user's wiki books (the books they're a member of), most-recently-updated first. Returns slug, name, role, and description.",
			Parameters:  listBooksParams,
		},
		{
			Name:        "list_pages",
			Description: "List pages inside a specific book by slug. Use list_books first to discover slugs.",
			Parameters:  listPagesParams,
		},
		{
			Name:        "search_pages",
			Description: "Search pages within a book by free-text query. Returns up to 10 hits with title, slug, and a content preview.",
			Parameters:  searchPagesParams,
		},
		{
			Name:        "read_page",
			Description: "Read the full markdown body of a single wiki page by book slug + page slug.",
			Parameters:  readPageParams,
		},
		{
			Name:        "create_page",
			Description: "Create a new page in a book with the given title and (optional) markdown body. Caller must be owner or writer on the book.",
			Parameters:  createPageParams,
		},
		{
			Name:        "update_page",
			Description: "Replace the body of an existing page. WARNING: this overwrites the current content — use append_to_page or patch_page when you only want to add or change part of it. Optionally retitle. Caller must be owner or writer.",
			Parameters:  updatePageParams,
		},
		{
			Name:        "append_to_page",
			Description: "Add a paragraph to the bottom of an existing page. Non-destructive — the existing content is preserved and the new text is added as a fresh paragraph. Caller must be owner or writer.",
			Parameters:  appendPageParams,
		},
		{
			Name:        "patch_page",
			Description: "Surgically edit a page by finding and replacing a specific passage. Use this instead of update_page when you need to change, insert, or delete a specific section without rewriting the entire document. The find string must match exactly. Caller must be owner or writer.",
			Parameters:  patchPageParams,
		},
		{
			Name:        "pin_page",
			Description: "Pin a wiki page so it appears in the user's Pinned list (Home and sidebar). Pins are per-user — this pins for the user this conversation runs on behalf of. Pass pinned=false to unpin. Any member of the book can pin; no writer role needed.",
			Parameters:  pinPageParams,
		},
	}
}

// ──────────────────────────────────────────────────────────────────
// Argument types
// ──────────────────────────────────────────────────────────────────

// flexBool tolerates the string booleans local models routinely emit
// ("true"/"false") as well as real JSON booleans, so a `{"include_
// personal":"true"}` tool call no longer fails to unmarshal — the exact
// "parameter format issue" that broke a research note's book lookup.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch s := strings.ToLower(strings.Trim(string(b), `" `)); s {
	case "true", "1", "yes", "on":
		*f = true
	case "false", "0", "no", "off", "", "null":
		*f = false
	default:
		var bv bool
		if err := json.Unmarshal(b, &bv); err != nil {
			return err
		}
		*f = flexBool(bv)
	}
	return nil
}

type listBooksArgs struct {
	IncludeArchived flexBool `json:"include_archived,omitempty"`
	IncludePersonal flexBool `json:"include_personal,omitempty"`
}

type listPagesArgs struct {
	BookSlug string `json:"book_slug"`
	Limit    *int   `json:"limit,omitempty"`
}

type searchArgs struct {
	BookSlug string `json:"book_slug"`
	Query    string `json:"query"`
}

type readArgs struct {
	BookSlug string `json:"book_slug"`
	PageSlug string `json:"page_slug"`
}

type createArgs struct {
	BookSlug string `json:"book_slug"`
	Title    string `json:"title"`
	Content  string `json:"content"`
}

type updateArgs struct {
	BookSlug string `json:"book_slug"`
	PageSlug string `json:"page_slug"`
	Title    string `json:"title,omitempty"`
	Content  string `json:"content"`
}

type appendArgs struct {
	BookSlug string `json:"book_slug"`
	PageSlug string `json:"page_slug"`
	Text     string `json:"text"`
}

type patchArgs struct {
	BookSlug string `json:"book_slug"`
	PageSlug string `json:"page_slug"`
	Find     string `json:"find"`
	Replace  string `json:"replace"`
}

type pinArgs struct {
	BookSlug string `json:"book_slug"`
	PageSlug string `json:"page_slug"`
	// Pinned is a pointer so "omitted" defaults to true (the common
	// intent) while an explicit false unpins.
	Pinned *bool `json:"pinned,omitempty"`
}

// ──────────────────────────────────────────────────────────────────
// Dispatch
// ──────────────────────────────────────────────────────────────────

const noBackendMsg = "Wiki backend not configured on this gateway. Ask an admin to wire AttachWikiStore."

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	if s.backend() == nil {
		return skills.ToolResult{Error: noBackendMsg}, nil
	}
	userID := userIDFromCtx(ctx)
	if userID == "" {
		return skills.ToolResult{Error: "no authenticated user on this turn — wiki needs a user context"}, nil
	}

	switch toolName {
	case "list_books":
		var args listBooksArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		return s.listBooks(ctx, userID, bool(args.IncludeArchived), bool(args.IncludePersonal))

	case "list_pages":
		var args listPagesArgs
		if err := decode(params, &args); err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		if strings.TrimSpace(args.BookSlug) == "" {
			return skills.ToolResult{Error: "book_slug is required"}, nil
		}
		limit := defaultListLimit
		if args.Limit != nil {
			limit = *args.Limit
		}
		if limit < 1 {
			limit = 1
		}
		if limit > listLimitMax {
			limit = listLimitMax
		}
		return s.listPages(ctx, userID, args.BookSlug, limit)

	case "search_pages":
		var args searchArgs
		if err := decode(params, &args); err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		if strings.TrimSpace(args.BookSlug) == "" {
			return skills.ToolResult{Error: "book_slug is required"}, nil
		}
		if strings.TrimSpace(args.Query) == "" {
			return skills.ToolResult{Error: "query is required"}, nil
		}
		return s.searchPages(ctx, userID, args.BookSlug, args.Query)

	case "read_page":
		var args readArgs
		if err := decode(params, &args); err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		if strings.TrimSpace(args.BookSlug) == "" || strings.TrimSpace(args.PageSlug) == "" {
			return skills.ToolResult{Error: "book_slug and page_slug are required"}, nil
		}
		return s.readPage(ctx, userID, args.BookSlug, args.PageSlug)

	case "create_page":
		var args createArgs
		if err := decode(params, &args); err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		if strings.TrimSpace(args.BookSlug) == "" {
			return skills.ToolResult{Error: "book_slug is required"}, nil
		}
		if strings.TrimSpace(args.Title) == "" {
			return skills.ToolResult{Error: "title is required"}, nil
		}
		return s.createPage(ctx, userID, args.BookSlug, args.Title, args.Content)

	case "update_page":
		var args updateArgs
		if err := decode(params, &args); err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		if strings.TrimSpace(args.BookSlug) == "" || strings.TrimSpace(args.PageSlug) == "" {
			return skills.ToolResult{Error: "book_slug and page_slug are required"}, nil
		}
		return s.updatePage(ctx, userID, args.BookSlug, args.PageSlug, args.Title, args.Content)

	case "append_to_page":
		var args appendArgs
		if err := decode(params, &args); err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		if strings.TrimSpace(args.BookSlug) == "" || strings.TrimSpace(args.PageSlug) == "" {
			return skills.ToolResult{Error: "book_slug and page_slug are required"}, nil
		}
		if strings.TrimSpace(args.Text) == "" {
			return skills.ToolResult{Error: "text is required"}, nil
		}
		return s.appendToPage(ctx, userID, args.BookSlug, args.PageSlug, args.Text)

	case "patch_page":
		var args patchArgs
		if err := decode(params, &args); err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		if strings.TrimSpace(args.BookSlug) == "" || strings.TrimSpace(args.PageSlug) == "" {
			return skills.ToolResult{Error: "book_slug and page_slug are required"}, nil
		}
		if args.Find == "" {
			return skills.ToolResult{Error: "find is required (the exact text to locate in the page)"}, nil
		}
		return s.patchPage(ctx, userID, args.BookSlug, args.PageSlug, args.Find, args.Replace)

	case "pin_page":
		var args pinArgs
		if err := decode(params, &args); err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		if strings.TrimSpace(args.BookSlug) == "" || strings.TrimSpace(args.PageSlug) == "" {
			return skills.ToolResult{Error: "book_slug and page_slug are required"}, nil
		}
		pinned := true
		if args.Pinned != nil {
			pinned = *args.Pinned
		}
		return s.pinPage(ctx, userID, args.BookSlug, args.PageSlug, pinned)

	default:
		return skills.ToolResult{}, fmt.Errorf("wiki: unknown tool %q", toolName)
	}
}

func decode(params json.RawMessage, dst any) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, dst); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}

// userIDFromCtx pulls the canonical user id from SessionContext.
func userIDFromCtx(ctx context.Context) string {
	sc, _ := skills.ContextFrom(ctx)
	return sc.UserID
}

// bookScopeFromCtx reads the shard book allowlist off SessionContext.
// The second return is false when no scope is active — the trusted path
// and shards with empty book_access both fall through unrestricted. When
// active, only book IDs in the set are addressable; everything else is
// reported not-found, matching the existence-leak convention resolveBook
// already uses for non-members.
func bookScopeFromCtx(ctx context.Context) (map[string]bool, bool) {
	sc, _ := skills.ContextFrom(ctx)
	if len(sc.BookScope) == 0 {
		return nil, false
	}
	set := make(map[string]bool, len(sc.BookScope))
	for _, id := range sc.BookScope {
		set[id] = true
	}
	return set, true
}

// ──────────────────────────────────────────────────────────────────
// Membership helpers
// ──────────────────────────────────────────────────────────────────

// resolveBook looks the book up by slug and confirms membership. The
// returned role is "" only when the book exists but the user can't
// see it — in which case we report not-found to avoid leaking
// existence (matches handler behavior).
func (s *Skill) resolveBook(ctx context.Context, userID, slug string) (*admin.Book, string, error) {
	// "personal" is a deterministic alias for the caller's personal
	// notes book (slug personal:{userID}) so agents can write there
	// without a list_books round-trip — the fragile step that let a
	// local model, unable to see the hidden personal book, improvise
	// into a random wiki. The book scope check below still applies, so
	// a confined shard can't reach it unless its scope allows.
	var b *admin.Book
	var err error
	if slug == "personal" {
		b, err = s.backend().EnsurePersonalBook(ctx, userID)
	} else {
		b, err = s.backend().GetBookBySlug(ctx, slug, userID, false)
	}
	if errors.Is(err, admin.ErrBookNotFound) {
		return nil, "", err
	}
	if err != nil {
		return nil, "", err
	}
	role, err := s.backend().MemberRole(ctx, b.ID, userID)
	if err != nil {
		return nil, "", err
	}
	if role == "" {
		// Defense in depth — GetBookBySlug already checked membership
		// when isAdmin=false, but a future change could relax that.
		return nil, "", admin.ErrBookNotFound
	}
	// Shard book confinement. A scoped shard run can only address books
	// in its allowlist; anything else reads as not-found so a confined
	// shard can't even probe for the existence of the owner's other
	// books. The scope only ever narrows — membership is still required
	// above, so a stray scope entry never grants access.
	if scope, active := bookScopeFromCtx(ctx); active && !scope[b.ID] {
		return nil, "", admin.ErrBookNotFound
	}
	return b, role, nil
}

func canWrite(role string) bool {
	return role == "owner" || role == "writer"
}

// ──────────────────────────────────────────────────────────────────
// Tool implementations
// ──────────────────────────────────────────────────────────────────

func (s *Skill) listBooks(ctx context.Context, userID string, includeArchived, includePersonal bool) (skills.ToolResult, error) {
	var rows []admin.BookSummary
	var err error
	if includePersonal {
		rows, err = s.backend().ListBooksWithPersonal(ctx, userID, includeArchived)
	} else {
		rows, err = s.backend().ListBooks(ctx, userID, includeArchived)
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("list books: %w", err)
	}
	// Confine the listing to the shard's book allowlist when one is
	// active so a scoped shard never even sees the names of the owner's
	// other books.
	if scope, active := bookScopeFromCtx(ctx); active {
		kept := make([]admin.BookSummary, 0, len(rows))
		for _, bk := range rows {
			if scope[bk.ID] {
				kept = append(kept, bk)
			}
		}
		rows = kept
	}
	if len(rows) == 0 {
		const c = "no books found"
		return skills.ToolResult{Content: c, Tokens: len(c) / 4}, nil
	}
	var b strings.Builder
	b.WriteString("Books:\n\n")
	for i, bk := range rows {
		fmt.Fprintf(&b, "%d. %s  (slug: %s, role: %s, updated %s",
			i+1,
			fallback(bk.Name, "(untitled)"),
			bk.Slug,
			fallback(bk.Role, "?"),
			humanizeTimeAgo(bk.UpdatedAt),
		)
		if bk.ArchivedAt != nil {
			b.WriteString(", archived")
		}
		b.WriteString(")\n")
		if bk.Description != "" {
			desc := bk.Description
			runes := []rune(desc)
			if len(runes) > previewMaxRunes {
				desc = string(runes[:previewMaxRunes]) + "…"
			}
			b.WriteString("   ")
			b.WriteString(desc)
			b.WriteString("\n")
		}
		if i < len(rows)-1 {
			b.WriteString("\n")
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

func (s *Skill) listPages(ctx context.Context, userID, bookSlug string, limit int) (skills.ToolResult, error) {
	bk, _, err := s.resolveBook(ctx, userID, bookSlug)
	if errors.Is(err, admin.ErrBookNotFound) {
		return skills.ToolResult{Error: bookNotFoundMsg(bookSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("list pages: %w", err)
	}
	rows, err := s.backend().ListPages(ctx, bk.ID)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("list pages: %w", err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(rows) == 0 {
		out := fmt.Sprintf("No pages in book %q yet.", bk.Name)
		return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
	}
	out := formatPageList(fmt.Sprintf("Pages in %q (book slug: %s):", bk.Name, bk.Slug), rows)
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

func (s *Skill) searchPages(ctx context.Context, userID, bookSlug, query string) (skills.ToolResult, error) {
	bk, _, err := s.resolveBook(ctx, userID, bookSlug)
	if errors.Is(err, admin.ErrBookNotFound) {
		return skills.ToolResult{Error: bookNotFoundMsg(bookSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("search pages: %w", err)
	}
	rows, err := s.backend().SearchPages(ctx, bk.ID, query, searchHitMax)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("search pages: %w", err)
	}
	if len(rows) == 0 {
		out := fmt.Sprintf("no pages in %q match %s", bk.Name, strconv.Quote(query))
		return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
	}
	out := formatPageList(fmt.Sprintf("Search results for %s in %q:", strconv.Quote(query), bk.Name), rows)
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

func (s *Skill) readPage(ctx context.Context, userID, bookSlug, pageSlug string) (skills.ToolResult, error) {
	bk, _, err := s.resolveBook(ctx, userID, bookSlug)
	if errors.Is(err, admin.ErrBookNotFound) {
		return skills.ToolResult{Error: bookNotFoundMsg(bookSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("read: %w", err)
	}
	p, err := s.backend().GetPage(ctx, bk.ID, pageSlug)
	if errors.Is(err, admin.ErrPageNotFound) {
		return skills.ToolResult{Error: pageNotFoundMsg(bk.Name, pageSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("read: %w", err)
	}
	out := formatPageFull(bk, p)
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

// pinPage flips the per-user pin on a page. Any member role may pin —
// a pin is a personal preference, not a content write, matching the
// console handler's posture (pinBookPageByID gates on membership
// only). The pin lands on the acting user (SessionContext.UserID), so
// a shard turn pins for the user the shard is working on behalf of.
func (s *Skill) pinPage(ctx context.Context, userID, bookSlug, pageSlug string, pinned bool) (skills.ToolResult, error) {
	bk, _, err := s.resolveBook(ctx, userID, bookSlug)
	if errors.Is(err, admin.ErrBookNotFound) {
		return skills.ToolResult{Error: bookNotFoundMsg(bookSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("pin: %w", err)
	}
	p, err := s.backend().GetPage(ctx, bk.ID, pageSlug)
	if errors.Is(err, admin.ErrPageNotFound) {
		return skills.ToolResult{Error: pageNotFoundMsg(bk.Name, pageSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("pin: %w", err)
	}
	if err := s.backend().SetPagePinned(ctx, userID, p.ID, pinned); err != nil {
		return skills.ToolResult{}, fmt.Errorf("pin: %w", err)
	}
	verb := "Pinned"
	if !pinned {
		verb = "Unpinned"
	}
	out := fmt.Sprintf("%s %q in %q. The user's Pinned list (Home and sidebar) reflects this immediately.", verb, p.Title, bk.Name)
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

func (s *Skill) createPage(ctx context.Context, userID, bookSlug, title, content string) (skills.ToolResult, error) {
	bk, role, err := s.resolveBook(ctx, userID, bookSlug)
	if errors.Is(err, admin.ErrBookNotFound) {
		return skills.ToolResult{Error: bookNotFoundMsg(bookSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("create: %w", err)
	}
	if !canWrite(role) {
		return skills.ToolResult{Error: fmt.Sprintf("Read-only on %q (role: %s). Ask an owner to grant writer access.", bk.Name, role)}, nil
	}
	p, err := s.backend().CreatePage(ctx, bk.ID, userID, title, content, "")
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("create: %w", err)
	}
	out := fmt.Sprintf("Created page %q in %q (page slug: %s, book slug: %s).", p.Title, bk.Name, p.Slug, bk.Slug)
	return skills.ToolResult{Content: out, Data: pageLocationData(bk.Slug, p.Slug, p.Title), Tokens: len(out) / 4}, nil
}

// pageLocation is the structured side-channel a write tool attaches to
// its ToolResult.Data so downstream consumers (the pipeline's research-
// note detector, the workspace auto-open) can act on the page without
// parsing the prose Content. bk.Slug is the REAL book slug (e.g.
// "personal:{userID}"), not an alias.
type pageLocation struct {
	BookSlug string `json:"book_slug"`
	PageSlug string `json:"page_slug"`
	Title    string `json:"title"`
}

func pageLocationData(bookSlug, pageSlug, title string) json.RawMessage {
	data, err := json.Marshal(pageLocation{BookSlug: bookSlug, PageSlug: pageSlug, Title: title})
	if err != nil {
		return nil // non-fatal: consumers treat absent Data as "no location"
	}
	return data
}

// updatePage replaces the body. Title is preserved when omitted by
// passing nil into PagePatch — the store treats nil fields as
// "leave alone".
func (s *Skill) updatePage(ctx context.Context, userID, bookSlug, pageSlug, newTitle, content string) (skills.ToolResult, error) {
	bk, role, err := s.resolveBook(ctx, userID, bookSlug)
	if errors.Is(err, admin.ErrBookNotFound) {
		return skills.ToolResult{Error: bookNotFoundMsg(bookSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("update: %w", err)
	}
	if !canWrite(role) {
		return skills.ToolResult{Error: fmt.Sprintf("Read-only on %q (role: %s). Ask an owner to grant writer access.", bk.Name, role)}, nil
	}
	patch := admin.PagePatch{Content: &content}
	if strings.TrimSpace(newTitle) != "" {
		patch.Title = &newTitle
	}
	p, err := s.backend().UpdatePage(ctx, bk.ID, pageSlug, userID, patch)
	if errors.Is(err, admin.ErrPageNotFound) {
		return skills.ToolResult{Error: pageNotFoundMsg(bk.Name, pageSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("update: %w", err)
	}
	out := fmt.Sprintf("Updated page %q in %q (page slug: %s). New body is %d chars.", p.Title, bk.Name, p.Slug, len(content))
	return skills.ToolResult{Content: out, Data: pageLocationData(bk.Slug, p.Slug, p.Title), Tokens: len(out) / 4}, nil
}

// appendToPage uses the store's atomic AppendPage — a single in-DB
// concatenation, so an agent adding an item can't lose a human's
// concurrent edit (the old Get+compose+Update had a lost-update
// window). GetPage first only to resolve the slug to an id + 404.
func (s *Skill) appendToPage(ctx context.Context, userID, bookSlug, pageSlug, text string) (skills.ToolResult, error) {
	bk, role, err := s.resolveBook(ctx, userID, bookSlug)
	if errors.Is(err, admin.ErrBookNotFound) {
		return skills.ToolResult{Error: bookNotFoundMsg(bookSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("append: %w", err)
	}
	if !canWrite(role) {
		return skills.ToolResult{Error: fmt.Sprintf("Read-only on %q (role: %s). Ask an owner to grant writer access.", bk.Name, role)}, nil
	}
	cur, err := s.backend().GetPage(ctx, bk.ID, pageSlug)
	if errors.Is(err, admin.ErrPageNotFound) {
		return skills.ToolResult{Error: pageNotFoundMsg(bk.Name, pageSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("append: %w", err)
	}
	p, err := s.backend().AppendPage(ctx, bk.ID, cur.ID, userID, text)
	if errors.Is(err, admin.ErrPageNotFound) {
		return skills.ToolResult{Error: pageNotFoundMsg(bk.Name, pageSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("append: %w", err)
	}
	out := fmt.Sprintf("Appended to %q in %q (page slug: %s): %q", p.Title, bk.Name, p.Slug, text)
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

// patchPage finds an exact-match passage and replaces it. Same
// disambiguation contract as patch_note: the find string must occur
// exactly once in the body.
func (s *Skill) patchPage(ctx context.Context, userID, bookSlug, pageSlug, find, replace string) (skills.ToolResult, error) {
	bk, role, err := s.resolveBook(ctx, userID, bookSlug)
	if errors.Is(err, admin.ErrBookNotFound) {
		return skills.ToolResult{Error: bookNotFoundMsg(bookSlug)}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("patch: %w", err)
	}
	if !canWrite(role) {
		return skills.ToolResult{Error: fmt.Sprintf("Read-only on %q (role: %s). Ask an owner to grant writer access.", bk.Name, role)}, nil
	}
	// Read → find/replace → conditional write, retried on a stale
	// precondition. A find/replace is safely re-appliable: if a human
	// edit lands between our read and write, the CAS rejects us, and we
	// re-read the fresh body and re-locate the passage. This closes the
	// agent-vs-human lost-update window without ever overwriting the
	// human's other lines (unlike the full-replace update_page).
	var title string
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := s.backend().GetPage(ctx, bk.ID, pageSlug)
		if errors.Is(err, admin.ErrPageNotFound) {
			return skills.ToolResult{Error: pageNotFoundMsg(bk.Name, pageSlug)}, nil
		}
		if err != nil {
			return skills.ToolResult{}, fmt.Errorf("patch/read: %w", err)
		}
		title = cur.Title

		body := cur.Content
		matchFind := find
		count := strings.Count(body, matchFind)
		if count == 0 {
			// Forgive trailing whitespace on each line — the most common
			// mismatch when the model copies from a table row.
			trimmed := trimTrailingPerLine(matchFind)
			if strings.Count(body, trimmed) == 1 {
				matchFind = trimmed
				count = 1
			}
		}
		if count == 0 {
			preview := find
			if len(preview) > 120 {
				preview = preview[:120] + "…"
			}
			return skills.ToolResult{Error: fmt.Sprintf("find string not found in page %q. Make sure it matches exactly (case-sensitive). Searched for: %s", cur.Title, preview)}, nil
		}
		if count > 1 {
			return skills.ToolResult{Error: fmt.Sprintf("find string matches %d locations in the page — it must be unique. Include more surrounding context to disambiguate.", count)}, nil
		}

		newBody := strings.Replace(body, matchFind, replace, 1)
		base := cur.UpdatedAt
		_, err = s.backend().UpdatePage(ctx, bk.ID, pageSlug, userID, admin.PagePatch{
			Content: &newBody,
			IfMatch: &base,
		})
		if errors.Is(err, admin.ErrPageStale) {
			continue // someone edited between read and write — retry on fresh content
		}
		if err != nil {
			return skills.ToolResult{}, fmt.Errorf("patch/write: %w", err)
		}
		action := "replaced"
		if replace == "" {
			action = "deleted"
		}
		out := fmt.Sprintf("Patched page %q in %q (page slug: %s): %s %d chars.", cur.Title, bk.Name, pageSlug, action, len(matchFind))
		return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
	}
	return skills.ToolResult{Error: fmt.Sprintf("Couldn't apply the edit to %q — it kept being changed by another writer. Re-read the page and try again.", title)}, nil
}

// ──────────────────────────────────────────────────────────────────
// Formatting
// ──────────────────────────────────────────────────────────────────

func formatPageList(header string, pages []admin.WikiPageSummary) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n\n")
	for i, p := range pages {
		fmt.Fprintf(&b, "%d. %s  (page slug: %s, updated %s)\n",
			i+1,
			fallback(p.Title, "(untitled)"),
			p.Slug,
			humanizeTimeAgo(p.UpdatedAt),
		)
		preview := strings.TrimSpace(stripMarkdown(p.Snippet))
		if preview == "" {
			b.WriteString("   (no content)\n")
		} else {
			runes := []rune(preview)
			if len(runes) > previewMaxRunes {
				preview = string(runes[:previewMaxRunes]) + "…"
			}
			for _, line := range strings.Split(preview, "\n") {
				b.WriteString("   ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
		if i < len(pages)-1 {
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatPageFull(bk *admin.Book, p *admin.WikiPage) string {
	title := fallback(p.Title, "(untitled)")
	body := strings.TrimSpace(p.Content)
	if body == "" {
		body = "(empty)"
	}
	return fmt.Sprintf(
		"# %s\n\n%s\n\n---\nbook: %s (slug: %s)\npage_slug: %s\ncreated: %s\nupdated: %s",
		title,
		body,
		bk.Name,
		bk.Slug,
		p.Slug,
		humanizeTimestamp(p.CreatedAt),
		humanizeTimestamp(p.UpdatedAt),
	)
}

// appendParagraph adds text as a fresh paragraph at the bottom of body.
// Mirrors NotesStore.Append semantics: separate from existing content
// by a blank line.
func appendParagraph(body, text string) string {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return text
	}
	return body + "\n\n" + text
}

func bookNotFoundMsg(slug string) string {
	return fmt.Sprintf("Book %q not found, or you're not a member. Use list_books to see what's available.", slug)
}
func pageNotFoundMsg(bookName, pageSlug string) string {
	return fmt.Sprintf("Page %q not found in %q. Use list_pages or search_pages to discover slugs.", pageSlug, bookName)
}

// ──────────────────────────────────────────────────────────────────
// Helpers (markdown stripping + time formatting — same recipe as
// the notes skill, duplicated to keep this skill self-contained)
// ──────────────────────────────────────────────────────────────────

var (
	mdFencedCode = regexp.MustCompile("(?s)```.*?```")
	mdInlineCode = regexp.MustCompile("`([^`]*)`")
	mdLink       = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	mdImage      = regexp.MustCompile(`!\[[^\]]*\]\([^)]+\)`)
	mdHeading    = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	mdListMarker = regexp.MustCompile(`(?m)^[\s]*[-*+]\s+`)
	mdNumberMark = regexp.MustCompile(`(?m)^[\s]*\d+\.\s+`)
	mdEmphasis   = regexp.MustCompile(`[*_]{1,3}([^*_]+)[*_]{1,3}`)
)

func stripMarkdown(s string) string {
	s = mdFencedCode.ReplaceAllString(s, "")
	s = mdImage.ReplaceAllString(s, "")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdInlineCode.ReplaceAllString(s, "$1")
	s = mdHeading.ReplaceAllString(s, "")
	s = mdListMarker.ReplaceAllString(s, "")
	s = mdNumberMark.ReplaceAllString(s, "")
	s = mdEmphasis.ReplaceAllString(s, "$1")
	return s
}

func trimTrailingPerLine(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func humanizeTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	diff := time.Since(t)
	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	case diff < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(diff.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

func humanizeTimestamp(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
