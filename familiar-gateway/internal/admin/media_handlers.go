package admin

// Page-media console API (MEDIA-DIAGRAMS Phase 1). Upload rides the
// book/page route family so the existing membership + write-role
// gates apply verbatim; serving is membership-gated (any role that
// can read the book can see its images) and always proxied through
// the gateway — clients never touch the media directory.

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/familiar/gateway/internal/media"
)

// AttachMedia wires the media store.
func (h *Handler) AttachMedia(store *media.Store) {
	h.media = store
}

func (h *Handler) requireMedia(w http.ResponseWriter) bool {
	if h.media == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "media storage not configured")
		return false
	}
	return true
}

// uploadPageMedia serves POST /console/api/books/{slug}/page-by-id/{page_id}/media.
// Multipart with a "file" part. Requires write access to the book —
// an image is page content.
func (h *Handler) uploadPageMedia(w http.ResponseWriter, r *http.Request) {
	if !h.requireMedia(w) || !h.ensureWiki(w) {
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
	if _, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID); err != nil {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}

	limit := h.media.MaxBytes()
	if err := r.ParseMultipartForm(limit + (1 << 20)); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid upload: "+err.Error())
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing file part")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if int64(len(data)) > limit {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			"file exceeds the "+strconv.FormatInt(limit>>20, 10)+"MB limit")
		return
	}

	m, err := h.media.SaveImage(r.Context(), pageID, userID, hdr.Filename, data)
	if errors.Is(err, media.ErrUnsupported) {
		writeJSONError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        m.ID,
		"url":       "/console/api/media/" + m.ID,
		"thumb_url": "/console/api/media/" + m.ID + "?thumb=1",
		"width":     m.Width,
		"height":    m.Height,
		"alt_text":  m.AltText,
	})
}

// serveMedia serves GET /console/api/media/{id} (+?thumb=1). Any
// member of the owning book may read; non-members get the same 404
// a wrong id gets, so media ids can't be probed.
func (h *Handler) serveMedia(w http.ResponseWriter, r *http.Request) {
	if !h.requireMedia(w) || !h.ensureWiki(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	m, err := h.media.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, media.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !au.IsAdmin() {
		role, err := h.wiki.MemberRole(r.Context(), m.BookID, au.UserID)
		if err != nil || role == "" {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
	}

	f, ct, err := h.media.Open(m, r.URL.Query().Get("thumb") == "1")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "media bytes missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// ServeContent handles ranges + If-Modified-Since for free.
	info, err := f.Stat()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "stat media")
		return
	}
	http.ServeContent(w, r, m.Filename, info.ModTime(), f)
}

// listPageMedia serves GET .../page-by-id/{page_id}/media — the
// page's media rows. Membership-gated (scopeForWiki); the client
// share-render sync uses it to know which diagram PNGs exist.
func (h *Handler) listPageMedia(w http.ResponseWriter, r *http.Request) {
	if !h.requireMedia(w) || !h.ensureWiki(w) {
		return
	}
	b, _, _, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	pageID := r.PathValue("page_id")
	if _, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID); err != nil {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	items, err := h.media.ListForPage(r.Context(), pageID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// deleteMedia serves DELETE /console/api/media/{id}. Requires write
// access to the owning book — removing an image is a content edit.
func (h *Handler) deleteMedia(w http.ResponseWriter, r *http.Request) {
	if !h.requireMedia(w) || !h.ensureWiki(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	m, err := h.media.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, media.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !au.IsAdmin() {
		role, err := h.wiki.MemberRole(r.Context(), m.BookID, au.UserID)
		if err != nil || (role != "owner" && role != "writer") {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
	}
	if err := h.media.Delete(r.Context(), m.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
