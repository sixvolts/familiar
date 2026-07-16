package admin

// Workspace conversations + messages CRUD (FAMILIAR-WORKSPACE-SPEC
// Phase 1a). Storage backs the Chat surface in the workspace; the
// LLM completions endpoint at /v1/chat/completions stays unchanged
// — this file is purely about persisting threads and turns so the
// workspace can list, paginate, rename, archive, and reload them.
//
// Per-role scoping mirrors the dashboard pattern (see
// dashboardScopeFor): non-admins see their own conversations, admins
// can pass ?user_id=<id> to view another user's history. Per-resource
// authz (a non-admin asking for conversation X belonging to user Y)
// is enforced at GET/PATCH/DELETE time by joining on user_id; the
// handler returns 404 to avoid leaking existence.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ──────────────────────────────────────────────────────────────────
// Types
// ──────────────────────────────────────────────────────────────────

// Conversation is one chat thread. Field shape mirrors the
// conversations table from migrate.go's "workspace_conversations"
// migration; nullable timestamps use *time.Time so the JSON wire
// shape can carry null for "not archived".
type Conversation struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Title      string     `json:"title"`
	Model      string     `json:"model"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	Pinned     bool       `json:"pinned"`
	// FolderID is the chat folder this conversation lives in (nil =
	// uncategorized). Frontend groups by this field in the sidebar.
	FolderID *string `json:"folder_id,omitempty"`
}

// Message is one turn inside a conversation. ToolCalls is the raw
// JSONB blob the LLM produced (function calls). ReasoningContent
// holds the model's extended-thinking trace (and any pipeline
// status updates the gateway pumped through the same SSE channel)
// so reloading a conversation surfaces the trace alongside the
// final response — essential for debugging. Token counts are
// nullable because system messages and early reconstructions don't
// always carry them.
type Message struct {
	ID               string          `json:"id"`
	ConversationID   string          `json:"conversation_id"`
	Role             string          `json:"role"` // user|assistant|system|tool
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Model            string          `json:"model,omitempty"`
	ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	TokensPrompt     *int            `json:"tokens_prompt,omitempty"`
	TokensCompletion *int            `json:"tokens_completion,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
}

// ErrConversationNotFound is returned by the store + by handlers
// when an id is unknown OR when the caller isn't entitled to see
// it. Conflating "missing" with "forbidden" stops a non-admin from
// probing for other users' conversation IDs.
var ErrConversationNotFound = errors.New("conversation: not found")

// ──────────────────────────────────────────────────────────────────
// Store
// ──────────────────────────────────────────────────────────────────

// ConversationStore is the persistence surface for the Chat panel.
// Backed by the shared *db.Pool. Every method takes a userID for
// authz scoping; passing the empty string is treated as "no scope"
// and is reserved for admin overrides — handlers must NOT pass ""
// for non-admin sessions.
type ConversationStore struct {
	db *db.Pool
}

// NewConversationStore wires a store onto the shared pool. Returns
// nil if pool is nil so the constructor mirrors CredentialStore /
// SessionStore in this package.
func NewConversationStore(pool *db.Pool) *ConversationStore {
	if pool == nil || pool.DB == nil {
		return nil
	}
	return &ConversationStore{db: pool}
}

