package admin

// Chat folders ("projects") group conversations under a flat,
// per-user label. Distinct from the wiki/notes hierarchy because
// chats are leaves only — there are no folder-folders. The sort_order
// column lets the frontend reorder folders without renaming them.
//
// Future shards work will use a folder as a chat scope, so the
// folder identity (not just its name) needs to be stable. ON DELETE
// SET NULL on conversations.folder_id keeps the chat row alive when
// its folder is removed — the row just falls back to "uncategorized".

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ChatFolder is one row from chat_folders.
type ChatFolder struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrChatFolderNotFound is returned for unknown id or cross-user
// access — same as the conversation pattern so a probe can't tell
// the two apart.
var ErrChatFolderNotFound = errors.New("chat folder: not found")

// ── Store ─────────────────────────────────────────────────────────

// ListFolders returns folders owned by userID, ordered by
// sort_order then name. The frontend renders this list as a flat
// set of expandable sections.
func (s *ConversationStore) ListFolders(ctx context.Context, userID string) ([]*ChatFolder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, user_id, name, sort_order, created_at
		  FROM chat_folders
		 WHERE user_id = $1
		 ORDER BY sort_order ASC, name ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("chat folders: list: %w", err)
	}
	defer rows.Close()
	out := make([]*ChatFolder, 0)
	for rows.Next() {
		var f ChatFolder
		if err := rows.Scan(&f.ID, &f.UserID, &f.Name, &f.SortOrder, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("chat folders: scan: %w", err)
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

// CreateFolder inserts a new folder owned by userID. sort_order
// defaults to MAX+1 within the user's existing folders so a fresh
// folder lands at the bottom.
func (s *ConversationStore) CreateFolder(ctx context.Context, userID, name string) (*ChatFolder, error) {
	if userID == "" {
		return nil, fmt.Errorf("chat folders: create: empty user_id")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("chat folders: create: name required")
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO chat_folders (user_id, name, sort_order)
		VALUES ($1, $2, COALESCE(
			(SELECT MAX(sort_order) + 1 FROM chat_folders WHERE user_id = $1),
			0
		))
		RETURNING id::text, user_id, name, sort_order, created_at`,
		userID, name)
	var f ChatFolder
	if err := row.Scan(&f.ID, &f.UserID, &f.Name, &f.SortOrder, &f.CreatedAt); err != nil {
		return nil, fmt.Errorf("chat folders: create: %w", err)
	}
	return &f, nil
}

// ChatFolderPatch carries the renameable/reorderable fields. nil =
// don't touch.
type ChatFolderPatch struct {
	Name      *string
	SortOrder *int
}

// UpdateFolder applies the patch. Returns ErrChatFolderNotFound for
// unknown id or cross-user access.
func (s *ConversationStore) UpdateFolder(ctx context.Context, id, userID string, p ChatFolderPatch) (*ChatFolder, error) {
	if p.Name == nil && p.SortOrder == nil {
		// Nothing to do — just return the current state. Mirrors the
		// no-op semantics of UpdatePage and Update on conversations.
		return s.getFolder(ctx, id, userID)
	}
	sets := []string{}
	args := []any{}
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return nil, fmt.Errorf("chat folders: name cannot be empty")
		}
		args = append(args, name)
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	if p.SortOrder != nil {
		args = append(args, *p.SortOrder)
		sets = append(sets, fmt.Sprintf("sort_order = $%d", len(args)))
	}
	args = append(args, id, userID)
	q := `UPDATE chat_folders SET ` + strings.Join(sets, ", ") +
		fmt.Sprintf(" WHERE id = $%d::uuid AND user_id = $%d", len(args)-1, len(args)) +
		" RETURNING id::text, user_id, name, sort_order, created_at"
	var f ChatFolder
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&f.ID, &f.UserID, &f.Name, &f.SortOrder, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChatFolderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("chat folders: update: %w", err)
	}
	return &f, nil
}

func (s *ConversationStore) getFolder(ctx context.Context, id, userID string) (*ChatFolder, error) {
	var f ChatFolder
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text, user_id, name, sort_order, created_at
		  FROM chat_folders
		 WHERE id = $1::uuid AND user_id = $2`,
		id, userID,
	).Scan(&f.ID, &f.UserID, &f.Name, &f.SortOrder, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChatFolderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("chat folders: get: %w", err)
	}
	return &f, nil
}

// DeleteFolder removes a folder owned by userID. Conversations
// referencing the folder have their folder_id cleared by the
// ON DELETE SET NULL FK constraint — they fall back to
// "uncategorized" rather than being deleted.
func (s *ConversationStore) DeleteFolder(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM chat_folders WHERE id = $1::uuid AND user_id = $2`,
		id, userID)
	if err != nil {
		return fmt.Errorf("chat folders: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrChatFolderNotFound
	}
	return nil
}

// MoveConversation reassigns a conversation's folder. folderID="" or
// "null" clears the folder (back to uncategorized). A non-empty
// folderID must belong to the same user — cross-user moves are
// rejected as ErrChatFolderNotFound.
func (s *ConversationStore) MoveConversation(ctx context.Context, convID, userID, folderID string) (*Conversation, error) {
	if folderID != "" {
		// Existence + ownership check on the destination folder
		// before we touch the conversation. Reuses getFolder which
		// returns the not-found error for cross-user attempts.
		if _, err := s.getFolder(ctx, folderID, userID); err != nil {
			return nil, err
		}
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE conversations
		   SET folder_id = NULLIF($1, '')::uuid,
		       updated_at = NOW()
		 WHERE id = $2::uuid AND user_id = $3
		RETURNING id::text, user_id, title, model, created_at, updated_at,
		          archived_at, pinned, folder_id::text`,
		folderID, convID, userID)
	var c Conversation
	var fid sql.NullString
	err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.Model,
		&c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.Pinned, &fid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("conversations: move: %w", err)
	}
	if fid.Valid {
		c.FolderID = &fid.String
	}
	return &c, nil
}

