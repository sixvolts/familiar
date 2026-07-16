// Deterministic fan-out tests for spawn_research_workers
// (RESEARCH-SKILL-SPEC §8): a mock InvokeFunc captures every worker's
// (prompt, envelope) and a mock Backend records page traffic in
// memory. No DB, no network, no pipeline — synchronization is all
// channels (started/release handshakes + an append signal), never
// time.Sleep, so the suite can't flake on scheduler timing.
package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/skills"
)

const testUser = "user-operator"

// waitTimeout is deliberately generous: every wait in this file is on
// a channel that a mock closes/sends deterministically, so the timeout
// only ever fires on an actual bug.
const waitTimeout = 10 * time.Second

// ──────────────────────────────────────────────────────────────────
// Mock backend
// ──────────────────────────────────────────────────────────────────

type mockBackend struct {
	mu      sync.Mutex
	books   map[string]*admin.Book       // slug → book
	members map[string]map[string]string // bookID → userID → role
	pages   map[string]*admin.WikiPage   // bookID+"/"+slug → page
	byID    map[string]*admin.WikiPage   // pageID → page
	appends []string

	// appendCh gets every AppendPage text — tests wait on it for the
	// worker-failure and run-completion lines instead of polling.
	appendCh chan string
	// updateCh gets every UpdatePage content — the compose tests wait
	// on it for the writer's note landing on the stub.
	updateCh chan string

	nextPage int
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		books:    make(map[string]*admin.Book),
		members:  make(map[string]map[string]string),
		pages:    make(map[string]*admin.WikiPage),
		byID:     make(map[string]*admin.WikiPage),
		appendCh: make(chan string, 64),
		updateCh: make(chan string, 16),
	}
}

func (m *mockBackend) seedPage(bookID, slug, id string) *admin.WikiPage {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := &admin.WikiPage{ID: id, BookID: bookID, Slug: slug, Title: slug}
	m.pages[bookID+"/"+slug] = p
	m.byID[id] = p
	return p
}

// researchSlugFor mirrors admin.researchSlug — the deterministic
// per-user research-book slug the store mints.
func researchSlugFor(userID string) string { return "research:" + userID }

// EnsureResearchBook mints the caller's per-user research book
// idempotently, always with the caller as owner — the real store's
// deterministic-slug guarantee, so no collision/membership/fork logic
// to model.
func (m *mockBackend) EnsureResearchBook(_ context.Context, userID string) (*admin.Book, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slug := researchSlugFor(userID)
	if b, ok := m.books[slug]; ok {
		return b, nil
	}
	b := &admin.Book{ID: "book-" + slug, Slug: slug, Name: "Research"}
	m.books[slug] = b
	m.members[b.ID] = map[string]string{userID: "owner"}
	return b, nil
}

// book returns a seeded/created book by slug for test assertions.
func (m *mockBackend) book(slug string) *admin.Book {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.books[slug]
}

func (m *mockBackend) GetPage(_ context.Context, bookID, pageSlug string) (*admin.WikiPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[bookID+"/"+pageSlug]
	if !ok {
		return nil, admin.ErrPageNotFound
	}
	return p, nil
}

func (m *mockBackend) CreatePage(_ context.Context, bookID, userID, title, content, _ string) (*admin.WikiPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextPage++
	p := &admin.WikiPage{
		ID:        fmt.Sprintf("page-%d", m.nextPage),
		BookID:    bookID,
		Slug:      fmt.Sprintf("evidence-%d", m.nextPage),
		Title:     title,
		Content:   content,
		UpdatedAt: time.Unix(int64(m.nextPage), 0),
	}
	m.pages[bookID+"/"+p.Slug] = p
	m.byID[p.ID] = p
	return p, nil
}

// touchPage simulates a user edit landing between stub creation and
// writer completion: content changes and updated_at moves, so a
// writer UpdatePage carrying the stale IfMatch must get ErrPageStale.
func (m *mockBackend) touchPage(bookID, slug, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pages[bookID+"/"+slug]
	p.Content = content
	p.UpdatedAt = p.UpdatedAt.Add(time.Minute)
}

func (m *mockBackend) AppendPage(_ context.Context, bookID, pageID, _ string, text string) (*admin.WikiPage, error) {
	m.mu.Lock()
	p, ok := m.byID[pageID]
	if !ok || p.BookID != bookID {
		m.mu.Unlock()
		return nil, admin.ErrPageNotFound
	}
	p.Content += "\n\n" + text
	m.appends = append(m.appends, text)
	m.mu.Unlock()
	m.appendCh <- text
	return p, nil
}

func (m *mockBackend) appendsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.appends...)
}

func (m *mockBackend) EnsurePersonalBook(_ context.Context, userID string) (*admin.Book, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slug := "personal:" + userID
	if b, ok := m.books[slug]; ok {
		return b, nil
	}
	b := &admin.Book{ID: "book-" + slug, Slug: slug, Name: "Personal"}
	m.books[slug] = b
	m.members[b.ID] = map[string]string{userID: "owner"}
	return b, nil
}

func (m *mockBackend) UpdatePage(_ context.Context, bookID, pageSlug, _ string, p admin.PagePatch) (*admin.WikiPage, error) {
	m.mu.Lock()
	pg, ok := m.pages[bookID+"/"+pageSlug]
	if !ok {
		m.mu.Unlock()
		return nil, admin.ErrPageNotFound
	}
	// CAS on updated_at, mirroring the real store: a stale IfMatch is
	// refused (the real store would try a diff3 merge first for
	// content-only patches; the mock's hard refusal exercises the
	// caller's conflict fallback).
	if p.IfMatch != nil && !p.IfMatch.Equal(pg.UpdatedAt) {
		m.mu.Unlock()
		return nil, admin.ErrPageStale
	}
	if p.Content != nil {
		pg.Content = *p.Content
		pg.UpdatedAt = pg.UpdatedAt.Add(time.Second)
	}
	content := pg.Content
	m.mu.Unlock()
	m.updateCh <- content
	return pg, nil
}

// ──────────────────────────────────────────────────────────────────
// Mock invoke
// ──────────────────────────────────────────────────────────────────

type capturedCall struct {
	prompt    string
	overrides *pipeline.ShardOverrides
}

