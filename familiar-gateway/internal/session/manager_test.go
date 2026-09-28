package session

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/classifier"
)

func TestSessionLastClassifierRoundTrip(t *testing.T) {
	s := &Session{ID: "s1"}
	if got := s.LastClassifier(); got != nil {
		t.Fatalf("expected nil before SetLastClassifier, got %+v", got)
	}
	s.SetLastClassifier(classifier.Output{
		Thinking:    classifier.ThinkingHigh,
		MemoryDepth: classifier.MemoryDeep,
		SearchDepth: classifier.SearchShallow,
	})
	got := s.LastClassifier()
	if got == nil {
		t.Fatal("expected non-nil after SetLastClassifier")
	}
	if got.Thinking != classifier.ThinkingHigh ||
		got.MemoryDepth != classifier.MemoryDeep ||
		got.SearchDepth != classifier.SearchShallow {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Returned value is a copy — mutation must not bleed into the
	// session's internal state.
	got.Thinking = classifier.ThinkingOff
	again := s.LastClassifier()
	if again.Thinking != classifier.ThinkingHigh {
		t.Error("session state leaked through returned pointer (Thinking)")
	}
}

func TestSessionRecentFactsRing(t *testing.T) {
	s := &Session{ID: "s1"}
	if got := s.RecentFacts(); len(got) != 0 {
		t.Fatalf("expected empty ring, got %v", got)
	}
	s.AddRecentFacts([]string{"a", "b"})
	s.AddRecentFacts([]string{"c"})
	if got := s.RecentFacts(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("ring = %v, want [a b c]", got)
	}

	// Overflow keeps only the newest maxRecentFacts.
	many := make([]string, maxRecentFacts+5)
	for i := range many {
		many[i] = fmt.Sprintf("f%d", i)
	}
	s.AddRecentFacts(many)
	got := s.RecentFacts()
	if len(got) != maxRecentFacts {
		t.Fatalf("ring len = %d, want %d after overflow", len(got), maxRecentFacts)
	}
	if got[len(got)-1] != many[len(many)-1] {
		t.Errorf("newest fact evicted; last = %q", got[len(got)-1])
	}

	// The returned slice is a copy — mutating it can't corrupt session state.
	got[0] = "TAMPERED"
	if s.RecentFacts()[0] == "TAMPERED" {
		t.Error("RecentFacts returned the live slice, not a copy")
	}
}

func TestSessionAddTurn(t *testing.T) {
	s := &Session{ID: "s1", Metadata: make(map[string]string)}
	before := time.Now()
	s.AddTurn("user", "hello")

	if len(s.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(s.Turns))
	}
	if s.Turns[0].Role != "user" || s.Turns[0].Content != "hello" {
		t.Fatalf("unexpected turn: %+v", s.Turns[0])
	}
	if s.LastActive.Before(before) {
		t.Fatal("LastActive was not updated")
	}
}

func TestSessionRecentTurns(t *testing.T) {
	s := &Session{ID: "s1"}
	for i := 0; i < 5; i++ {
		s.Turns = append(s.Turns, Turn{Role: "user", Content: string(rune('a' + i))})
	}

	recent := s.RecentTurns(3)
	if len(recent) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(recent))
	}
	// Should be the last 3: c, d, e
	if recent[0].Content != "c" || recent[1].Content != "d" || recent[2].Content != "e" {
		t.Fatalf("unexpected recent turns: %+v", recent)
	}
}

func TestSessionRecentTurnsAll(t *testing.T) {
	s := &Session{ID: "s1"}
	for i := 0; i < 3; i++ {
		s.Turns = append(s.Turns, Turn{Role: "user", Content: string(rune('a' + i))})
	}

	// n=0 should return all
	all := s.RecentTurns(0)
	if len(all) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(all))
	}

	// n >= len should also return all
	all2 := s.RecentTurns(10)
	if len(all2) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(all2))
	}
}

func TestSessionMetadata(t *testing.T) {
	s := &Session{ID: "s1"}

	// GetMeta on nil map returns false
	_, ok := s.GetMeta("missing")
	if ok {
		t.Fatal("expected ok=false for missing key on nil map")
	}

	s.SetMeta("key1", "val1")
	v, ok := s.GetMeta("key1")
	if !ok || v != "val1" {
		t.Fatalf("expected val1, got %q (ok=%v)", v, ok)
	}

	// Missing key on initialized map
	_, ok = s.GetMeta("nope")
	if ok {
		t.Fatal("expected ok=false for missing key")
	}
}

