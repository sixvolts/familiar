package session

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/google/uuid"
)

// Turn represents one message in a conversation. Role is "user",
// "assistant", or "tool". An assistant turn that invoked tools
// carries ToolCalls (provider-shaped JSON, same as llm.ToolCall
// marshalled); the tool turns that follow each carry the matching
// ToolCallID so the LLM provider can stitch them back together on
// replay. See pipeline.flattenAssembled for the read side.
//
// Persisting these so the agentic loop's intermediate work
// (tool plan + tool results) survives the gap between turns —
// without that history, the model has to rediscover identifiers
// it already figured out (see SESSION-HYDRATION.md and the
// "lost context across turns" issue this turn-shape extension
// fixes).
type Turn struct {
	Role       string
	Content    string
	Timestamp  time.Time
	ToolCalls  json.RawMessage // assistant turns that called tools
	ToolCallID string          // tool turns, answering a prior call
}

// Session holds the in-memory state of one conversation.
type Session struct {
	ID        string
	ChannelID string
	SenderID  string

	// platform and canonicalID are the mutable identity fields. They
	// were exported but are now guarded by mu and reached through
	// accessors (Platform()/CanonicalID()/SetPlatform/SetCanonicalID/
	// SetIdentity): a turn's resolveIdentity writes canonicalID while
	// a prior turn's post-extract goroutine reads it via UserID(), so
	// unguarded access was a data race (EXTERNAL-READINESS-REVIEW.md).
	//
	// platform names the adapter that created the session ("slack",
	// "workspace", "cli", "scheduler"); canonicalID is the resolved
	// canonical user identity (empty until the resolver maps it, with
	// SenderID as the fallback — see UserID()).
	platform    string
	canonicalID string

	CreatedAt       time.Time
	LastActive      time.Time
	Turns           []Turn
	Metadata        map[string]string
	RollingSummary  string // lossy narrative of turns that have been summarized away
	SummarizedCount int    // number of turns that have been folded into RollingSummary
	summarizing     bool   // guard: prevents concurrent summarization goroutines for this session
	hydrated        bool   // set once the running_summary has been loaded from the sessions store
	// explicitID is true when this session was created via
	// GetOrCreateWithID — i.e. its ID is a stable, externally-
	// meaningful identifier (a workspace conversation UUID, a Slack
	// per-conversation hash) rather than a fresh per-process UUID.
	// It governs SummaryKey: explicit-ID sessions persist their
	// rolling summary under their own ID (so two of one user's
	// conversations don't share a summary row), while implicit
	// (channel, sender) sessions keep the legacy stable key.
	explicitID bool

	// lastClassifier is the classifier output from the most recent
	// turn. CHAT-REARCH §"Smaller Hardening" — kept for debug
	// surfaces and future heuristics (rapid-follow-up detection,
	// effort-trend tracking). Read with LastClassifier(); written
	// by the pipeline via SetLastClassifier(). Nil before the first
	// classify call.
	lastClassifier *classifier.Output

	mu sync.Mutex
}

// SummaryKey returns the persistence key for this session's rolling
// summary in the sessions store. For explicit-ID sessions (workspace
// conversation UUID, Slack per-conversation hash) it's the session
// ID, so each conversation gets its own summary row. For implicit
// (channel, sender) sessions (CLI, scheduler) it's the legacy
// Key(channel, sender), which is stable across process restarts for
// those single-session-per-sender adapters.
//
// This must match how the manager keys the session, otherwise save
// and load disagree: before this existed, every workspace
// conversation shared one Key(channel, sender) summary row and they
// clobbered each other (EXTERNAL-READINESS-REVIEW.md P1).
func (s *Session) SummaryKey() string {
	if s == nil {
		return ""
	}
	if s.explicitID {
		return s.ID
	}
	return Key(s.ChannelID, s.SenderID)
}

// LastClassifier returns a deep copy of the most recent classifier
// output for this session, or nil if the session hasn't been
// classified yet. Tools is copied so caller mutation can't bleed into
// session state.
func (s *Session) LastClassifier() *classifier.Output {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastClassifier == nil {
		return nil
	}
	cp := *s.lastClassifier
	if len(s.lastClassifier.Tools) > 0 {
		cp.Tools = append([]string(nil), s.lastClassifier.Tools...)
	}
	return &cp
}