type mockInvoke struct {
	mu    sync.Mutex
	calls []capturedCall

	inFlight    atomic.Int32
	maxInFlight atomic.Int32

	// started (when non-nil) gets one send per call at entry; release
	// (when non-nil) must yield one token per call before it returns.
	// Together they let tests hold workers mid-flight and observe the
	// semaphore without sleeping.
	started chan struct{}
	release chan struct{}

	// failFor returns a non-nil error for prompts that should fail.
	failFor func(prompt string) error
}

func (m *mockInvoke) fn(ctx context.Context, _ *session.Session, prompt string, ov *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
	cur := m.inFlight.Add(1)
	defer m.inFlight.Add(-1)
	for {
		max := m.maxInFlight.Load()
		if cur <= max || m.maxInFlight.CompareAndSwap(max, cur) {
			break
		}
	}

	m.mu.Lock()
	m.calls = append(m.calls, capturedCall{prompt: prompt, overrides: ov})
	m.mu.Unlock()

	if m.started != nil {
		m.started <- struct{}{}
	}
	if m.release != nil {
		select {
		case <-m.release:
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
	if m.failFor != nil {
		if err := m.failFor(prompt); err != nil {
			return "", nil, err
		}
	}
	return "DONE — findings appended", nil, nil
}

func (m *mockInvoke) callsSnapshot() []capturedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]capturedCall(nil), m.calls...)
}

// ──────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────

func newSkill(t *testing.T, inv *mockInvoke, be *mockBackend, opts Options) *Skill {
	t.Helper()
	opts.Invoke = inv.fn
	opts.Sessions = session.NewManager()
	opts.Backend = be
	return New(opts)
}

func userCtx() context.Context {
	return skills.WithContext(context.Background(), skills.SessionContext{UserID: testUser})
}

func execute(t *testing.T, s *Skill, ctx context.Context, args string) skills.ToolResult {
	t.Helper()
	return executeTool(t, s, ctx, toolName, args)
}

func executeTool(t *testing.T, s *Skill, ctx context.Context, tool, args string) skills.ToolResult {
	t.Helper()
	res, err := s.Execute(ctx, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute returned transport error: %v", err)
	}
	return res
}

// waitForAppend drains the backend's append signal until a text
// containing substr shows up (or the deadline hits).
func waitForAppend(t *testing.T, be *mockBackend, substr string) string {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		select {
		case text := <-be.appendCh:
			if strings.Contains(text, substr) {
				return text
			}
		case <-deadline:
			t.Fatalf("timed out waiting for an append containing %q; appends so far: %q", substr, be.appendsSnapshot())
		}
	}
}

func recvN(t *testing.T, ch <-chan struct{}, n int, what string) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(waitTimeout):
			t.Fatalf("timed out waiting for %s (%d/%d)", what, i, n)
		}
	}
}

func twoTaskArgs() string {
	return `{"topic":"local first software","tasks":[
		{"question":"What is CRDT convergence?","hints":"prefer academic sources"},
		{"question":"How does diff3 merge work?"}
	]}`
}

// ──────────────────────────────────────────────────────────────────
// Tests
// ──────────────────────────────────────────────────────────────────

// Every worker envelope must match RESEARCH-SKILL-SPEC §6.3 exactly —
// the envelope IS the security boundary, so this is the test that
// keeps a refactor from quietly widening it.
func TestSpawn_WorkerEnvelope(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s := newSkill(t, inv, be, Options{MaxWorkers: 2, WorkerSearchBudget: 7, WorkerTier: "tier2"})

	res := execute(t, s, userCtx(), twoTaskArgs())
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	waitForAppend(t, be, "complete: 2/2 workers succeeded")

	calls := inv.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("want 2 worker invocations, got %d", len(calls))
	}

	book := be.book(researchSlugFor(testUser))
	if book == nil {
		t.Fatalf("per-user research book %q was not created", researchSlugFor(testUser))
	}

	wantTools := append([]string(nil), workerAllowlist...)
	sort.Strings(wantTools)
	for i, c := range calls {
		ov := c.overrides
		if ov == nil {
			t.Fatalf("call %d: nil overrides — worker ran on the trusted path", i)
		}
		gotTools := append([]string(nil), ov.ToolAllowlist...)
		sort.Strings(gotTools)
		if strings.Join(gotTools, ",") != strings.Join(wantTools, ",") {
			t.Errorf("call %d: allowlist = %v, want %v (any order)", i, ov.ToolAllowlist, wantTools)
		}
		if len(ov.BookAccess) != 1 || ov.BookAccess[0] != book.ID {
			t.Errorf("call %d: BookAccess = %v, want [%s]", i, ov.BookAccess, book.ID)
		}
		if ov.SearchBudget != 7 {
			t.Errorf("call %d: SearchBudget = %d, want 7", i, ov.SearchBudget)
		}
		if ov.TierHint != "tier2" {
			t.Errorf("call %d: TierHint = %q, want tier2", i, ov.TierHint)
		}
		if ov.MaxTokens != workerMaxTokens {
			t.Errorf("call %d: MaxTokens = %d, want %d", i, ov.MaxTokens, workerMaxTokens)
		}
		if !ov.SkipMemoryRetrieval || !ov.SkipSessionHydration || !ov.SkipCommit {
			t.Errorf("call %d: Skip flags = (%t,%t,%t), want all true",
				i, ov.SkipMemoryRetrieval, ov.SkipSessionHydration, ov.SkipCommit)
		}
		if !ov.ExcludeFromHot {
			t.Errorf("call %d: ExcludeFromHot = false, want true", i)
		}
		if strings.TrimSpace(ov.SystemPrompt) == "" {
			t.Errorf("call %d: SystemPrompt is empty", i)
		}
		if !strings.Contains(ov.SystemPrompt, "7 searches") {
			t.Errorf("call %d: SystemPrompt does not mention the 7-search budget", i)
		}
		if ov.ShardID != "research-worker" {
			t.Errorf("call %d: ShardID = %q, want research-worker", i, ov.ShardID)
		}
		if !strings.Contains(c.prompt, "page_slug=") || !strings.Contains(c.prompt, "book_slug=") {
			t.Errorf("call %d: task prompt lacks the append target: %q", i, c.prompt)
		}
	}

	// Both questions ran; the one with hints carried them.
	joined := calls[0].prompt + "\n" + calls[1].prompt
	for _, q := range []string{"What is CRDT convergence?", "How does diff3 merge work?", "prefer academic sources"} {
		if !strings.Contains(joined, q) {
			t.Errorf("no worker prompt carries %q", q)
		}
	}
}