func TestManagerGetOrCreate(t *testing.T) {
	m := NewManager()

	s1 := m.GetOrCreate("ch1", "user1")
	s2 := m.GetOrCreate("ch1", "user1")

	if s1.ID != s2.ID {
		t.Fatalf("expected same session, got %s vs %s", s1.ID, s2.ID)
	}
}

// SummaryKey must distinguish explicit-ID sessions (per-conversation
// rolling summary, keyed by ID) from implicit (channel, sender)
// sessions (legacy stable key). The bug this guards: every workspace
// conversation sharing one Key(channel, sender) summary row.
func TestSummaryKey(t *testing.T) {
	m := NewManager()

	// Two explicit-ID sessions for the SAME (channel, sender) — the
	// workspace case: one user, two conversations. Their summary keys
	// must differ so they don't clobber each other.
	convA := m.GetOrCreateWithID("conv-a", "workspace", "alice")
	convB := m.GetOrCreateWithID("conv-b", "workspace", "alice")
	if convA.SummaryKey() != "conv-a" || convB.SummaryKey() != "conv-b" {
		t.Errorf("explicit-ID SummaryKey: got %q / %q, want conv-a / conv-b",
			convA.SummaryKey(), convB.SummaryKey())
	}
	if convA.SummaryKey() == convB.SummaryKey() {
		t.Fatal("two conversations for one user share a summary key — the bug")
	}

	// Implicit (channel, sender) session keeps the legacy stable key.
	cli := m.GetOrCreate("cli", "local")
	if got, want := cli.SummaryKey(), Key("cli", "local"); got != want {
		t.Errorf("implicit SummaryKey = %q, want %q", got, want)
	}
}

// EvictIdle removes sessions past the idle cutoff and leaves fresh and
// mid-summarization ones alone.
func TestEvictIdle(t *testing.T) {
	m := NewManager()

	stale := m.GetOrCreateWithID("stale", "workspace", "alice")
	stale.AddTurn("user", "old") // sets LastActive = now
	// Backdate it past the cutoff.
	stale.mu.Lock()
	stale.LastActive = time.Now().Add(-time.Hour)
	stale.mu.Unlock()

	fresh := m.GetOrCreateWithID("fresh", "workspace", "alice")
	fresh.AddTurn("user", "recent")

	// A stale-but-summarizing session must be spared.
	busy := m.GetOrCreateWithID("busy", "workspace", "alice")
	busy.mu.Lock()
	busy.LastActive = time.Now().Add(-time.Hour)
	busy.summarizing = true
	busy.mu.Unlock()

	n := m.EvictIdle(30 * time.Minute)
	if n != 1 {
		t.Fatalf("evicted %d, want 1 (only the stale idle session)", n)
	}
	if _, ok := m.Get("stale"); ok {
		t.Error("stale session not evicted")
	}
	if _, ok := m.Get("fresh"); !ok {
		t.Error("fresh session wrongly evicted")
	}
	if _, ok := m.Get("busy"); !ok {
		t.Error("summarizing session wrongly evicted")
	}

	// maxIdle <= 0 disables eviction.
	if got := m.EvictIdle(0); got != 0 {
		t.Errorf("EvictIdle(0) evicted %d, want 0", got)
	}
}

// GetOrCreateWithID is concurrency-safe: N goroutines racing to create
// the same explicit-ID session all get the same *Session (no
// check-then-act split that drops one side's turns).
func TestGetOrCreateWithIDConcurrent(t *testing.T) {
	m := NewManager()
	const n = 50
	got := make([]*Session, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = m.GetOrCreateWithID("same-conv", "workspace", "alice")
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if got[i] != got[0] {
			t.Fatalf("racing GetOrCreateWithID returned distinct sessions (%p vs %p)", got[0], got[i])
		}
	}
}

func TestManagerGetOrCreateDifferentSender(t *testing.T) {
	m := NewManager()

	s1 := m.GetOrCreate("ch1", "user1")
	s2 := m.GetOrCreate("ch1", "user2")

	if s1.ID == s2.ID {
		t.Fatal("expected different sessions for different senders")
	}
}