// List returns conversations for userID, newest-updated first.
// includeArchived=false hides archived rows. limit/offset paginate;
// limit clamps to [1, 200] with default 50.
//
// External conversations (external_key NOT NULL — Slack channels + DMs
// the adapter and slack_dm digests hydrate) are excluded: those threads
// live on their origin surface (Slack) and shouldn't clutter the
// workspace chat list. Their context is still persisted and reachable by
// id; only this list view omits them.
func (s *ConversationStore) List(ctx context.Context, userID string, includeArchived bool, limit, offset int) ([]*Conversation, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	q := `
		SELECT id::text, user_id, title, model, created_at, updated_at,
		       archived_at, pinned, folder_id::text
		  FROM conversations
		 WHERE user_id = $1
		   AND external_key IS NULL`
	if !includeArchived {
		q += ` AND archived_at IS NULL`
	}
	q += ` ORDER BY pinned DESC, updated_at DESC LIMIT $2 OFFSET $3`
	rows, err := s.db.QueryContext(ctx, q, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("conversations: list: %w", err)
	}
	defer rows.Close()
	out := make([]*Conversation, 0)
	for rows.Next() {
		var c Conversation
		var folderID sql.NullString
		if err := rows.Scan(&c.ID, &c.UserID, &c.Title, &c.Model,
			&c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.Pinned, &folderID); err != nil {
			return nil, fmt.Errorf("conversations: scan: %w", err)
		}
		if folderID.Valid {
			c.FolderID = &folderID.String
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// Get returns one conversation by id, scoped to userID. Returns
// ErrConversationNotFound when the row is missing OR belongs to a
// different user — same status to both prevents existence probes.
func (s *ConversationStore) Get(ctx context.Context, id, userID string) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, user_id, title, model, created_at, updated_at,
		       archived_at, pinned, folder_id::text
		  FROM conversations
		 WHERE id = $1::uuid AND user_id = $2`, id, userID)
	var c Conversation
	var folderID sql.NullString
	err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.Model,
		&c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.Pinned, &folderID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("conversations: get: %w", err)
	}
	if folderID.Valid {
		c.FolderID = &folderID.String
	}
	return &c, nil
}

// OwnsConversation reports whether conversationID exists and is owned
// by userID. A non-UUID id, a missing row, or a row owned by someone
// else all return (false, nil) — only a real DB error returns non-nil.
//
// The native chat adapter calls this before binding a client-supplied
// conversation_id to an in-memory session, closing the IDOR where a
// caller who knows another user's conversation UUID could read that
// conversation's turns (via hydration) or write into it. See
// EXTERNAL-READINESS-REVIEW.md P0.
func (s *ConversationStore) OwnsConversation(ctx context.Context, conversationID, userID string) (bool, error) {
	if conversationID == "" || userID == "" {
		return false, nil
	}
	if _, err := uuid.Parse(conversationID); err != nil {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM conversations
			 WHERE id = $1::uuid AND user_id = $2
		)`, conversationID, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("conversations: ownership check: %w", err)
	}
	return exists, nil
}

// Create inserts a new conversation owned by userID. title and model
// fall back to the column defaults when empty.
func (s *ConversationStore) Create(ctx context.Context, userID, title, model string) (*Conversation, error) {
	if userID == "" {
		return nil, fmt.Errorf("conversations: create: empty user_id")
	}
	if model == "" {
		model = "familiar"
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO conversations (user_id, title, model)
		VALUES ($1, $2, $3)
		RETURNING id::text, user_id, title, model, created_at, updated_at,
		          archived_at, pinned, folder_id::text`,
		userID, title, model)
	var c Conversation
	var folderID sql.NullString
	if err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.Model,
		&c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.Pinned, &folderID); err != nil {
		return nil, fmt.Errorf("conversations: create: %w", err)
	}
	if folderID.Valid {
		c.FolderID = &folderID.String
	}
	return &c, nil
}

// EnsureExternalConversation returns the conversation row bound to
// externalKey, creating it on first use. It's the entry point for
// adapters that own a stable external identity (a Slack DM, a Slack
// thread) rather than a workspace-minted conversation UUID: the same
// key always resolves to the same row, so the gateway's hydration
// path (keyed by conversation id) replays everything posted into that
// conversation — including scheduled-action deliveries.
//
// Idempotent via INSERT ... ON CONFLICT on the external_key unique
// index; the title is only applied on first insert (an existing
// conversation keeps whatever title it has, including a user rename).
// userID owns the row and must match the caller's resolved identity —
// a DM's external key embeds that user, so ownership is consistent
// across the adapter (inbound) and the scheduled deliverer (outbound).
func (s *ConversationStore) EnsureExternalConversation(ctx context.Context, userID, externalKey, title string) (*Conversation, error) {
	if userID == "" {
		return nil, fmt.Errorf("conversations: ensure external: empty user_id")
	}
	if externalKey == "" {
		return nil, fmt.Errorf("conversations: ensure external: empty external_key")
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO conversations (user_id, title, model, external_key)
		VALUES ($1, $2, 'familiar', $3)
		ON CONFLICT (external_key) WHERE external_key IS NOT NULL
		    DO UPDATE SET external_key = EXCLUDED.external_key
		RETURNING id::text, user_id, title, model, created_at, updated_at,
		          archived_at, pinned, folder_id::text`,
		userID, title, externalKey)
	var c Conversation
	var folderID sql.NullString
	if err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.Model,
		&c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.Pinned, &folderID); err != nil {
		return nil, fmt.Errorf("conversations: ensure external: %w", err)
	}
	if folderID.Valid {
		c.FolderID = &folderID.String
	}
	return &c, nil
}