// 5 tasks through a 2-slot semaphore: all 5 run eventually, never more
// than 2 in flight. Workers block inside the mock until the test hands
// out release tokens, so the concurrency ceiling is actually exercised
// (a broken semaphore would put all 5 in flight and trip maxInFlight).
func TestSpawn_FanOutHonorsSemaphore(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{
		started: make(chan struct{}, 8),
		release: make(chan struct{}, 8),
	}
	s := newSkill(t, inv, be, Options{MaxWorkers: 2})

	res := execute(t, s, userCtx(), `{"topic":"t","tasks":[
		{"question":"q1"},{"question":"q2"},{"question":"q3"},{"question":"q4"},{"question":"q5"}
	]}`)
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}

	// Exactly two workers make it into invoke while the pool is held.
	recvN(t, inv.started, 2, "first two workers to start")
	if n := inv.inFlight.Load(); n != 2 {
		t.Fatalf("in-flight after 2 starts = %d, want 2", n)
	}

	// Release everyone; the remaining three flow through the semaphore.
	for i := 0; i < 5; i++ {
		inv.release <- struct{}{}
	}
	recvN(t, inv.started, 3, "remaining workers to start")
	waitForAppend(t, be, "complete: 5/5 workers succeeded")

	if got := len(inv.callsSnapshot()); got != 5 {
		t.Errorf("worker invocations = %d, want 5", got)
	}
	if max := inv.maxInFlight.Load(); max > 2 {
		t.Errorf("max concurrent workers = %d, want ≤ 2", max)
	}
}

// Execute must return while workers are still running — the tool
// call's 30s cap can never be allowed to wait on multi-minute workers.
func TestSpawn_ReturnsBeforeWorkersFinish(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{
		started: make(chan struct{}, 4),
		release: make(chan struct{}, 4),
	}
	s := newSkill(t, inv, be, Options{MaxWorkers: 2})

	// Both workers will block on release; Execute returning at all is
	// the assertion.
	res := execute(t, s, userCtx(), twoTaskArgs())
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "2 workers dispatched") {
		t.Errorf("result content = %q, want a '2 workers dispatched' line", res.Content)
	}
	if !strings.Contains(res.Content, researchSlugFor(testUser)+"/") {
		t.Errorf("result content = %q, want the evidence page path", res.Content)
	}

	// Workers are genuinely still in flight at this point.
	recvN(t, inv.started, 2, "workers to start")
	if n := inv.inFlight.Load(); n != 2 {
		t.Fatalf("in-flight after Execute returned = %d, want 2", n)
	}

	inv.release <- struct{}{}
	inv.release <- struct{}{}
	waitForAppend(t, be, "complete: 2/2 workers succeeded")
}

// One worker fails → a "failed:" line lands on the page and the
// completion line counts successes only (1/2).
func TestSpawn_WorkerFailureIsRecorded(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{
		failFor: func(prompt string) error {
			if strings.Contains(prompt, "How does diff3 merge work?") {
				return fmt.Errorf("synthetic worker crash")
			}
			return nil
		},
	}
	s := newSkill(t, inv, be, Options{})

	res := execute(t, s, userCtx(), twoTaskArgs())
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}

	failLine := waitForAppend(t, be, "failed:")
	if !strings.Contains(failLine, "How does diff3 merge work?") {
		t.Errorf("failure line lacks the question: %q", failLine)
	}
	if !strings.Contains(failLine, "synthetic worker crash") {
		t.Errorf("failure line lacks the error: %q", failLine)
	}

	done := waitForAppend(t, be, "complete:")
	if !strings.Contains(done, "1/2 workers succeeded") {
		t.Errorf("completion line = %q, want '1/2 workers succeeded'", done)
	}
}

// A caller with no resolved identity is refused before anything is
// created or dispatched (memory-skill posture).
func TestSpawn_RequiresIdentity(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s := newSkill(t, inv, be, Options{})

	res := execute(t, s, context.Background(), twoTaskArgs())
	if res.Error == "" || !strings.Contains(res.Error, "identity") {
		t.Fatalf("want an identity refusal, got error=%q content=%q", res.Error, res.Content)
	}
	if len(inv.callsSnapshot()) != 0 {
		t.Errorf("workers were dispatched without identity")
	}
	if len(be.appendsSnapshot()) != 0 || len(be.books) != 0 {
		t.Errorf("backend was touched without identity")
	}
}

// Schema-boundary validation: empty tasks, >8 tasks, missing topic,
// blank question — each refused with a ToolResult error, nothing
// dispatched.
func TestSpawn_Validation(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"empty tasks", `{"topic":"t","tasks":[]}`, "at least 1"},
		{"too many tasks", `{"topic":"t","tasks":[
			{"question":"1"},{"question":"2"},{"question":"3"},{"question":"4"},{"question":"5"},
			{"question":"6"},{"question":"7"},{"question":"8"},{"question":"9"}]}`, "capped at 8"},
		{"missing topic", `{"tasks":[{"question":"q"}]}`, "topic is required"},
		{"blank question", `{"topic":"t","tasks":[{"question":"  "}]}`, "question is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := newMockBackend()
			inv := &mockInvoke{}
			s := newSkill(t, inv, be, Options{})
			res := execute(t, s, userCtx(), tc.args)
			if res.Error == "" || !strings.Contains(res.Error, tc.want) {
				t.Fatalf("want error containing %q, got error=%q", tc.want, res.Error)
			}
			if len(inv.callsSnapshot()) != 0 {
				t.Errorf("workers were dispatched despite invalid args")
			}
		})
	}
}

