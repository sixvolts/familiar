package memevents

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ServeSSE is the HTTP handler for the per-session event stream.
// Mount it under a path that captures {session_id}, e.g.
// "/v1/events/{session_id}". CHAT-REARCH S5.
//
// Wire format: standard text/event-stream with a custom "event:"
// field carrying the Kind, "id:" carrying the bus-monotonic event ID,
// and "data:" carrying the full Event JSON. The id field lets the
// browser's EventSource send Last-Event-ID on reconnect, which we
// honor by replaying any events still in the per-session ring
// buffer.
//
// Subscribers are bound to the request context — closing the
// connection (or cancelling ctx) tears down the subscription via
// Bus.Subscribe's goroutine.
func (b *Bus) ServeSSE(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		http.Error(w, "missing session_id", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx response buffering
	w.WriteHeader(http.StatusOK)

	// Replay any events the client missed during reconnect. The
	// EventSource API encodes Last-Event-ID as a header; we accept
	// it as a query string ?since=<id> too for hand-rolled clients.
	var sinceID uint64
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		if id, err := strconv.ParseUint(h, 10, 64); err == nil {
			sinceID = id
		}
	}
	if q := r.URL.Query().Get("since"); q != "" {
		if id, err := strconv.ParseUint(q, 10, 64); err == nil {
			sinceID = id
		}
	}
	for _, e := range b.Replay(sessionID, sinceID) {
		writeSSEEvent(w, e)
	}
	flusher.Flush()

	ch := b.Subscribe(r.Context(), sessionID, 64)
	for e := range ch {
		writeSSEEvent(w, e)
		flusher.Flush()
	}
}

// writeSSEEvent serializes one Event onto the SSE stream. Errors
// (mostly client-disconnect) are silently dropped — the next write
// or the ctx-cancel will surface them as a closed channel.
func writeSSEEvent(w http.ResponseWriter, e Event) {
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Kind, body)
}