// Update applies the patch fields. Each *string/*bool is "set when
// non-nil"; nil leaves the column alone. archive=true sets
// archived_at = NOW(); archive=false clears it. updated_at is
// always bumped on a successful update.
type ConversationPatch struct {
	Title   *string
	Model   *string
	Pinned  *bool
	Archive *bool
}

func (s *ConversationStore) Update(ctx context.Context, id, userID string, p ConversationPatch) (*Conversation, error) {
	// Build a dynamic UPDATE so untouched columns aren't rewritten.
	// Positional args in order; each branch appends one expression
	// + matching arg slice.
	sets := []string{"updated_at = NOW()"}
	args := []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		sets = append(sets, strings.ReplaceAll(expr, "$?", "$"+strconv.Itoa(len(args))))
	}
	if p.Title != nil {
		add("title = $?", *p.Title)
	}
	if p.Model != nil && *p.Model != "" {
		add("model = $?", *p.Model)
	}
	if p.Pinned != nil {
		add("pinned = $?", *p.Pinned)
	}
	if p.Archive != nil {
		if *p.Archive {
			sets = append(sets, "archived_at = NOW()")
		} else {
			sets = append(sets, "archived_at = NULL")
		}
	}
	args = append(args, id, userID)
	q := `UPDATE conversations SET ` + strings.Join(sets, ", ") +
		` WHERE id = $` + strconv.Itoa(len(args)-1) + `::uuid` +
		` AND user_id = $` + strconv.Itoa(len(args)) +
		` RETURNING id::text, user_id, title, model, created_at, updated_at,
		           archived_at, pinned, folder_id::text`
	row := s.db.QueryRowContext(ctx, q, args...)
	var c Conversation
	var folderID sql.NullString
	err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.Model,
		&c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.Pinned, &folderID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("conversations: update: %w", err)
	}
	if folderID.Valid {
		c.FolderID = &folderID.String
	}
	return &c, nil
}