// page_slug pointing at nothing is a clear user-facing error, not a
// silent new page.
func TestSpawn_MissingPageSlug(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s := newSkill(t, inv, be, Options{})

	res := execute(t, s, userCtx(), `{"topic":"t","tasks":[{"question":"q"}],"page_slug":"nope"}`)
	if res.Error == "" || !strings.Contains(res.Error, `"nope"`) {
		t.Fatalf("want a page-not-found error naming the slug, got error=%q", res.Error)
	}
	if len(inv.callsSnapshot()) != 0 {
		t.Errorf("workers were dispatched despite the missing page")
	}
}

// A page_slug that DOES exist is reused — no second evidence page.
func TestSpawn_ReusesExistingPage(t *testing.T) {
	be := newMockBackend()
	book, _ := be.EnsureResearchBook(context.Background(), testUser)
	be.seedPage(book.ID, "research-topic", "page-keep")
	inv := &mockInvoke{}
	s := newSkill(t, inv, be, Options{})

	res := execute(t, s, userCtx(), `{"topic":"t","tasks":[{"question":"q"}],"page_slug":"research-topic"}`)
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	waitForAppend(t, be, "complete: 1/1")

	calls := inv.callsSnapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].prompt, `page_slug="research-topic"`) {
		t.Errorf("worker prompt should target the reused page: %+v", calls)
	}
	if be.nextPage != 0 {
		t.Errorf("a new page was created despite page_slug reuse")
	}
}

// Concurrent users each get their OWN research book (slug
// research:{userID}) — the shared fixed-slug book that only the first
// user could use is gone. Both runs succeed and land in distinct books.
func TestSpawn_PerUserBooksDoNotCollide(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s := newSkill(t, inv, be, Options{})

	ctxA := skills.WithContext(context.Background(), skills.SessionContext{UserID: "alice"})
	ctxB := skills.WithContext(context.Background(), skills.SessionContext{UserID: "bob"})

	if res := execute(t, s, ctxA, `{"topic":"t","tasks":[{"question":"q"}]}`); res.Error != "" {
		t.Fatalf("alice run errored: %s", res.Error)
	}
	if res := execute(t, s, ctxB, `{"topic":"t","tasks":[{"question":"q"}]}`); res.Error != "" {
		t.Fatalf("bob run errored (shared-book collision?): %s", res.Error)
	}
	waitForAppend(t, be, "complete: 1/1 workers succeeded")
	waitForAppend(t, be, "complete: 1/1 workers succeeded")

	if be.book("research:alice") == nil || be.book("research:bob") == nil {
		t.Fatalf("each user should own a research book; books=%v", func() []string {
			be.mu.Lock()
			defer be.mu.Unlock()
			var ks []string
			for k := range be.books {
				ks = append(ks, k)
			}
			return ks
		}())
	}
}

// First spawn creates the per-user book and the evidence page carries
// the Plan checklist.
func TestSpawn_CreatesBookAndChecklist(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s := newSkill(t, inv, be, Options{})

	res := execute(t, s, userCtx(), twoTaskArgs())
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	waitForAppend(t, be, "complete: 2/2")

	book := be.book(researchSlugFor(testUser))
	if book == nil {
		t.Fatalf("per-user research book was not created")
	}
	be.mu.Lock()
	var page *admin.WikiPage
	for _, p := range be.byID {
		page = p
	}
	be.mu.Unlock()
	if page == nil || page.BookID != book.ID {
		t.Fatalf("no evidence page created in the research book")
	}
	for _, want := range []string{
		"# Research: local first software",
		"## Plan",
		"- [ ] What is CRDT convergence?",
		"- [ ] How does diff3 merge work?",
		"## Findings",
	} {
		if !strings.Contains(page.Content, want) {
			t.Errorf("evidence page missing %q; content:\n%s", want, page.Content)
		}
	}
}

// Zero-valued options fall back to the spec defaults.
func TestNew_Defaults(t *testing.T) {
	s := New(Options{})
	if s.opts.MaxWorkers != defaultMaxWorkers {
		t.Errorf("MaxWorkers = %d, want %d", s.opts.MaxWorkers, defaultMaxWorkers)
	}
	if s.opts.WorkerSearchBudget != defaultSearchBudget {
		t.Errorf("WorkerSearchBudget = %d, want %d", s.opts.WorkerSearchBudget, defaultSearchBudget)
	}
	if s.opts.WorkerTier != defaultWorkerTier {
		t.Errorf("WorkerTier = %q, want %q", s.opts.WorkerTier, defaultWorkerTier)
	}
}

// The MaxWorkers cap is gateway-wide, not per-spawn: two overlapping
// Execute calls share one semaphore, so total in-flight workers never
// exceed the cap. (Review finding: a per-dispatch semaphore let
// overlapping runs fill every inference slot.)
func TestSpawn_SemaphoreIsGatewayWide(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{
		started: make(chan struct{}, 8),
		release: make(chan struct{}, 8),
	}
	s := newSkill(t, inv, be, Options{MaxWorkers: 2})

	if res := execute(t, s, userCtx(), twoTaskArgs()); res.Error != "" {
		t.Fatalf("run 1: unexpected tool error: %s", res.Error)
	}
	recvN(t, inv.started, 2, "run 1's workers to start")

	// Overlapping second spawn: its workers must queue behind the SAME
	// pool, not mint a fresh one.
	if res := execute(t, s, userCtx(), twoTaskArgs()); res.Error != "" {
		t.Fatalf("run 2: unexpected tool error: %s", res.Error)
	}
	if n := inv.inFlight.Load(); n != 2 {
		t.Fatalf("in-flight after second spawn = %d, want still 2", n)
	}

	// Free one slot; exactly one queued worker may enter.
	inv.release <- struct{}{}
	recvN(t, inv.started, 1, "first queued worker to start")
	if max := inv.maxInFlight.Load(); max > 2 {
		t.Fatalf("max concurrent workers across runs = %d, want ≤ 2", max)
	}

	// Drain everything; both runs complete.
	for i := 0; i < 3; i++ {
		inv.release <- struct{}{}
	}
	waitForAppend(t, be, "complete: 2/2 workers succeeded")
	waitForAppend(t, be, "complete: 2/2 workers succeeded")
	if max := inv.maxInFlight.Load(); max > 2 {
		t.Errorf("max concurrent workers = %d, want ≤ 2", max)
	}
	if got := len(inv.callsSnapshot()); got != 4 {
		t.Errorf("total worker invocations = %d, want 4", got)
	}
}

