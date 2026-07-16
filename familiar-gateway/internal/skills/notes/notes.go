// Package notes provides a Familiar skill backed by Familiar's own
// Postgres notes table. Six tools — search_notes, list_recent_notes,
// read_note, create_note, update_note, append_to_note — let the
// model read and write the user's personal notes during a
// conversation.
//
// User scoping: SessionContext.UserID drives every query. The
// store enforces ownership at the SQL layer; the skill never has
// to construct cross-user queries.
//
// delete_note remains intentionally absent — destructive
// operations belong in the workspace's Notes panel where the user
// can see what they're nuking before confirming. The skill's six
// tools cover read + non-destructive write.
package notes

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
	// search_notes / list_recent_notes responses so the rendered
	// context block stays compact.
	previewMaxRunes = 150

	// searchHitMax is the spec-defined ceiling on hits returned
	// from search_notes. The model can disambiguate within ten
	// cleanly formatted entries; more wash out.
	searchHitMax = 10

	// defaultListLimit is the default for list_recent_notes when
	// the model doesn't specify.
	defaultListLimit = 10

	// listLimitMax is the upper bound declared in the schema.
	// Inputs above clamp silently — the LLM occasionally over-asks
	// and a soft clamp keeps the answer flowing.
	listLimitMax = 50
)

// NotesBackend is the slice of admin.NotesStore the skill uses.
// Defining the interface here (instead of importing the concrete
// store) keeps the test surface narrow and lets a future refactor
// swap implementations without touching the skill.
type NotesBackend interface {
	List(ctx context.Context, userID, folder string, includeDeleted bool, limit, offset int) ([]admin.NoteSummary, error)
	Get(ctx context.Context, id, userID string) (*admin.Note, error)
	Create(ctx context.Context, userID, title, content, folder string) (*admin.Note, error)
	Update(ctx context.Context, id, userID string, p admin.NotePatch) (*admin.Note, error)
	Append(ctx context.Context, id, userID, text string) (*admin.Note, error)
	Search(ctx context.Context, userID, q string, limit int) ([]admin.NoteSummary, error)
}

// Skill implements the six notes tools backed by NotesBackend.
//
// Backend resolution is lazy via a getter rather than a direct
// pointer. Wiring order in cmd/gateway/main.go has the skill
// register before the notes store is attached to admin.Handler;
// a lazy getter skirts that ordering rather than reorganizing
// main.go's pool-init block. The getter is called on every tool
// invocation — cheap, since it's just a struct field read.
type Skill struct {
	resolve func() NotesBackend
}

// New constructs a notes skill. resolve is a getter that returns
// the NotesBackend or nil. Lazy by design — main.go registers
// the skill before the admin pool block creates the store, and
// `func() NotesBackend { return adminH.NotesStore() }` defers
// the lookup until first tool invocation.
//
// nil resolve is treated as "permanently no backend" — every tool
// invocation returns the not-configured error. Same posture as
// the weather skill without an API key.
func New(resolve func() NotesBackend) *Skill {
	return &Skill{resolve: resolve}
}

func (s *Skill) backend() NotesBackend {
	if s.resolve == nil {
		return nil
	}
	return s.resolve()
}

func (s *Skill) Name() string { return "notes" }
func (s *Skill) Description() string {
	return "Read and write the user's personal notes (Familiar Postgres)"
}
func (s *Skill) Version() string { return "2.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

// ──────────────────────────────────────────────────────────────────
// Tool schemas
// ──────────────────────────────────────────────────────────────────

var searchNotesParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Text to search for in note titles and content."
    }
  },
  "required": ["query"]
}`)

var listRecentNotesParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "limit": {
      "type": "integer",
      "description": "Number of notes to return (1-50, default 10).",
      "minimum": 1,
      "maximum": 50
    }
  }
}`)

var readNoteParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {
      "type": "string",
      "description": "Note ID, typically obtained from search_notes or list_recent_notes."
    }
  },
  "required": ["id"]
}`)

var createNoteParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "title":   {"type": "string", "description": "Note title."},
    "content": {"type": "string", "description": "Markdown body of the note."}
  },
  "required": ["title"]
}`)

var updateNoteParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":      {"type": "string", "description": "ID of the note to replace."},
    "content": {"type": "string", "description": "New markdown body. REPLACES the existing content. Use append_to_note to add text without losing anything."},
    "title":   {"type": "string", "description": "Optional new title. When omitted the existing title is preserved."}
  },
  "required": ["id", "content"]
}`)

var appendNoteParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":   {"type": "string", "description": "ID of the note to append to."},
    "text": {"type": "string", "description": "Text to add to the bottom of the note as a new paragraph."}
  },
  "required": ["id", "text"]
}`)

var patchNoteParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":      {"type": "string", "description": "ID of the note to patch."},
    "find":    {"type": "string", "description": "Exact text to find in the note body. Must match verbatim (case-sensitive). Include enough surrounding context to be unique — typically the full line or table row."},
    "replace": {"type": "string", "description": "Replacement text. The find match is swapped for this string. To delete, set to empty string. To insert after a line, set find to that line and replace to the line plus the new content."}
  },
  "required": ["id", "find", "replace"]
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name:        "search_notes",
			Description: "Search the user's notes by free-text query. Returns up to 10 hits with title, ID, folder, and a content preview.",
			Parameters:  searchNotesParams,
		},
		{
			Name:        "list_recent_notes",
			Description: "List the user's most-recently-updated notes. Use when the user asks for \"recent notes\" or \"what have I been working on\".",
			Parameters:  listRecentNotesParams,
		},
		{
			Name:        "read_note",
			Description: "Read the full markdown body of a single note by its ID. The ID typically comes from search_notes or list_recent_notes.",
			Parameters:  readNoteParams,
		},
		{
			Name:        "create_note",
			Description: "Create a new note with the given title and (optional) markdown body.",
			Parameters:  createNoteParams,
		},
		{
			Name:        "update_note",
			Description: "Replace the body of an existing note. WARNING: this overwrites the current content — use append_to_note when you only want to add text. Optionally retitle.",
			Parameters:  updateNoteParams,
		},
		{
			Name:        "append_to_note",
			Description: "Add a paragraph to the bottom of an existing note. Non-destructive — the existing content is preserved and the new text is added as a fresh paragraph.",
			Parameters:  appendNoteParams,
		},
		{
			Name:        "patch_note",
			Description: "Surgically edit a note by finding and replacing a specific passage. Use this instead of update_note when you need to change, insert, or delete a specific section without rewriting the entire document. The find string must match exactly. To insert a new line after an existing one, set find to the existing line and replace to that line plus the new content.",
			Parameters:  patchNoteParams,
		},
	}
}

// ──────────────────────────────────────────────────────────────────
// Argument types
// ──────────────────────────────────────────────────────────────────

type searchArgs struct {
	Query string `json:"query"`
}

type listArgs struct {
	Limit *int `json:"limit,omitempty"`
}

type readArgs struct {
	ID string `json:"id"`
}

type createArgs struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

type updateArgs struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Title   string `json:"title,omitempty"`
}

type appendArgs struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type patchArgs struct {
	ID      string `json:"id"`
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

// ──────────────────────────────────────────────────────────────────
// Dispatch
// ──────────────────────────────────────────────────────────────────

const noBackendMsg = "Notes backend not configured on this gateway. Ask an admin to wire AttachNotesStore."

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	if s.backend() == nil {
		return skills.ToolResult{Error: noBackendMsg}, nil
	}
	userID := userIDFromCtx(ctx)
	if userID == "" {
		return skills.ToolResult{Error: "no authenticated user on this turn — notes need a user context"}, nil
	}

	switch toolName {
	case "search_notes":
		var args searchArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if strings.TrimSpace(args.Query) == "" {
			return skills.ToolResult{Error: "query is required"}, nil
		}
		return s.searchNotes(ctx, userID, args.Query)

	case "list_recent_notes":
		var args listArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
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
		return s.listRecentNotes(ctx, userID, limit)

	case "read_note":
		var args readArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if strings.TrimSpace(args.ID) == "" {
			return skills.ToolResult{Error: "id is required"}, nil
		}
		return s.readNote(ctx, userID, args.ID)

	case "create_note":
		var args createArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if strings.TrimSpace(args.Title) == "" {
			return skills.ToolResult{Error: "title is required"}, nil
		}
		return s.createNote(ctx, userID, args.Title, args.Content)

	case "update_note":
		var args updateArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if strings.TrimSpace(args.ID) == "" {
			return skills.ToolResult{Error: "id is required"}, nil
		}
		return s.updateNote(ctx, userID, args.ID, args.Title, args.Content)

	case "append_to_note":
		var args appendArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if strings.TrimSpace(args.ID) == "" {
			return skills.ToolResult{Error: "id is required"}, nil
		}
		if strings.TrimSpace(args.Text) == "" {
			return skills.ToolResult{Error: "text is required"}, nil
		}
		return s.appendToNote(ctx, userID, args.ID, args.Text)

	case "patch_note":
		var args patchArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if strings.TrimSpace(args.ID) == "" {
			return skills.ToolResult{Error: "id is required"}, nil
		}
		if args.Find == "" {
			return skills.ToolResult{Error: "find is required (the exact text to locate in the note)"}, nil
		}
		return s.patchNote(ctx, userID, args.ID, args.Find, args.Replace)

	default:
		return skills.ToolResult{}, fmt.Errorf("notes: unknown tool %q", toolName)
	}
}