// SetLastClassifier stamps the classifier output from the current
// turn onto the session. Pipeline calls this immediately after
// classifyRequest returns.
func (s *Session) SetLastClassifier(out classifier.Output) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastClassifier = &out
}

// UserID returns the canonical user identity when one has been resolved,
// otherwise the raw platform SenderID. Callers that need to key profile
// storage, memory scope, or fact attribution should use this instead of
// reading SenderID directly.
func (s *Session) UserID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.canonicalID != "" {
		return s.canonicalID
	}
	return s.SenderID
}

// CanonicalID returns the resolved canonical identity (empty until the
// resolver maps it). Guarded read.
func (s *Session) CanonicalID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canonicalID
}

// Platform returns the adapter that created this session. Guarded read.
func (s *Session) Platform() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.platform
}

// SetPlatform records the creating adapter. Adapters call this once at
// session setup.
func (s *Session) SetPlatform(platform string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.platform = platform
}

// SetCanonicalID records the resolved canonical identity. The pipeline
// calls this from resolveIdentity (idempotent after the first resolve).
func (s *Session) SetCanonicalID(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canonicalID = id
}

// SetIdentity sets platform + canonicalID together — the convenience an
// adapter uses when it already knows both at session setup (a cookie
// canonical id, a shard owner id).
func (s *Session) SetIdentity(platform, canonicalID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.platform = platform
	s.canonicalID = canonicalID
}

// IsHydrated reports whether the persistent running_summary has already
// been loaded into this session in the current process.
func (s *Session) IsHydrated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hydrated
}

// MarkHydrated records that the persistent running_summary has been
// merged in, preventing repeat DB round-trips on subsequent turns.
func (s *Session) MarkHydrated() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hydrated = true
}

// SummarizedCountSnapshot returns the number of turns that have been
// folded into the rolling summary so far, under the session lock.
func (s *Session) SummarizedCountSnapshot() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.SummarizedCount
}

// SetSummary overwrites the rolling summary and summarized turn count
// without dropping any live turns. Used by the hydration path on first
// use after a gateway restart.
func (s *Session) SetSummary(summary string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RollingSummary = summary
	s.SummarizedCount = count
}

// MaxSessionTurns caps the in-memory turn buffer. The cap is a
// memory bound — token-budget eviction inside ctxbuild does the
// real work of deciding what fits in any given LLM call. Bumped
// from 40 (≈20 exchanges) to 100 because each tool-using exchange
// now stores its full message sequence (user → assistant w/
// tool_calls → tool result → … → assistant final), so a single
// exchange can be 5–10 turns instead of 2.
const MaxSessionTurns = 100

// AddMessage appends one message — user, assistant (with or
// without tool_calls), or tool — to the session and updates
// LastActive. The oldest are evicted when the buffer exceeds
// MaxSessionTurns. Callers that just want the (role, content)
// shorthand should use AddTurn.
func (s *Session) AddMessage(t Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.Timestamp.IsZero() {
		t.Timestamp = time.Now()
	}
	s.LastActive = t.Timestamp
	s.Turns = append(s.Turns, t)
	if len(s.Turns) > MaxSessionTurns {
		s.Turns = s.Turns[len(s.Turns)-MaxSessionTurns:]
	}
}

// AddTurn is the shorthand for the plain (role, content) case
// where no tool_calls or tool_call_id apply.
func (s *Session) AddTurn(role, content string) {
	s.AddMessage(Turn{Role: role, Content: content})
}

// RecentTurns returns the most recent n turns (or all if n >= len).
// TurnCount returns the number of buffered turns under the lock.
// Callers must use this instead of len(sess.Turns): AddMessage mutates
// Turns concurrently (maxInflightPerUser permits several requests on
// one session), so an unlocked len() is a data race.
func (s *Session) TurnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Turns)
}