func TestManagerDelete(t *testing.T) {
	m := NewManager()

	s := m.GetOrCreate("ch1", "user1")
	id := s.ID

	m.Delete(id)

	_, ok := m.Get(id)
	if ok {
		t.Fatal("expected session to be deleted")
	}

	// GetOrCreate after delete should yield a new session
	s2 := m.GetOrCreate("ch1", "user1")
	if s2.ID == id {
		t.Fatal("expected new session after delete")
	}
}

func TestSessionConcurrency(t *testing.T) {
	s := &Session{ID: "s1", Metadata: make(map[string]string)}
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.AddTurn("user", "msg")
			s.SetMeta("k", "v")
			s.GetMeta("k")
			s.RecentTurns(5)
		}(i)
	}

	wg.Wait()

	if len(s.Turns) != MaxSessionTurns {
		t.Fatalf("expected %d turns (capped), got %d", MaxSessionTurns, len(s.Turns))
	}
}

// A session already bound to one user must not be re-homed to another.
// SetIdentity overwrote it, so a colliding id let a second user's turn
// read the first user's history and commit facts under the wrong id.
func TestClaimIdentity_RefusesAnotherUsersSession(t *testing.T) {
	m := NewManager()
	s := m.GetOrCreateWithID("conv-1", "slack:C1", "U1")
	if !s.ClaimIdentity("slack", "alice") {
		t.Fatal("claiming an unbound session failed")
	}
	if !s.ClaimIdentity("slack", "alice") {
		t.Fatal("re-claiming by the same user failed")
	}
	if s.ClaimIdentity("slack", "bob") {
		t.Fatal("bob claimed alice's session")
	}
	if got := s.UserID(); got != "alice" {
		t.Fatalf("session re-homed to %q after a refused claim", got)
	}
	if s.ClaimIdentity("slack", "") {
		t.Fatal("an empty identity claimed a session")
	}
}

// ReplaceTurnsIf swaps the buffer only if nothing was added since the
// caller looked: hydration must not drop a turn another request
// committed while the load ran.
func TestReplaceTurnsIf(t *testing.T) {
	s := NewManager().GetOrCreate("cli", "u")
	s.AddTurn("user", "live")
	if s.ReplaceTurnsIf([]Turn{{Role: "user", Content: "loaded"}}, 0) {
		t.Fatal("replaced a buffer that changed since it was counted")
	}
	if got := s.RecentTurns(0); len(got) != 1 || got[0].Content != "live" {
		t.Fatalf("buffer = %v, want the live turn", got)
	}
	if !s.ReplaceTurnsIf([]Turn{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}, 1) {
		t.Fatal("did not replace an unchanged buffer")
	}
	got := s.RecentTurns(0)
	if len(got) != 2 || got[0].Seq == 0 || got[1].Seq <= got[0].Seq {
		t.Errorf("replaced turns %v, want two with increasing Seq", got)
	}
}

// A session with a turn running is not evicted as idle: its LastActive
// only moved when a turn committed, so a long turn on a quiet session
// was evicted mid-turn.
func TestEvictIdle_SkipsSessionsWithATurnRunning(t *testing.T) {
	m := NewManager()
	s := m.GetOrCreateWithID("conv-1", "workspace", "u")
	s.BeginTurn()
	s.mu.Lock()
	s.LastActive = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	if n := m.EvictIdle(time.Minute); n != 0 {
		t.Fatalf("evicted %d sessions with a turn running", n)
	}
	s.EndTurn()
	s.mu.Lock()
	s.LastActive = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	if n := m.EvictIdle(time.Minute); n != 1 {
		t.Fatalf("evicted %d idle sessions, want 1", n)
	}
}

// Concurrent first requests for one (channel, sender) get one session;
// the look-up-then-create raced into duplicates.
func TestGetOrCreate_ConcurrentFirstRequestsShareASession(t *testing.T) {
	m := NewManager()
	var wg sync.WaitGroup
	got := make([]*Session, 50)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = m.GetOrCreate("action:1", "scheduler")
		}(i)
	}
	wg.Wait()
	for _, s := range got {
		if s != got[0] {
			t.Fatal("concurrent first requests created more than one session")
		}
	}
	if n := m.Count(); n != 1 {
		t.Errorf("manager holds %d sessions, want 1", n)
	}
}