// userIDFromCtx pulls the canonical user id from SessionContext.
// Returns "" when the pipeline didn't install one — the dispatch
// path turns that into a user-facing "no user" error.
func userIDFromCtx(ctx context.Context) string {
	sc, _ := skills.ContextFrom(ctx)
	return sc.UserID
}

// ──────────────────────────────────────────────────────────────────
// Tool implementations
// ──────────────────────────────────────────────────────────────────

func (s *Skill) searchNotes(ctx context.Context, userID, query string) (skills.ToolResult, error) {
	rows, err := s.backend().Search(ctx, userID, query, searchHitMax)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("search: %w", err)
	}
	if len(rows) == 0 {
		content := fmt.Sprintf("no notes match %q", query)
		return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
	}
	content := formatNoteList("Search results for "+strconv.Quote(query)+":", rows)
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}

func (s *Skill) listRecentNotes(ctx context.Context, userID string, limit int) (skills.ToolResult, error) {
	rows, err := s.backend().List(ctx, userID, "", false, limit, 0)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("list: %w", err)
	}
	if len(rows) == 0 {
		const c = "no notes found"
		return skills.ToolResult{Content: c, Tokens: len(c) / 4}, nil
	}
	content := formatNoteList("Recent notes:", rows)
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}

func (s *Skill) readNote(ctx context.Context, userID, id string) (skills.ToolResult, error) {
	n, err := s.backend().Get(ctx, id, userID)
	if errors.Is(err, admin.ErrNoteNotFound) {
		return skills.ToolResult{Error: "Note not found. The ID may be wrong, or the note may belong to another user."}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("read: %w", err)
	}
	content := formatNoteFull(n)
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}

func (s *Skill) createNote(ctx context.Context, userID, title, content string) (skills.ToolResult, error) {
	n, err := s.backend().Create(ctx, userID, title, content, "")
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("create: %w", err)
	}
	out := fmt.Sprintf("Created note %q (id: %s).", n.Title, n.ID)
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

