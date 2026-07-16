// Package memevents carries the typed event stream emitted by the
// post-turn write pipeline and compaction. CHAT-REARCH §"Typed Event
// Stream" — same stream is consumed by frontend UX (animated stages)
// and observability/debug logs.
//
// The package itself is transport-agnostic. internal/adapter/native
// owns the SSE endpoint that serves these events to a browser.
package memevents

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Kind identifies an event type. Values are stable strings — they
// appear in the SSE stream's "event:" field and in JSON payloads
// frontends key off of.
type Kind string

const (
	KindCompactionStarted   Kind = "CompactionStarted"
	KindSummaryGenerated    Kind = "SummaryGenerated"
	KindFactExtracted       Kind = "FactExtracted"
	KindConflictResolved    Kind = "ConflictResolved"
	KindRelationshipAdded   Kind = "RelationshipAdded"
	KindRelationshipUpdated Kind = "RelationshipUpdated"
	KindCompactionCompleted Kind = "CompactionCompleted"
	KindCompactionFailed    Kind = "CompactionFailed"
)

// Event is one entry in the stream. ID is monotonic per process —
// the SSE handler uses it for Last-Event-ID replay.
type Event struct {
	ID        uint64          `json:"id"`
	SessionID string          `json:"session_id"`
	Kind      Kind            `json:"kind"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload"`
}

// CompactionStartedPayload — runSummarize is about to fold turns.
type CompactionStartedPayload struct {
	TurnCount     int `json:"turn_count"`      // turns being folded this pass
	OldestTurnAge int `json:"oldest_turn_age"` // turns since the oldest folded turn
}

// SummaryGeneratedPayload — sidecar produced a fresh rolling summary.
type SummaryGeneratedPayload struct {
	TokensIn       int    `json:"tokens_in"`
	TokensOut      int    `json:"tokens_out"`
	SummaryPreview string `json:"summary_preview"` // first ~200 chars
}

// FactExtractedPayload — one fact has been pulled from the turn pair
// and committed as a memory.
type FactExtractedPayload struct {
	FactID   string `json:"fact_id"`
	Content  string `json:"content"`
	Category string `json:"category"`
}

// ConflictResolvedPayload — the medium slot decided what to do with
// an extracted candidate. Action ∈ {ADD, UPDATE, DUPLICATE}.
type ConflictResolvedPayload struct {
	Action      string `json:"action"`
	FactID      string `json:"fact_id,omitempty"`   // new fact id (ADD/UPDATE)
	TargetID    string `json:"target_id,omitempty"` // superseded id (UPDATE)
	FactPreview string `json:"fact_preview"`
}

// RelationshipAddedPayload — a fresh edge has been upserted.
type RelationshipAddedPayload struct {
	From      string `json:"from"`
	Predicate string `json:"predicate"`
	To        string `json:"to"`
}

// RelationshipUpdatedPayload — an existing edge's value changed.
// Reserved for the relationship versioning work; emitted only when
// the upsert path can identify a prior value.
type RelationshipUpdatedPayload struct {
	From       string `json:"from"`
	Predicate  string `json:"predicate"`
	To         string `json:"to"`
	PriorValue string `json:"prior_value"`
}

// CompactionCompletedPayload — terminal event for a successful pass.
type CompactionCompletedPayload struct {
	DurationMs        int `json:"duration_ms"`
	FactsAdded        int `json:"facts_added"`
	ConflictsResolved int `json:"conflicts_resolved"`
}

// CompactionFailedPayload — terminal event for a failed pass.
// Stage names match the runtime: "summarize", "extract", "batch",
// "commit". Deferred=true means the next turn will retry.
type CompactionFailedPayload struct {
	Stage    string `json:"stage"`
	Reason   string `json:"reason"`
	Deferred bool   `json:"deferred"`
}

