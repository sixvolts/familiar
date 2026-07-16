// Package pageevents is the in-process fanout point for wiki / note
// page mutations. Every successful Create / Update / Delete (and the
// "share toggled" / "moved" siblings, in time) calls Publish; SSE
// handlers Subscribe to receive events for the books the connected
// user is a member of.
//
// Per-user filtering does NOT happen here — the bus is book-scoped.
// Subscribers carry a "books I can see" set captured at subscription
// time and ignore events for books outside it. The bus broadcasts;
// the SSE handler filters.
//
// Best-effort delivery: a slow subscriber whose buffered channel
// fills up loses events for that backlog. EventSource clients
// reconnect on their own and a fresh fetch re-syncs the editor, so
// occasional drops are acceptable.
package pageevents

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Kind names the event the bus emits.
type Kind string

const (
	KindPageSaved   Kind = "page-saved"
	KindPageDeleted Kind = "page-deleted"
)

// Event is one bus emission. ID monotonically increases per Bus
// instance and is surfaced to SSE clients as the event id so they
// can resume with Last-Event-ID after a reconnect (replay isn't
// implemented yet — IDs are advisory only).
type Event struct {
	ID      uint64          `json:"id"`
	Kind    Kind            `json:"kind"`
	At      time.Time       `json:"at"`
	BookID  string          `json:"book_id"`
	PageID  string          `json:"page_id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// PageSavedPayload is the JSON body of a page-saved event. Just
// enough for the editor to decide whether to refresh: book slug +
// page slug to look up, updated_at to compare against, and the
// updater's canonical id so the surface can suppress refresh on
// its own writes ("did I just save this?").
type PageSavedPayload struct {
	BookSlug  string    `json:"book_slug"`
	PageSlug  string    `json:"page_slug"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
	// UpdatedBy is a display-ready actor label resolved server-side —
	// the user's display name for human edits, or the shard's name
	// for shard-driven writes. Falls back to the raw user_id when
	// no lookup succeeds.
	UpdatedBy string `json:"updated_by"`
	// IsShard flags shard-driven writes so the frontend can tag the
	// "Synced — …" indicator differently if it ever wants to.
	IsShard bool `json:"is_shard,omitempty"`
}

// PageDeletedPayload travels with a page-deleted event so the
// editor can drop the row from the in-memory list and close the
// note if it's currently open.
type PageDeletedPayload struct {
	BookSlug string `json:"book_slug"`
	PageSlug string `json:"page_slug"`
}

// Bus is the fanout point.
type Bus struct {
	nextID atomic.Uint64

	mu   sync.RWMutex
	subs map[*subscription]struct{}
}

type subscription struct {
	ch chan Event
}

// NewBus returns a fresh bus with no subscribers.
func NewBus() *Bus {
	return &Bus{subs: make(map[*subscription]struct{})}
}

// Subscribe registers a new listener with the supplied buffer size.
// The returned channel closes when ctx is cancelled — callers
// should always range with that cancellation in mind.
func (b *Bus) Subscribe(ctx context.Context, bufferSize int) <-chan Event {
	if bufferSize <= 0 {
		bufferSize = 32
	}
	sub := &subscription{ch: make(chan Event, bufferSize)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, sub)
		close(sub.ch)
		b.mu.Unlock()
	}()
	return sub.ch
}

// Publish fans an event out to every subscriber. Non-blocking: a
// subscriber whose channel is full simply doesn't get this event.
// JSON-marshal failures fall back to "{}" so the payload type is
// never the cause of a missed broadcast.
func (b *Bus) Publish(kind Kind, bookID, pageID string, payload any) {
	if b == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	e := Event{
		ID:      b.nextID.Add(1),
		Kind:    kind,
		At:      time.Now().UTC(),
		BookID:  bookID,
		PageID:  pageID,
		Payload: raw,
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subs {
		select {
		case sub.ch <- e:
		default:
			// Slow subscriber — drop. EventSource clients re-sync on
			// reconnect, so an occasional miss is fine.
		}
	}
}