// Delete hard-deletes a conversation owned by userID. Messages
// cascade via the FK. Returns ErrConversationNotFound for unknown
// id or non-owner.
func (s *ConversationStore) Delete(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE id = $1::uuid AND user_id = $2`,
		id, userID)
	if err != nil {
		return fmt.Errorf("conversations: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// Messages returns the message log for a conversation in
// chronological order. Caller must verify ownership via Get first.
// messageColumns is the shared SELECT list for message rows; scanMessageRows
// consumes exactly these columns in order.
const messageColumns = `id::text, conversation_id::text, role, content,
	       COALESCE(model, ''),
	       tool_calls,
	       COALESCE(tool_call_id, ''),
	       tokens_prompt, tokens_completion, created_at,
	       COALESCE(reasoning_content, '')`

func scanMessageRows(rows *sql.Rows) ([]*Message, error) {
	out := make([]*Message, 0)
	for rows.Next() {
		var m Message
		var tc []byte
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content,
			&m.Model, &tc, &m.ToolCallID,
			&m.TokensPrompt, &m.TokensCompletion, &m.CreatedAt,
			&m.ReasoningContent); err != nil {
			return nil, fmt.Errorf("messages: scan: %w", err)
		}
		if len(tc) > 0 {
			m.ToolCalls = json.RawMessage(tc)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (s *ConversationStore) Messages(ctx context.Context, conversationID string, limit, offset int) ([]*Message, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+messageColumns+`
		  FROM messages
		 WHERE conversation_id = $1::uuid
		 ORDER BY seq ASC
		 LIMIT $2 OFFSET $3`,
		conversationID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("messages: list: %w", err)
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

// allMessagesCap bounds MessagesAll — a memory/payload backstop far above
// any realistic chat, NOT a page size. When a conversation somehow exceeds
// it, we keep the most RECENT rows (the active conversation), never the
// ancient beginning.
const allMessagesCap = 5000

// MessagesAll returns a conversation's whole thread, oldest first. It backs
// getConversation, which the workspace loads in one shot on open/refresh
// (there is no client-side lazy-load). The paginated Messages() defaults a
// zero limit to 100, so calling it here silently dropped every message past
// the first 100 on reload — a long conversation lost its recent turns from
// the UI even though the server session kept the context. This loads the
// full thread instead. The DESC-then-ASC shape means a conversation over
// the cap keeps its latest rows, so the active end is never the part that
// falls off.
func (s *ConversationStore) MessagesAll(ctx context.Context, conversationID string) ([]*Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+messageColumns+` FROM (
			SELECT id, conversation_id, role, content, model, tool_calls,
			       tool_call_id, tokens_prompt, tokens_completion,
			       created_at, reasoning_content, seq
			  FROM messages
			 WHERE conversation_id = $1::uuid
			 ORDER BY seq DESC
			 LIMIT $2
		) sub
		ORDER BY seq ASC`,
		conversationID, allMessagesCap)
	if err != nil {
		return nil, fmt.Errorf("messages: list all: %w", err)
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

// LoadRecentTurns invokes visit for each of the last `limit`
// user/assistant/tool messages in conversationID, in chronological
// order (oldest first). Used by the pipeline to repopulate an
// in-memory session's verbatim turns after a gateway restart —
// see SESSION-HYDRATION.md.
//
// Tool-call shape is preserved: assistant rows that invoked tools
// surface their tool_calls JSON; tool-result rows surface their
// tool_call_id. Without these, the model loses the work it did in
// a prior turn's agentic loop (the "lost context across turns"
// regression Canyon hit).
//
// reasoning_content is NOT loaded — historical thinking is
// intentionally stripped (the context builder drops it anyway).
//
// A non-UUID conversationID is treated as "no messages" (no error)
// so non-workspace adapters whose session ids aren't UUIDs (e.g.
// the Slack sha256 scheme) get a harmless no-op when the pipeline
// speculatively asks for hydration.
func (s *ConversationStore) LoadRecentTurns(ctx context.Context, conversationID string, limit int, visit func(role, content string, toolCalls []byte, toolCallID string)) error {
	if visit == nil || limit <= 0 {
		return nil
	}
	if _, err := uuid.Parse(conversationID); err != nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, tool_calls, COALESCE(tool_call_id, '') FROM (
			SELECT role, content, tool_calls, tool_call_id, seq
			  FROM messages
			 WHERE conversation_id = $1::uuid
			   AND role IN ('user', 'assistant', 'tool')
			 ORDER BY seq DESC
			 LIMIT $2
		) sub
		ORDER BY seq ASC`,
		conversationID, limit)
	if err != nil {
		return fmt.Errorf("messages: recent turns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var role, content, toolCallID string
		var toolCalls []byte
		if err := rows.Scan(&role, &content, &toolCalls, &toolCallID); err != nil {
			return fmt.Errorf("messages: recent turns scan: %w", err)
		}
		visit(role, content, toolCalls, toolCallID)
	}
	return rows.Err()
}