func (s *Session) RecentTurns(n int) []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n >= len(s.Turns) {
		out := make([]Turn, len(s.Turns))
		copy(out, s.Turns)
		return out
	}
	out := make([]Turn, n)
	copy(out, s.Turns[len(s.Turns)-n:])
	return out
}

// TryBeginSummarize returns true if the caller acquired the summarization
// lock for this session. Prevents concurrent summarization goroutines from
// stepping on each other. Call EndSummarize when done.
func (s *Session) TryBeginSummarize() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.summarizing {
		return false
	}
	s.summarizing = true
	return true
}

// EndSummarize releases the summarization lock.
func (s *Session) EndSummarize() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summarizing = false
}

// CompactSummary updates the rolling summary and trims the oldest `dropCount`
// turns from the session (those have been folded into the summary).
// Returns the turns that were dropped so the caller can use them for
// fact extraction.
func (s *Session) CompactSummary(newSummary string, dropCount int) []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dropCount <= 0 || dropCount > len(s.Turns) {
		return nil
	}
	dropped := make([]Turn, dropCount)
	copy(dropped, s.Turns[:dropCount])
	s.Turns = append([]Turn{}, s.Turns[dropCount:]...)
	s.RollingSummary = newSummary
	s.SummarizedCount += dropCount
	s.LastActive = time.Now()
	return dropped
}

// Snapshot returns a read-only view of the session's summary and turn count
// without copying Turns. Used by the summarization trigger check.
func (s *Session) Snapshot() (summary string, turnCount int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.RollingSummary, len(s.Turns)
}

// SnapshotForSummarize atomically grabs the previous summary and a copy of
// the first `n` turns for folding into the rolling summary. Returns fewer
// turns if the session has fewer available. The session is NOT mutated —
// callers should follow up with CompactSummary once the sidecar returns.
func (s *Session) SnapshotForSummarize(n int) (prevSummary string, turns []Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.Turns) {
		n = len(s.Turns)
	}
	out := make([]Turn, n)
	copy(out, s.Turns[:n])
	return s.RollingSummary, out
}

// SetMeta stores a metadata key on the session.
func (s *Session) SetMeta(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Metadata == nil {
		s.Metadata = make(map[string]string)
	}
	s.Metadata[key] = value
}

// GetMeta retrieves a metadata value.
func (s *Session) GetMeta(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Metadata == nil {
		return "", false
	}
	v, ok := s.Metadata[key]
	return v, ok
}

// Manager manages active sessions.
type Manager struct {
	sessions sync.Map // map[string]*Session
}

// NewManager creates a new session manager.
func NewManager() *Manager {
	return &Manager{}
}

// Get retrieves a session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	v, ok := m.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Session), true
}

// GetOrCreate retrieves an existing session for (channelID, senderID)
// or creates a new one with a fresh UUID. Used by adapters that
// don't have an externally-meaningful session identity to hand back
// — every message from the same (channel, sender) shares one session.
func (m *Manager) GetOrCreate(channelID, senderID string) *Session {
	return m.getOrCreate(channelID, senderID, "")
}

// GetOrCreateWithID is the explicit-ID path. When id is non-empty it
// becomes the authoritative key: the manager looks the session up by
// ID, and a miss creates a fresh session stamped with that exact ID.
// (channelID, senderID) are recorded but NOT used for lookup — that
// keeps multiple per-conversation sessions for the same user/channel
// pair distinct, which is what the workspace adapter relies on when
// it passes conversation_id as the session id (see
// SESSION-HYDRATION.md). Adapters that derive a stable platform-
// specific id (Slack sha256, workspace conversation UUID) use this
// path so the same identity persists across process restarts.
func (m *Manager) GetOrCreateWithID(id, channelID, senderID string) *Session {
	return m.getOrCreate(channelID, senderID, id)
}