// Close() cuts in-flight workers: their contexts cancel, the failure
// lands on the evidence page, and nothing runs on after shutdown.
// (Review finding: workers on context.Background() outlived shutdown.)
func TestClose_CancelsInFlightWorkers(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{
		started: make(chan struct{}, 2),
		release: make(chan struct{}), // never fed — workers block until ctx dies
	}
	s := newSkill(t, inv, be, Options{MaxWorkers: 2})

	res := execute(t, s, userCtx(), `{"topic":"t","tasks":[{"question":"q1"}]}`)
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	recvN(t, inv.started, 1, "worker to start")

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The blocked worker's ctx dies → Invoke errors → failure line +
	// 0/1 completion line (appends run on fresh contexts, so they land
	// even after Close).
	waitForAppend(t, be, "failed: context canceled")
	waitForAppend(t, be, "complete: 0/1 workers succeeded")
}

// An unknown worker_tier must not silently reroute workers through the
// trusted path (shardModelOverride returns ok=false for unknown
// aliases) — New falls back to the default tier and logs.
func TestNew_InvalidTierFallsBack(t *testing.T) {
	s := New(Options{WorkerTier: "tier-2"}) // typo'd alias
	if s.opts.WorkerTier != defaultWorkerTier {
		t.Errorf("WorkerTier = %q, want fallback to %q", s.opts.WorkerTier, defaultWorkerTier)
	}
	// Case-insensitive acceptance of a real alias.
	if s := New(Options{WorkerTier: "Tier2"}); s.opts.WorkerTier != "Tier2" {
		t.Errorf("WorkerTier = %q, want Tier2 accepted", s.opts.WorkerTier)
	}
}

// waitForUpdate mirrors waitForAppend for UpdatePage contents.
func waitForUpdate(t *testing.T, be *mockBackend, substr string) string {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		select {
		case text := <-be.updateCh:
			if strings.Contains(text, substr) {
				return text
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a page update containing %q", substr)
		}
	}
}

// The compose tool is offered exactly when a writer model is
// configured — tool presence is the capability signal the SKILL.md
// branches on (RESEARCH-SKILL-SPEC §6.5).
func TestTools_ComposeGatedOnWriterModel(t *testing.T) {
	names := func(s *Skill) []string {
		var out []string
		for _, td := range s.Tools() {
			out = append(out, td.Name)
		}
		return out
	}
	without := names(New(Options{}))
	if len(without) != 1 || without[0] != toolName {
		t.Errorf("tools without writer model = %v, want [%s]", without, toolName)
	}
	with := names(New(Options{WriterModel: "prose-model"}))
	if len(with) != 2 || with[1] != composeToolName {
		t.Errorf("tools with writer model = %v, want [%s %s]", with, toolName, composeToolName)
	}
	// And dispatching compose without a writer model is an unknown tool.
	s := newSkill(t, &mockInvoke{}, newMockBackend(), Options{})
	if _, err := s.Execute(userCtx(), composeToolName, json.RawMessage(`{"topic":"t","evidence":"e"}`)); err == nil {
		t.Error("compose without writer_model should be an unknown tool error")
	}
}

// worker_model pins the worker envelopes to an explicit registry model
// via ModelOverride; unset keeps tier routing (empty ModelOverride).
func TestSpawn_WorkerModelOverride(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s := newSkill(t, inv, be, Options{WorkerModel: "gemma-4-31b-dense"})
	if res := execute(t, s, userCtx(), `{"topic":"t","tasks":[{"question":"q1"}]}`); res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	waitForAppend(t, be, "complete: 1/1 workers succeeded")
	calls := inv.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("worker invocations = %d, want 1", len(calls))
	}
	if got := calls[0].overrides.ModelOverride; got != "gemma-4-31b-dense" {
		t.Errorf("worker ModelOverride = %q, want gemma-4-31b-dense", got)
	}
	if got := calls[0].overrides.TierHint; got != defaultWorkerTier {
		t.Errorf("worker TierHint = %q, want %q (tier still shapes thinking)", got, defaultWorkerTier)
	}
}

// Compose from an evidence page: the page is read server-side into the
// writer prompt, a stub note appears in the personal book immediately,
// and the writer's completion replaces it.
func TestCompose_FromEvidencePage(t *testing.T) {
	be := newMockBackend()
	book, _ := be.EnsureResearchBook(context.Background(), testUser)
	ev := be.seedPage(book.ID, "evidence-topic", "page-ev")
	be.mu.Lock()
	ev.Content = "- CRDTs converge — [Shapiro](https://example.com/crdt)"
	be.mu.Unlock()

	inv := &mockInvoke{
		started: make(chan struct{}, 2),
		release: make(chan struct{}, 2),
	}
	s := newSkill(t, inv, be, Options{WriterModel: "prose-model"})

	res := executeTool(t, s, userCtx(), composeToolName, `{"topic":"local first software","evidence_page_slug":"evidence-topic"}`)
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "Writer dispatched") || !strings.Contains(res.Content, "Research: local first software") {
		t.Errorf("compose result = %q", res.Content)
	}

	// Stub exists in the personal book before the writer finishes.
	be.mu.Lock()
	var stub *admin.WikiPage
	for _, p := range be.pages {
		if strings.Contains(p.Content, "Composing the research note") {
			stub = p
		}
	}
	be.mu.Unlock()
	if stub == nil {
		t.Fatal("no stub note created in the personal book")
	}
	if !strings.HasPrefix(stub.BookID, "book-personal:") {
		t.Errorf("stub landed in %s, want the personal book", stub.BookID)
	}

	recvN(t, inv.started, 1, "writer to start")
	calls := inv.callsSnapshot()
	ov := calls[0].overrides
	if ov.ModelOverride != "prose-model" {
		t.Errorf("writer ModelOverride = %q, want prose-model", ov.ModelOverride)
	}
	if len(ov.ToolAllowlist) != 0 {
		t.Errorf("writer allowlist = %v, want empty (pure completion)", ov.ToolAllowlist)
	}
	if ov.MaxTokens != writerMaxTokens {
		t.Errorf("writer MaxTokens = %d, want %d", ov.MaxTokens, writerMaxTokens)
	}
	if ov.SearchBudget != 0 {
		t.Errorf("writer SearchBudget = %d, want 0", ov.SearchBudget)
	}
	if !strings.Contains(calls[0].prompt, "Shapiro") {
		t.Errorf("writer prompt lacks the evidence content: %q", calls[0].prompt)
	}

	inv.release <- struct{}{}
	// The mock invoke's reply becomes the note.
	got := waitForUpdate(t, be, "DONE")
	if got != "DONE — findings appended" {
		t.Errorf("note content = %q", got)
	}

	// Compose no longer eagerly deletes the evidence page (it raced
	// note-loss and newer-evidence-loss); the retention sweep owns
	// cleanup. The page must still be present right after the note.
	be.mu.Lock()
	_, stillThere := be.pages[book.ID+"/"+ev.Slug]
	be.mu.Unlock()
	if !stillThere {
		t.Error("compose eagerly deleted the evidence page — cleanup should be the sweep's job")
	}
}