// AppendIntermediateMessages writes the gateway-side messages from
// an agentic loop iteration (assistant w/ tool_calls, tool result,
// …) into the conversation. The frontend continues to write the
// user prompt and the final assistant text; these fill in the
// middle so the row sequence in the messages table mirrors the
// LLM-side message sequence and post-restart hydration can replay
// it. Wrapped in a transaction so a partial failure doesn't leave
// the row sequence half-written. A non-UUID conversationID is a
// harmless no-op (matches LoadRecentTurns).
//
// The message-shape type lives in pipeline package so the pipeline
// can satisfy its own ConversationStore interface without two
// packages owning the same struct.
func (s *ConversationStore) AppendIntermediateMessages(ctx context.Context, conversationID string, msgs []pipeline.IntermediateMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	if _, err := uuid.Parse(conversationID); err != nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("messages: begin tx: %w", err)
	}
	defer tx.Rollback()
	for _, m := range msgs {
		var tc any
		if len(m.ToolCalls) > 0 {
			tc = string(m.ToolCalls)
		}
		toolCallID := sql.NullString{String: m.ToolCallID, Valid: m.ToolCallID != ""}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages
			    (conversation_id, role, content, tool_calls, tool_call_id)
			VALUES ($1::uuid, $2, $3, $4::jsonb, $5)`,
			conversationID, m.Role, m.Content, tc, toolCallID); err != nil {
			return fmt.Errorf("messages: append intermediate: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = NOW() WHERE id = $1::uuid`, conversationID); err != nil {
		return fmt.Errorf("messages: bump conversation: %w", err)
	}
	return tx.Commit()
}

// AppendMessage records one turn. Returns the inserted row with its
// generated id + created_at. Used by the workspace's send-message
// flow on both the user prompt and the assistant response (the
// latter after streaming completes — partial streaming chunks live
// in the workspace JS until terminal).
func (s *ConversationStore) AppendMessage(ctx context.Context, m *Message) (*Message, error) {
	if m.ConversationID == "" || m.Role == "" {
		return nil, fmt.Errorf("messages: append: conversation_id and role required")
	}
	var tc any
	if len(m.ToolCalls) > 0 {
		tc = string(m.ToolCalls)
	}
	model := sql.NullString{String: m.Model, Valid: m.Model != ""}
	toolCallID := sql.NullString{String: m.ToolCallID, Valid: m.ToolCallID != ""}
	reasoning := sql.NullString{String: m.ReasoningContent, Valid: m.ReasoningContent != ""}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO messages
		    (conversation_id, role, content, model, tool_calls, tool_call_id,
		     tokens_prompt, tokens_completion, reasoning_content)
		VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6, $7, $8, $9)
		RETURNING id::text, conversation_id::text, role, content,
		          COALESCE(model, ''), tool_calls, COALESCE(tool_call_id, ''),
		          tokens_prompt, tokens_completion, created_at,
		          COALESCE(reasoning_content, '')`,
		m.ConversationID, m.Role, m.Content, model, tc, toolCallID,
		nullableInt(m.TokensPrompt), nullableInt(m.TokensCompletion), reasoning)
	var out Message
	var tcOut []byte
	if err := row.Scan(&out.ID, &out.ConversationID, &out.Role, &out.Content,
		&out.Model, &tcOut, &out.ToolCallID,
		&out.TokensPrompt, &out.TokensCompletion, &out.CreatedAt,
		&out.ReasoningContent); err != nil {
		return nil, fmt.Errorf("messages: append: %w", err)
	}
	if len(tcOut) > 0 {
		out.ToolCalls = json.RawMessage(tcOut)
	}
	// Bump conversation.updated_at so list ordering tracks chat
	// activity, not just metadata edits.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET updated_at = NOW() WHERE id = $1::uuid`,
		m.ConversationID); err != nil {
		return nil, fmt.Errorf("messages: bump conversation: %w", err)
	}
	return &out, nil
}

