package admin

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/familiar/gateway/internal/skills/weather"
)

// AttachWeather wires the weather skill so the Home weather widget
// endpoint (/console/api/home/weather) can serve structured
// forecasts. Optional — when nil the endpoint returns 503.
func (h *Handler) AttachWeather(w *weather.Skill) { h.weather = w }

// homeWeather serves GET /console/api/home/weather. Coordinate
// resolution:
//
//  1. ?lat=&lon= query params from browser geolocation. When
//     either is present both must be valid.
//  2. Neither — respond 200 with {"error":"no_location"} so the
//     frontend prompts for browser geolocation. There is no
//     server-side location fallback: the per-user working-context
//     blob that once stored a location string was retired.
func (h *Handler) homeWeather(w http.ResponseWriter, r *http.Request) {
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	if h.weather == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "weather is not configured")
		return
	}

	// 1. Explicit browser-supplied coordinates win.
	latRaw := r.URL.Query().Get("lat")
	lonRaw := r.URL.Query().Get("lon")
	if latRaw != "" || lonRaw != "" {
		lat, errLat := strconv.ParseFloat(latRaw, 64)
		lon, errLon := strconv.ParseFloat(lonRaw, 64)
		if errLat != nil || errLon != nil ||
			lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			writeJSONError(w, http.StatusBadRequest, "valid lat and lon query params are required")
			return
		}
		rep, err := h.weather.HomeForecast(r.Context(), lat, lon)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rep)
		return
	}

	// 2. No explicit coordinates — let the frontend try browser
	// geolocation. A missing location isn't a server error.
	writeJSON(w, http.StatusOK, map[string]any{"error": "no_location"})
}

// HomePin is one row in the unified Home pins list. Carries just
// enough to render a row (kind + id + title + meta + when) without
// the full body — the client follows up to the kind-specific GET
// endpoint when the user actually opens it.
type HomePin struct {
	Kind      string    `json:"kind"` // "note" | "chat" | "wiki"
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Snippet   string    `json:"snippet,omitempty"`   // notes only
	Folder    string    `json:"folder,omitempty"`    // notes only
	Model     string    `json:"model,omitempty"`     // chats only
	BookSlug  string    `json:"book_slug,omitempty"` // wiki only
	BookName  string    `json:"book_name,omitempty"` // wiki only
	UpdatedAt time.Time `json:"updated_at"`
}

// homePins serves GET /console/api/home/pins. Returns pinned notes
// (personal-book pages), pinned wiki pages, and pinned conversations
// as one ordered list (newest-updated first) so Home can render a
// unified Pinned section without separate fetches.
//
// Note pins come exclusively from user_page_prefs via the wiki store
// — NOT the legacy notes table. The notesmigration copied notes into
// personal-book pages keeping the same id, so the notes table and
// wiki_pages diverged: deleting a note in the panel soft-deletes the
// page but leaves the notes-table row pinned. Reading notes.pinned
// here resurfaced those as dead "echo" pins. user_page_prefs joined
// to live wiki_pages is the single source of truth.
func (h *Handler) homePins(w http.ResponseWriter, r *http.Request) {
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	userID := adminUserScope(r, au)

	out := make([]HomePin, 0, 16)

	if h.conversations != nil {
		crows, err := h.conversations.List(r.Context(), userID, false, 50, 0)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, c := range crows {
			if !c.Pinned {
				continue
			}
			out = append(out, HomePin{
				Kind:      "chat",
				ID:        c.ID,
				Title:     c.Title,
				Model:     c.Model,
				UpdatedAt: c.UpdatedAt,
			})
		}
	}

	if h.wiki != nil {
		// Wiki page pins live in user_page_prefs and span every book
		// the caller is a member of. ListPinnedPages already excludes
		// archived books + deleted pages and joins enough to deep-link
		// back to the page from Home.
		wpins, err := h.wiki.ListPinnedPages(r.Context(), userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, p := range wpins {
			// Personal-book pages are the Notes panel's pages — render
			// them as kind="note" so Home deep-links to #notes/<id>.
			// Shared-book pages stay kind="wiki" and carry their book.
			pin := HomePin{
				Kind:      "wiki",
				ID:        p.PageID,
				Title:     p.Title,
				UpdatedAt: p.UpdatedAt,
			}
			if p.IsPersonal {
				pin.Kind = "note"
			} else {
				pin.BookSlug = p.BookSlug
				pin.BookName = p.BookName
			}
			out = append(out, pin)
		}
	}

	// Stable cross-kind sort: most-recently-touched first. Ties
	// fall back to (kind, id) so the list is deterministic.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})

	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