// Bus is the in-process fanout point. Emit() is non-blocking and
// safe for concurrent callers; subscribers each get their own
// buffered channel and are dropped if they fall behind by more than
// the channel buffer.
//
// Cap is the per-subscriber buffer size. The bus itself has no
// global cap — backpressure lives at the subscriber boundary.
type Bus struct {
	nextID atomic.Uint64

	mu   sync.RWMutex
	subs map[*subscription]struct{}

	logSink func(Event) // optional — the log emitter writes here

	// Per-session ring buffer for replay-on-reconnect. Sized by
	// ringSize; older events fall off as new ones land.
	ringSize int
	rings    map[string]*sessionRing
}

type subscription struct {
	sessionID string // "" means subscribe to all sessions
	ch        chan Event
}

type sessionRing struct {
	mu     sync.Mutex
	events []Event
	cap    int
}

func (r *sessionRing) append(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	if len(r.events) > r.cap {
		drop := len(r.events) - r.cap
		r.events = append(r.events[:0], r.events[drop:]...)
	}
}

// Snapshot returns events with id > sinceID, in order. Used by the
// SSE handler to replay anything missed during a Last-Event-ID
// reconnect window.
func (r *sessionRing) Snapshot(sinceID uint64) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, 0, len(r.events))
	for _, e := range r.events {
		if e.ID > sinceID {
			out = append(out, e)
		}
	}
	return out
}

// NewBus constructs a bus. ringSize=0 disables per-session replay
// (subscribers only see events emitted after they subscribe).
func NewBus(ringSize int, logSink func(Event)) *Bus {
	return &Bus{
		subs:     make(map[*subscription]struct{}),
		logSink:  logSink,
		ringSize: ringSize,
		rings:    make(map[string]*sessionRing),
	}
}

// Emit publishes an event. Payload is JSON-marshaled here so the
// caller can pass typed structs and the bus has the source of truth
// for wire format. Failures are logged via logSink and otherwise
// silently dropped — events are best-effort.
func (b *Bus) Emit(sessionID string, kind Kind, payload any) {
	if b == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	e := Event{
		ID:        b.nextID.Add(1),
		SessionID: sessionID,
		Kind:      kind,
		At:        time.Now().UTC(),
		Payload:   raw,
	}

	if b.logSink != nil {
		b.logSink(e)
	}
	if b.ringSize > 0 && sessionID != "" {
		b.mu.Lock()
		ring, ok := b.rings[sessionID]
		if !ok {
			ring = &sessionRing{cap: b.ringSize}
			b.rings[sessionID] = ring
		}
		b.mu.Unlock()
		ring.append(e)
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subs {
		if sub.sessionID != "" && sub.sessionID != sessionID {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			// Subscriber buffer full — drop the event for this
			// subscriber. Logging is intentional only at the bus
			// level (logSink) so a slow SSE client doesn't spam
			// the gateway log on every emit.
		}
	}
}

// Subscribe registers a subscriber for the given session ID. Pass
// "" to receive every session's events. The returned channel is
// closed when ctx is cancelled or the bus is closed; callers should
// always range with the cancellation.
//
// bufferSize sets the per-subscriber backpressure window — events
// past the buffer are dropped for this subscriber. A reasonable
// default is 64; high-volume sessions may need more.
func (b *Bus) Subscribe(ctx context.Context, sessionID string, bufferSize int) <-chan Event {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	sub := &subscription{
		sessionID: sessionID,
		ch:        make(chan Event, bufferSize),
	}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, sub)
		b.mu.Unlock()
		close(sub.ch)
	}()

	return sub.ch
}

// Replay returns events for the given session whose ID is greater
// than sinceID. Used by the SSE handler when a client reconnects
// with Last-Event-ID set. Returns nil when the bus has no ring
// buffer or no events for this session.
func (b *Bus) Replay(sessionID string, sinceID uint64) []Event {
	if b == nil || b.ringSize <= 0 || sessionID == "" {
		return nil
	}
	b.mu.RLock()
	ring, ok := b.rings[sessionID]
	b.mu.RUnlock()
	if !ok {
		return nil
	}
	return ring.Snapshot(sinceID)
}