func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// suppress lib/pq unused-import error if a later refactor drops the
// only direct reference. lib/pq is the side-effect import that
// registers the postgres driver via internal/db, but if a future
// edit prunes a usage we keep the package wired in case other
// queries lean on its types. Cheap insurance.
var _ = pq.StringArray(nil)

// ──────────────────────────────────────────────────────────────────
// Wire-up
// ──────────────────────────────────────────────────────────────────

// AttachConversationStore wires a conversation store onto the
// handler. Idempotent. Mirrors AttachShardStore / AttachGraphStore
// in shape so main.go's wiring follows one pattern.
func (h *Handler) AttachConversationStore(s *ConversationStore) {
	h.conversations = s
}

// ──────────────────────────────────────────────────────────────────
// Handlers
// ──────────────────────────────────────────────────────────────────

// scopeForConversations resolves the userID whose conversations the
// caller is entitled to see, mirroring dashboardScopeFor. Non-admin
// callers always see their own data regardless of any ?user_id=
// override; admin's override is honored.
func scopeForConversations(r *http.Request) (string, bool) {
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		return "", false
	}
	return adminUserScope(r, au), true
}

func (h *Handler) ensureConversations(w http.ResponseWriter) bool {
	if h.conversations == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "conversations not configured on this deploy")
		return false
	}
	return true
}

