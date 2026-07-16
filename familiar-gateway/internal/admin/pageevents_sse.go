package admin

// GET /console/api/events/pages — Server-Sent Events stream of
// page-saved / page-deleted events for every book the calling user
// is a member of. The editor surfaces (desktop notes + wiki, mobile
// notes + wiki) listen and refresh-when-clean so an idle device
// picks up an edit made on another device in near-real-time.
//
// The membership set is captured once at connect time. New
// book memberships granted mid-connection don't start streaming
// until the EventSource reconnects (which happens naturally on
// network blips, the gateway restart, etc.). Acceptable trade-off
// for v1 — page sharing changes rarely compared to page edits.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/familiar/gateway/internal/pageevents"
)

// AttachPageEvents wires the pageevents.Bus the gateway uses for
// per-user page-mutation push. Optional — when nil, the SSE
// endpoint returns 503 and editors fall back to manual reload.
func (h *Handler) AttachPageEvents(b *pageevents.Bus) { h.pageEvents = b }

// PageEvents returns the bus so the wiring code in cmd/gateway can
// hand the same instance to the WikiStore hooks that call Publish.
// Returns nil before AttachPageEvents runs.
func (h *Handler) PageEvents() *pageevents.Bus { return h.pageEvents }

// servePageEvents streams page events for the calling user. Auth is
// enforced by authRequired (the route is on the authed mux), so by
// the time we're here AuthUser is in the context.
//
// Server-side filtering: the bus broadcasts every page event; this
// handler suppresses events for books the user isn't a member of.
// Without this filter a curious user could read off page IDs from
// every book on the deployment via SSE alone.
func (h *Handler) servePageEvents(w http.ResponseWriter, r *http.Request) {
	if h.pageEvents == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "page events not configured")
		return
	}
	if h.wiki == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "wiki not configured")
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// One-shot membership snapshot. Admins see every book.
	bookIDs := map[string]bool{}
	if au.IsAdmin() {
		// Admin: don't filter. Empty bookIDs map with isAdmin=true
		// below means "accept everything."
	} else {
		// Membership, not listing-visibility: the research evidence
		// books are hidden from ListBooks* but their owner must still
		// receive their page events (the live evidence view depends on
		// it), so gate on raw book_members.
		ids, err := h.wiki.ListMemberBookIDs(r.Context(), au.UserID)
		if err != nil {
			http.Error(w, "load memberships: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, id := range ids {
			bookIDs[id] = true
		}
	}
	allowEvent := func(e pageevents.Event) bool {
		if au.IsAdmin() {
			return true
		}
		return bookIDs[e.BookID]
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx-style proxy buffering off
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Subscribe BEFORE the first hello write so we never miss an
	// event that lands between hello and the first read.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ch := h.pageEvents.Subscribe(ctx, 64)

	// Hello frame — confirms the stream is open from the server's
	// point of view (useful for client-side debug).
	fmt.Fprintf(w, ":hello\n\n")
	flusher.Flush()

	// Heartbeat every 25s so intermediate proxies don't time out
	// the idle connection. SSE comment lines start with ":" and
	// are ignored by the EventSource API on the client.
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ":hb\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !allowEvent(e) {
				continue
			}
			// Frame the event. id is monotonic per-process; clients
			// can pass it back as Last-Event-ID on reconnect for a
			// future replay-on-reconnect upgrade (not implemented).
			raw, mErr := json.Marshal(e)
			if mErr != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Kind, raw); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