// updateNote replaces the body. When the model omits a title we
// preserve the existing one — required because admin.NotePatch
// distinguishes "set field" (non-nil) from "leave alone" (nil),
// so passing a nil title just keeps the row's current value
// without a round-trip.
func (s *Skill) updateNote(ctx context.Context, userID, id, newTitle, content string) (skills.ToolResult, error) {
	patch := admin.NotePatch{Content: &content}
	if strings.TrimSpace(newTitle) != "" {
		patch.Title = &newTitle
	}
	n, err := s.backend().Update(ctx, id, userID, patch)
	if errors.Is(err, admin.ErrNoteNotFound) {
		return skills.ToolResult{Error: "Note not found. Use search_notes or list_recent_notes to find it first."}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("update: %w", err)
	}
	out := fmt.Sprintf("Updated note %q (id: %s). New body is %d chars.", n.Title, id, len(content))
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

func (s *Skill) appendToNote(ctx context.Context, userID, id, text string) (skills.ToolResult, error) {
	n, err := s.backend().Append(ctx, id, userID, text)
	if errors.Is(err, admin.ErrNoteNotFound) {
		return skills.ToolResult{Error: "Note not found. Use search_notes or list_recent_notes to find it first."}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("append: %w", err)
	}
	out := fmt.Sprintf("Appended to %q (id: %s): %q", n.Title, id, text)
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

// patchNote reads the note, performs a find→replace on the body,
// and writes the result back. The find string must match exactly
// once. This keeps tool-call payloads tiny regardless of document
// size — the model only provides the anchor and replacement, not
// the entire document.
func (s *Skill) patchNote(ctx context.Context, userID, id, find, replace string) (skills.ToolResult, error) {
	// Read current content.
	n, err := s.backend().Get(ctx, id, userID)
	if errors.Is(err, admin.ErrNoteNotFound) {
		return skills.ToolResult{Error: "Note not found. Use search_notes or list_recent_notes to find it first."}, nil
	}
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("patch/read: %w", err)
	}

	body := n.Content

	// Count occurrences to give a useful error.
	count := strings.Count(body, find)
	if count == 0 {
		// Try a more forgiving match: trim trailing whitespace from
		// each line of the find string. Common source of mismatches
		// when the model copies from a table row.
		trimmedFind := trimTrailingPerLine(find)
		count = strings.Count(body, trimmedFind)
		if count == 1 {
			find = trimmedFind
		} else {
			preview := find
			if len(preview) > 120 {
				preview = preview[:120] + "…"
			}
			return skills.ToolResult{Error: fmt.Sprintf("find string not found in note %q. Make sure it matches exactly (case-sensitive). Searched for: %s", n.Title, preview)}, nil
		}
	}
	if count > 1 {
		return skills.ToolResult{Error: fmt.Sprintf("find string matches %d locations in the note — it must be unique. Include more surrounding context to disambiguate.", count)}, nil
	}

	// Perform the replacement.
	newBody := strings.Replace(body, find, replace, 1)

	// Write it back.
	patch := admin.NotePatch{Content: &newBody}
	_, err = s.backend().Update(ctx, id, userID, patch)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("patch/write: %w", err)
	}

	action := "replaced"
	if replace == "" {
		action = "deleted"
	}
	out := fmt.Sprintf("Patched note %q (id: %s): %s %d chars.", n.Title, id, action, len(find))
	return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
}

// trimTrailingPerLine trims trailing spaces/tabs from each line.
func trimTrailingPerLine(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// ──────────────────────────────────────────────────────────────────
// Formatting
// ──────────────────────────────────────────────────────────────────

// formatNoteList renders a numbered list with title + id + "updated
// X ago" + a markdown-stripped preview. Same shape across
// search_notes and list_recent_notes so the model gets predictable
// structure regardless of which it called.
func formatNoteList(header string, notes []admin.NoteSummary) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n\n")
	for i, n := range notes {
		fmt.Fprintf(&b, "%d. %s  (id: %s, updated %s",
			i+1,
			fallback(n.Title, "(untitled)"),
			n.ID,
			humanizeTimeAgo(n.UpdatedAt),
		)
		if n.Folder != "" {
			fmt.Fprintf(&b, ", folder: %s", n.Folder)
		}
		b.WriteString(")\n")
		preview := strings.TrimSpace(stripMarkdown(n.Snippet))
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
		if i < len(notes)-1 {
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatNoteFull(n *admin.Note) string {
	title := fallback(n.Title, "(untitled)")
	body := strings.TrimSpace(n.Content)
	if body == "" {
		body = "(empty)"
	}
	folder := ""
	if n.Folder != "" {
		folder = "\nfolder: " + n.Folder
	}
	return fmt.Sprintf(
		"# %s\n\n%s\n\n---\nnote_id: %s\ncreated: %s\nupdated: %s%s",
		title,
		body,
		n.ID,
		humanizeTimestamp(n.CreatedAt),
		humanizeTimestamp(n.UpdatedAt),
		folder,
	)
}

// ──────────────────────────────────────────────────────────────────
// Helpers
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

// stripMarkdown removes the most common markdown noise so previews
// read as prose. Not a full parser — drops fenced code first
// (otherwise we'd leak literal backticks), then inline code spans,
// link/image syntax keeping visible text, then leading list /
// heading markers, then emphasis characters.
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