// listConversations serves GET /console/api/conversations.
// Query params: ?archived=true (include archived), ?limit=, ?offset=.
func (h *Handler) listConversations(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	q := r.URL.Query()
	includeArchived := q.Get("archived") == "true"
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	rows, err := h.conversations.List(r.Context(), userID, includeArchived, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// createConversation serves POST /console/api/conversations.
// Body: {"title"?: string, "model"?: string}. Both optional; the
// store fills in defaults.
func (h *Handler) createConversation(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	if au, ok := AuthUserFrom(r.Context()); ok && au.IsShardSession() && au.Permissions != nil && !au.Permissions.CanChat {
		writeJSONError(w, http.StatusForbidden, "chat disabled for this session")
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body struct {
		Title string `json:"title"`
		Model string `json:"model"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	// Shard-bound conversation (SKILL-PACKAGES-SPEC Phase 1): the
	// binding is validated at creation and immutable afterward —
	// /api/chat re-resolves it per message, but a row must never be
	// born pointing at a shard the creator can't chat with.
	if refusal := h.validateShardChatBinding(r.Context(), body.Model, userID); refusal != "" {
		writeJSONError(w, http.StatusBadRequest, refusal)
		return
	}
	c, err := h.conversations.Create(r.Context(), userID, body.Title, body.Model)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// getConversation serves GET /console/api/conversations/{id}.
// Returns the conversation row plus its messages (no pagination on
// this endpoint — pagination lives at /messages so the workspace
// can lazy-load older history without re-fetching the metadata).
func (h *Handler) getConversation(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	id := r.PathValue("id")
	c, err := h.conversations.Get(r.Context(), id, userID)
	if errors.Is(err, ErrConversationNotFound) {
		writeJSONError(w, http.StatusNotFound, "conversation not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	msgs, err := h.conversations.MessagesAll(r.Context(), c.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conversation": c,
		"messages":     msgs,
	})
}

// patchConversation serves PATCH /console/api/conversations/{id}.
// Body fields are all optional pointers; only present fields are
// applied. Pass {"archive": true} to hide; {"archive": false} to
// restore.
func (h *Handler) patchConversation(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	id := r.PathValue("id")
	var body struct {
		Title   *string `json:"title,omitempty"`
		Model   *string `json:"model,omitempty"`
		Pinned  *bool   `json:"pinned,omitempty"`
		Archive *bool   `json:"archive,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// A shard binding is immutable: refuse any model patch that
	// would create, change, or drop one. Re-pointing a thread at a
	// different brain mid-history is a footgun, and dropping the
	// binding would silently upgrade the thread to the trusted path.
	if body.Model != nil {
		cur, getErr := h.conversations.Get(r.Context(), id, userID)
		if getErr == nil && (strings.HasPrefix(cur.Model, shardModelPrefix) ||
			strings.HasPrefix(*body.Model, shardModelPrefix)) &&
			cur.Model != *body.Model {
			writeJSONError(w, http.StatusBadRequest, "a conversation's shard binding is immutable")
			return
		}
	}
	c, err := h.conversations.Update(r.Context(), id, userID, ConversationPatch{
		Title: body.Title, Model: body.Model, Pinned: body.Pinned, Archive: body.Archive,
	})
	if errors.Is(err, ErrConversationNotFound) {
		writeJSONError(w, http.StatusNotFound, "conversation not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// deleteConversation serves DELETE /console/api/conversations/{id}.
// Hard delete; messages cascade via FK.
func (h *Handler) deleteConversation(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	id := r.PathValue("id")
	err := h.conversations.Delete(r.Context(), id, userID)
	if errors.Is(err, ErrConversationNotFound) {
		writeJSONError(w, http.StatusNotFound, "conversation not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
}

// appendConversationMessage serves POST /console/api/conversations/{id}/messages.
// The workspace's Chat surface uses this to persist both the user
// prompt (before calling /v1/chat/completions) and the final
// assistant response (after the SSE stream terminates). Body shape:
//
//	{
//	  "role": "user" | "assistant" | "system" | "tool",
//	  "content": "...",
//	  "model": "familiar",          (optional)
//	  "tool_calls": [...],          (optional; raw JSON)
//	  "tool_call_id": "...",        (optional; for role=tool)
//	  "tokens_prompt": 123,         (optional)
//	  "tokens_completion": 456      (optional)
//	}
//
// Ownership-checked: a non-admin appending to someone else's
// conversation gets 404.
func (h *Handler) appendConversationMessage(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	if au, ok := AuthUserFrom(r.Context()); ok && au.IsShardSession() && au.Permissions != nil && !au.Permissions.CanChat {
		writeJSONError(w, http.StatusForbidden, "chat disabled for this session")
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	id := r.PathValue("id")
	if _, err := h.conversations.Get(r.Context(), id, userID); err != nil {
		if errors.Is(err, ErrConversationNotFound) {
			writeJSONError(w, http.StatusNotFound, "conversation not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var body struct {
		Role             string          `json:"role"`
		Content          string          `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		Model            string          `json:"model"`
		ToolCalls        json.RawMessage `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		TokensPrompt     *int            `json:"tokens_prompt"`
		TokensCompletion *int            `json:"tokens_completion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if body.Role == "" {
		writeJSONError(w, http.StatusBadRequest, "role required")
		return
	}
	m, err := h.conversations.AppendMessage(r.Context(), &Message{
		ConversationID:   id,
		Role:             body.Role,
		Content:          body.Content,
		ReasoningContent: body.ReasoningContent,
		Model:            body.Model,
		ToolCalls:        body.ToolCalls,
		ToolCallID:       body.ToolCallID,
		TokensPrompt:     body.TokensPrompt,
		TokensCompletion: body.TokensCompletion,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// listConversationMessages serves GET /console/api/conversations/{id}/messages.
// Paginated with ?limit=&offset= so the workspace can lazy-load
// older history without re-fetching the conversation metadata.
func (h *Handler) listConversationMessages(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	id := r.PathValue("id")
	// Ownership check before exposing messages — Get returns
	// ErrConversationNotFound for missing OR non-owned, which we
	// surface as 404 to the caller.
	if _, err := h.conversations.Get(r.Context(), id, userID); err != nil {
		if errors.Is(err, ErrConversationNotFound) {
			writeJSONError(w, http.StatusNotFound, "conversation not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	msgs, err := h.conversations.Messages(r.Context(), id, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": msgs})
}