// Inline evidence works without any research book.
func TestCompose_InlineEvidence(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s := newSkill(t, inv, be, Options{WriterModel: "prose-model"})
	res := executeTool(t, s, userCtx(), composeToolName, `{"topic":"t","evidence":"- fact — [Src](https://x)"}`)
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	waitForUpdate(t, be, "DONE")
	calls := inv.callsSnapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].prompt, "fact — [Src]") {
		t.Fatalf("writer prompt missing inline evidence; calls=%d", len(calls))
	}
}

// A writer failure replaces the stub placeholder with a visible error.
func TestCompose_WriterFailureLandsOnStub(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{failFor: func(string) error { return errors.New("model exploded") }}
	s := newSkill(t, inv, be, Options{WriterModel: "prose-model"})
	if res := executeTool(t, s, userCtx(), composeToolName, `{"topic":"t","evidence":"e"}`); res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	got := waitForUpdate(t, be, "Compose failed")
	if !strings.Contains(got, "model exploded") {
		t.Errorf("failure note = %q", got)
	}
}

// Compose argument validation: topic required; exactly one evidence
// source; a named evidence page must exist.
func TestCompose_Validation(t *testing.T) {
	be := newMockBackend()
	s := newSkill(t, &mockInvoke{}, be, Options{WriterModel: "prose-model"})
	cases := []struct {
		args string
		want string
	}{
		{`{"evidence":"e"}`, "topic is required"},
		{`{"topic":"t"}`, "exactly one of"},
		{`{"topic":"t","evidence":"e","evidence_page_slug":"s"}`, "exactly one of"},
		{`{"topic":"t","evidence_page_slug":"missing"}`, "not found"},
	}
	for _, c := range cases {
		res := executeTool(t, s, userCtx(), composeToolName, c.args)
		if !strings.Contains(res.Error, c.want) {
			t.Errorf("args %s: Error = %q, want substring %q", c.args, res.Error, c.want)
		}
	}
}

// Role prompts come from the embedded builtin package with the search
// budget substituted; the compiled fallbacks never leak the
// placeholder.
func TestNew_PromptsFromBuiltinPackage(t *testing.T) {
	s := New(Options{WorkerSearchBudget: 7})
	if strings.Contains(s.workerPrompt, searchBudgetPlaceholder) {
		t.Error("worker prompt still contains the {{SEARCH_BUDGET}} placeholder")
	}
	if !strings.Contains(s.workerPrompt, "7 searches") {
		t.Errorf("worker prompt lacks the substituted budget:\n%s", s.workerPrompt)
	}
	if !strings.Contains(s.writerPrompt, "Sources") {
		t.Errorf("writer prompt lacks the Sources section:\n%s", s.writerPrompt)
	}
}

// A user edit landing on the stub during the compose window must NOT
// be clobbered: the writer's UpdatePage carries the stub's IfMatch, a
// conflict comes back stale, and the composed note falls back to the
// ID-addressed atomic append — the note is delivered AND the user's
// text survives. (Review finding: nil-IfMatch UpdatePage was the exact
// silent-clobber path the WIKI-SYNC work closed.)
func TestCompose_UserEditDuringComposeFallsBackToAppend(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{
		started: make(chan struct{}, 2),
		release: make(chan struct{}, 2),
	}
	s := newSkill(t, inv, be, Options{WriterModel: "prose-model"})

	res := executeTool(t, s, userCtx(), composeToolName, `{"topic":"t","evidence":"- fact — [Src](https://x)"}`)
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	recvN(t, inv.started, 1, "writer to start")

	// Find the stub and simulate the user typing into it mid-compose.
	be.mu.Lock()
	var stub *admin.WikiPage
	for _, p := range be.pages {
		if strings.Contains(p.Content, "Composing the research note") {
			stub = p
		}
	}
	be.mu.Unlock()
	if stub == nil {
		t.Fatal("no stub created")
	}
	be.touchPage(stub.BookID, stub.Slug, "user's own draft — must survive")

	inv.release <- struct{}{}
	// The composed note arrives via AppendPage (the fallback), not a
	// content replacement.
	got := waitForAppend(t, be, "DONE — findings appended")
	if !strings.HasPrefix(got, "\n---\n\n") {
		t.Errorf("append fallback lacks the separator: %q", got)
	}
	be.mu.Lock()
	final := be.pages[stub.BookID+"/"+stub.Slug].Content
	be.mu.Unlock()
	if !strings.Contains(final, "user's own draft — must survive") {
		t.Errorf("user edit was clobbered; page:\n%s", final)
	}
	if !strings.Contains(final, "DONE — findings appended") {
		t.Errorf("composed note missing from page:\n%s", final)
	}
}

// ── Autonomous run tests (§6.7) ─────────────────────────────────────

// mockRuns is an in-memory RunStore.
type mockRuns struct {
	mu     sync.Mutex
	byID   map[string]*admin.ResearchRun
	active map[string]*admin.ResearchRun // convID → non-terminal run
	seq    int
}

func newMockRuns() *mockRuns {
	return &mockRuns{byID: map[string]*admin.ResearchRun{}, active: map[string]*admin.ResearchRun{}}
}