func (m *Manager) getOrCreate(channelID, senderID, explicitID string) *Session {
	// Explicit-ID path: look up by ID. If found return it; otherwise
	// mint a new session stamped with that ID. (channel, sender) is
	// not used for lookup here so a single user can have N sessions
	// keyed by N distinct ids (one per conversation).
	//
	// LoadOrStore (not Load-then-Store) so two concurrent first
	// messages for the same conversation can't each mint a distinct
	// *Session and silently drop one side's turns. The race-loser's
	// freshly-built struct is discarded.
	if explicitID != "" {
		now := time.Now()
		fresh := &Session{
			ID:         explicitID,
			ChannelID:  channelID,
			SenderID:   senderID,
			CreatedAt:  now,
			LastActive: now,
			Metadata:   make(map[string]string),
			explicitID: true,
		}
		actual, _ := m.sessions.LoadOrStore(explicitID, fresh)
		return actual.(*Session)
	}

	// Implicit-ID path: scan by (channel, sender). Adapters using
	// this path get one session per (channel, sender) — the legacy
	// behavior.
	var existing *Session
	m.sessions.Range(func(_, v interface{}) bool {
		s := v.(*Session)
		if s.ChannelID == channelID && s.SenderID == senderID {
			existing = s
			return false
		}
		return true
	})
	if existing != nil {
		return existing
	}

	now := time.Now()
	sess := &Session{
		ID:         uuid.NewString(),
		ChannelID:  channelID,
		SenderID:   senderID,
		CreatedAt:  now,
		LastActive: now,
		Metadata:   make(map[string]string),
	}
	m.sessions.Store(sess.ID, sess)
	return sess
}

// EvictIdle removes every session with no activity for at least
// maxIdle and returns the count removed. Eviction only frees memory:
// the rolling summary is already persisted (summarize.go) and the
// verbatim turns rehydrate from the messages table on the next
// request (SESSION-HYDRATION.md), so a user who returns after an
// eviction transparently picks up where they left off.
//
// Sessions mid-summarization are skipped so eviction never races a
// compaction goroutine. An in-flight turn won't be idle (its
// LastActive was just bumped), and even if a turn races the sweep,
// it keeps working on its own *Session pointer — the next request
// just rehydrates a fresh one.
func (m *Manager) EvictIdle(maxIdle time.Duration) int {
	if maxIdle <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-maxIdle)
	evicted := 0
	m.sessions.Range(func(k, v interface{}) bool {
		s := v.(*Session)
		s.mu.Lock()
		idle := s.LastActive.Before(cutoff)
		busy := s.summarizing
		s.mu.Unlock()
		if idle && !busy {
			m.sessions.Delete(k)
			evicted++
		}
		return true
	})
	return evicted
}

// StartEviction runs EvictIdle every interval until ctx is cancelled.
// No-op when either duration is non-positive (eviction disabled).
func (m *Manager) StartEviction(ctx context.Context, interval, maxIdle time.Duration) {
	if interval <= 0 || maxIdle <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := m.EvictIdle(maxIdle); n > 0 {
					log.Printf("[session] evicted %d idle sessions (idle > %s)", n, maxIdle)
				}
			}
		}
	}()
}

// Count returns the number of currently-tracked sessions.
func (m *Manager) Count() int {
	n := 0
	m.sessions.Range(func(_, _ interface{}) bool {
		n++
		return true
	})
	return n
}

// List returns a snapshot of every session currently tracked in the
// manager. Order is not guaranteed. Callers that need deterministic
// ordering should sort the result themselves — the admin console
// sessions panel sorts by LastActive desc, for instance.
//
// The returned slice is a fresh allocation but the *Session pointers
// are shared with the manager. Callers must not mutate session state
// directly; they may read fields under the session's own mutex via
// the accessor methods (Snapshot, RecentTurns, etc.).
func (m *Manager) List() []*Session {
	out := make([]*Session, 0)
	m.sessions.Range(func(_, v interface{}) bool {
		if s, ok := v.(*Session); ok && s != nil {
			out = append(out, s)
		}
		return true
	})
	return out
}

// Delete removes a session from the manager.
func (m *Manager) Delete(id string) {
	m.sessions.Range(func(k, v interface{}) bool {
		s := v.(*Session)
		if s.ID == id {
			m.sessions.Delete(k)
			return false
		}
		return true
	})
}