// ── Handlers ──────────────────────────────────────────────────────

func (h *Handler) listChatFolders(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	folders, err := h.conversations.ListFolders(r.Context(), userID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": folders})
}

func (h *Handler) createChatFolder(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	f, err := h.conversations.CreateFolder(r.Context(), userID, body.Name)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (h *Handler) patchChatFolder(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body struct {
		Name      *string `json:"name,omitempty"`
		SortOrder *int    `json:"sort_order,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	f, err := h.conversations.UpdateFolder(r.Context(), r.PathValue("id"), userID,
		ChatFolderPatch{Name: body.Name, SortOrder: body.SortOrder})
	if errors.Is(err, ErrChatFolderNotFound) {
		writeJSONError(w, http.StatusNotFound, "folder not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) deleteChatFolder(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	err := h.conversations.DeleteFolder(r.Context(), r.PathValue("id"), userID)
	if errors.Is(err, ErrChatFolderNotFound) {
		writeJSONError(w, http.StatusNotFound, "folder not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// moveConversation reassigns a conversation to a folder (or
// uncategorized). Body: {"folder_id": "<uuid>" | "" | null}.
// Validation order: conversation must be owned by the caller; the
// destination folder (when non-empty) must also be owned by the
// caller.
func (h *Handler) moveConversation(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConversations(w) {
		return
	}
	userID, ok := scopeForConversations(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body struct {
		FolderID *string `json:"folder_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	folderID := ""
	if body.FolderID != nil {
		folderID = *body.FolderID
	}
	c, err := h.conversations.MoveConversation(r.Context(), r.PathValue("id"), userID, folderID)
	if errors.Is(err, ErrConversationNotFound) {
		writeJSONError(w, http.StatusNotFound, "conversation not found")
		return
	}
	if errors.Is(err, ErrChatFolderNotFound) {
		writeJSONError(w, http.StatusBadRequest, "destination folder not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}