func (m *mockRuns) Create(_ context.Context, userID, convID, topic string, questions []string, evB, evP string) (*admin.ResearchRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.active[convID]; ok && r.UserID == userID {
		return nil, admin.ErrActiveRunExists // mirror the DB partial-unique index
	}
	m.seq++
	roster := make([]admin.ResearchWorker, len(questions))
	for i, q := range questions {
		roster[i] = admin.ResearchWorker{Question: q, State: admin.WorkerQueued}
	}
	r := &admin.ResearchRun{
		ID: fmt.Sprintf("run-%d", m.seq), UserID: userID, ConversationID: convID,
		Topic: topic, Status: admin.RunStatusResearching, Round: 1, WorkersTotal: len(questions),
		EvidenceBookSlug: evB, EvidencePageSlug: evP, Workers: roster,
	}
	m.byID[r.ID] = r
	m.active[convID] = r
	return r, nil
}

// CancelRun flags a run cancelled (the authority dispatch/advanceRun
// check) and cuts its registered workers. It flags regardless of whether
// a worker context is currently registered (a run can be cancelled
// between rounds). This backs the card's stop button.
func TestCancelRun(t *testing.T) {
	s := &Skill{}
	called := false
	s.runCancels.Store("run-1", context.CancelFunc(func() { called = true }))

	if !s.CancelRun("run-1") {
		t.Fatal("CancelRun(run-1) = false, want true")
	}
	if !called {
		t.Error("cancel func was not invoked")
	}
	if _, ok := s.runCancels.Load("run-1"); ok {
		t.Error("run-1 worker-cancel entry should have been removed")
	}
	if !s.isCancelled("run-1") {
		t.Error("run-1 should be flagged cancelled")
	}

	// A run with no registered worker context (cancelled between rounds)
	// is still flagged so the next dispatch/advance bails.
	if !s.CancelRun("run-2") {
		t.Error("CancelRun(run-2) = false, want true (flags even without a registered ctx)")
	}
	if !s.isCancelled("run-2") {
		t.Error("run-2 should be flagged cancelled")
	}
	if s.isCancelled("never") {
		t.Error("an un-cancelled run must not read as cancelled")
	}
}

func (m *mockRuns) Get(_ context.Context, id string) (*admin.ResearchRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return nil, admin.ErrRunNotFound
	}
	return r, nil
}

func (m *mockRuns) UpdateIfActive(ctx context.Context, id string, p admin.RunPatch) (bool, error) {
	m.mu.Lock()
	r, ok := m.byID[id]
	terminal := ok && (r.Status == admin.RunStatusDone || r.Status == admin.RunStatusFailed)
	m.mu.Unlock()
	if !ok || terminal {
		return false, nil
	}
	return true, m.Update(ctx, id, p)
}

func (m *mockRuns) IncrementWorkerDone(_ context.Context, id string, tokens int64, pages int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return admin.ErrRunNotFound
	}
	r.WorkersDone++
	r.Tokens += tokens
	r.PagesRead += pages
	return nil
}

func (m *mockRuns) SetWorkerState(_ context.Context, id string, idx int, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return admin.ErrRunNotFound
	}
	if idx < 0 || idx >= len(r.Workers) {
		return nil
	}
	r.Workers[idx].State = state
	return nil
}

func (m *mockRuns) ActiveForConversation(_ context.Context, userID, convID string) (*admin.ResearchRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.active[convID]; ok && r.UserID == userID {
		return r, nil
	}
	return nil, admin.ErrRunNotFound
}

func (m *mockRuns) Update(_ context.Context, id string, p admin.RunPatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return admin.ErrRunNotFound
	}
	if p.Status != nil {
		r.Status = *p.Status
		if r.Status == admin.RunStatusDone || r.Status == admin.RunStatusFailed {
			delete(m.active, r.ConversationID)
		}
	}
	if p.Round != nil {
		r.Round = *p.Round
	}
	if p.WorkersTotal != nil {
		r.WorkersTotal = *p.WorkersTotal
	}
	if p.WorkersDone != nil {
		r.WorkersDone = *p.WorkersDone
	}
	return nil
}

func (m *mockRuns) get(id string) admin.ResearchRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.byID[id]
}

// convCtx is a kickoff turn: the session id is the conversation id.
func convCtx(convID string) context.Context {
	return skills.WithContext(context.Background(), skills.SessionContext{UserID: testUser, SessionID: convID})
}

// autonomousSkill wires the skill with a mock run store + a synthesize
// spy that marks the run done (like the real closure) and signals.
func autonomousSkill(t *testing.T, inv *mockInvoke, be *mockBackend, opts Options) (*Skill, *mockRuns, chan string) {
	s := newSkill(t, inv, be, opts)
	runs := newMockRuns()
	synthCh := make(chan string, 8)
	s.SetOrchestrator(runs, func(ctx context.Context, runID string) {
		st := admin.RunStatusDone
		_ = runs.Update(ctx, runID, admin.RunPatch{Status: &st})
		synthCh <- runID
	})
	return s, runs, synthCh
}

// Kickoff creates a tracked run, returns a background message, and when
// the (all-succeeding) batch finishes, synthesis fires automatically —
// no "continue".
func TestAutonomous_KickoffThenSynthesize(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s, runs, synthCh := autonomousSkill(t, inv, be, Options{})

	res := executeTool(t, s, convCtx("conv-1"), toolName,
		`{"topic":"Meow Wolf","tasks":[{"question":"q1"},{"question":"q2"}]}`)
	if res.Error != "" {
		t.Fatalf("kickoff error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "background") {
		t.Errorf("kickoff message should say it's working in the background: %q", res.Content)
	}
	if _, err := runs.ActiveForConversation(context.Background(), testUser, "conv-1"); err != nil {
		t.Fatalf("no active run after kickoff: %v", err)
	}
	select {
	case id := <-synthCh:
		if runs.get(id).Status != admin.RunStatusDone {
			t.Errorf("run not marked done by synth")
		}
	case <-time.After(waitTimeout):
		t.Fatal("synthesis was never triggered — the run needs a 'continue'")
	}
	if _, err := runs.ActiveForConversation(context.Background(), testUser, "conv-1"); !errors.Is(err, admin.ErrRunNotFound) {
		t.Error("run still active after synthesis")
	}
}

// A failed sub-question triggers a deterministic gap-fill round (the
// skill re-dispatches just the failed task); after the cap, synthesis
// runs regardless. No model call drives the loop.
func TestAutonomous_GapFillRetriesFailedTask(t *testing.T) {
	be := newMockBackend()
	// q2 always fails → round 1 fails it, round 2 re-dispatches it, fails
	// again → round == MaxRounds(2) → synthesize.
	inv := &mockInvoke{failFor: func(prompt string) error {
		if strings.Contains(prompt, "q2") {
			return errors.New("worker crash")
		}
		return nil
	}}
	s, runs, synthCh := autonomousSkill(t, inv, be, Options{MaxRounds: 2})

	res := executeTool(t, s, convCtx("conv-2"), toolName,
		`{"topic":"T","tasks":[{"question":"q1"},{"question":"q2"}]}`)
	if res.Error != "" {
		t.Fatalf("kickoff error: %s", res.Error)
	}
	select {
	case id := <-synthCh:
		if r := runs.get(id); r.Round != 2 {
			t.Errorf("run reached round %d, want the gap-fill round 2", r.Round)
		}
	case <-time.After(waitTimeout):
		t.Fatal("synthesis never fired after the gap-fill cap")
	}
	// q2 was attempted twice (round 1 + the gap-fill round 2); q1 once.
	var q1, q2 int
	for _, c := range inv.callsSnapshot() {
		if strings.Contains(c.prompt, "q2") {
			q2++
		} else if strings.Contains(c.prompt, "q1") {
			q1++
		}
	}
	if q1 != 1 || q2 != 2 {
		t.Errorf("attempts q1=%d q2=%d, want q1=1 q2=2 (only the failed task retries)", q1, q2)
	}
}

// A synthesis turn (run session) must NOT spawn more workers — the loop
// is skill-driven.
func TestAutonomous_SpawnRefusedDuringSynthesis(t *testing.T) {
	be := newMockBackend()
	inv := &mockInvoke{}
	s, _, _ := autonomousSkill(t, inv, be, Options{})
	ctx := skills.WithContext(context.Background(), skills.SessionContext{UserID: testUser, SessionID: "research:run:xyz"})
	res := executeTool(t, s, ctx, toolName, twoTaskArgs())
	if !strings.Contains(res.Error, "synthesiz") {
		t.Fatalf("spawn during a run session should be refused, got %q / %q", res.Error, res.Content)
	}
	if len(inv.callsSnapshot()) != 0 {
		t.Error("workers dispatched during a synthesis turn")
	}
}

// A second kickoff while a run is active is refused (one run per
// conversation at a time).
func TestAutonomous_SecondKickoffRefusedWhileActive(t *testing.T) {
	be := newMockBackend()
	// Workers block so the first run stays active.
	inv := &mockInvoke{started: make(chan struct{}, 4), release: make(chan struct{})}
	s, _, _ := autonomousSkill(t, inv, be, Options{})

	if res := executeTool(t, s, convCtx("conv-3"), toolName, twoTaskArgs()); res.Error != "" {
		t.Fatalf("first kickoff error: %s", res.Error)
	}
	recvN(t, inv.started, 1, "first run's workers to start")
	res := executeTool(t, s, convCtx("conv-3"), toolName, twoTaskArgs())
	if !strings.Contains(res.Error, "already underway") {
		t.Errorf("second kickoff should be refused while active, got %q", res.Error)
	}
}

// spawnArgs.Tasks must survive the ways local models (qwen) mis-encode
// the array: the schema-correct array, a double-encoded JSON *string*
// (the observed spawn_research_workers failure), a single object instead
// of an array, and bare-string elements. All must unmarshal to the same
// []taskSpec rather than erroring out and dropping the deep run.
func TestSpawnArgs_TolerantTasksUnmarshal(t *testing.T) {
	want := []taskSpec{{Question: "What is X?"}, {Question: "How does Y work?", Hints: "primary sources"}}

	cases := map[string]string{
		"array (schema)":        `{"topic":"T","tasks":[{"question":"What is X?"},{"question":"How does Y work?","hints":"primary sources"}]}`,
		"double-encoded string": `{"topic":"T","tasks":"[{\"question\":\"What is X?\"},{\"question\":\"How does Y work?\",\"hints\":\"primary sources\"}]"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var got spawnArgs
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("unmarshal errored: %v", err)
			}
			if len(got.Tasks) != len(want) {
				t.Fatalf("got %d tasks, want %d (%+v)", len(got.Tasks), len(want), got.Tasks)
			}
			for i := range want {
				if got.Tasks[i] != want[i] {
					t.Errorf("task[%d] = %+v, want %+v", i, got.Tasks[i], want[i])
				}
			}
		})
	}

	t.Run("single object wrapped", func(t *testing.T) {
		var got spawnArgs
		if err := json.Unmarshal([]byte(`{"topic":"T","tasks":{"question":"only one"}}`), &got); err != nil {
			t.Fatalf("unmarshal errored: %v", err)
		}
		if len(got.Tasks) != 1 || got.Tasks[0].Question != "only one" {
			t.Fatalf("single object not wrapped to one task: %+v", got.Tasks)
		}
	})

	t.Run("bare string elements", func(t *testing.T) {
		var got spawnArgs
		if err := json.Unmarshal([]byte(`{"topic":"T","tasks":["q one","q two"]}`), &got); err != nil {
			t.Fatalf("unmarshal errored: %v", err)
		}
		if len(got.Tasks) != 2 || got.Tasks[0].Question != "q one" || got.Tasks[1].Question != "q two" {
			t.Fatalf("bare string elements not accepted: %+v", got.Tasks)
		}
	})

	t.Run("empty and null are nil, not error", func(t *testing.T) {
		for _, raw := range []string{`{"topic":"T","tasks":null}`, `{"topic":"T","tasks":"[]"}`} {
			var got spawnArgs
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("%s errored: %v", raw, err)
			}
			if len(got.Tasks) != 0 {
				t.Errorf("%s → %d tasks, want 0", raw, len(got.Tasks))
			}
		}
	})
}
