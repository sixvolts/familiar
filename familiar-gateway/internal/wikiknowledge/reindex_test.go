package wikiknowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReindexStore keeps the job's state in memory: pages by id (a
// missing id is a page that is gone), and per-page progress.
type fakeReindexStore struct {
	mu       sync.Mutex
	order    []string
	pages    map[string]SaveEvent
	done     map[string]bool
	attempts map[string]int
	finished bool
	locked   bool // another gateway holds the lock
	held     bool // this job holds it
	begun    bool // the job recorded its start (the skip cutoff)
	loads    int
}

func newFakeReindexStore(pages ...SaveEvent) *fakeReindexStore {
	s := &fakeReindexStore{pages: map[string]SaveEvent{}, done: map[string]bool{}, attempts: map[string]int{}}
	for _, p := range pages {
		s.order = append(s.order, p.PageID)
		s.pages[p.PageID] = p
	}
	return s
}

func (s *fakeReindexStore) Lock(context.Context) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return nil, false, nil
	}
	s.held = true
	return func() {
		s.mu.Lock()
		s.held = false
		s.mu.Unlock()
	}, true, nil
}

func (s *fakeReindexStore) holding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}
func (s *fakeReindexStore) Done(context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished, nil
}
func (s *fakeReindexStore) Begin(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begun = true
	return nil
}
func (s *fakeReindexStore) PendingPages(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, id := range s.order {
		if !s.done[id] && s.attempts[id] < maxPageAttempts {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
func (s *fakeReindexStore) LoadPage(_ context.Context, id string) (SaveEvent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	evt, ok := s.pages[id]
	return evt, ok, nil
}
func (s *fakeReindexStore) MarkDone(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done[id] = true
	return nil
}
func (s *fakeReindexStore) RecordFailure(_ context.Context, id, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[id]++
	return nil
}
func (s *fakeReindexStore) Finish(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = true
	gaveUp := 0
	for _, id := range s.order {
		if !s.done[id] {
			gaveUp++
		}
	}
	return gaveUp, nil
}

func page(id, content string) SaveEvent {
	return SaveEvent{BookID: "book-uuid", BookSlug: "notes", PageID: id, PageSlug: id,
		UserID: "operator", Title: id, Content: content}
}

// reindexer builds a job whose waits are recorded, not slept.
func reindexer(p *Pipeline, st ReindexStore) (*Reindexer, *[]time.Duration) {
	var waits []time.Duration
	var mu sync.Mutex
	r := &Reindexer{Pipeline: p, Store: st, Pause: time.Second, Backoff: time.Hour}
	r.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	return r, &waits
}

func committedRefs(eng *fakeEngine) []string {
	var refs []string
	for _, c := range eng.calls() {
		refs = append(refs, c.sourceRef)
	}
	sort.Strings(refs)
	return refs
}

// Every pending page is re-extracted once, under its page id, and then
// the job records itself done.
func TestReindex_ReextractsEveryPageThenFinishes(t *testing.T) {
	eng := &fakeEngine{}
	p := newPipelineWith(eng, &fakeSidecar{}, nil, nil)
	st := newFakeReindexStore(
		page("p1", "The biopsy is on Monday at the Elm Street clinic."),
		page("p2", "The dentist appointment is on Thursday at 3pm."),
		page("p3", "tiny"),
	)
	r, waits := reindexer(p, st)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(committedRefs(eng), ","); got != "page:p1,page:p2,page:p3" {
		t.Errorf("replaced facts for %s, want every page once", got)
	}
	if !st.begun {
		t.Error("the job never recorded its start, so pages saved during it aren't skipped")
	}
	if !st.finished || len(st.done) != 3 {
		t.Errorf("finished=%v done=%v, want all three done and the job finished", st.finished, st.done)
	}
	for _, w := range *waits {
		if w != time.Second {
			t.Errorf("waited %v; a clean run only pauses between pages", w)
		}
	}
}

func TestReindex_NothingToDoWhenDoneOrLocked(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *fakeReindexStore
	}{
		{"already done", func() *fakeReindexStore {
			s := newFakeReindexStore(page("p1", "The biopsy is on Monday at the Elm Street clinic."))
			s.finished = true
			return s
		}()},
		{"another gateway has it", func() *fakeReindexStore {
			s := newFakeReindexStore(page("p1", "The biopsy is on Monday at the Elm Street clinic."))
			s.locked = true
			return s
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &fakeSidecar{}
			r, _ := reindexer(newPipelineWith(&fakeEngine{}, sc, nil, nil), tc.st)
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if sc.n() != 0 {
				t.Errorf("ran %d extraction(s)", sc.n())
			}
		})
	}
}

// A page the extractor keeps failing on is retried, then given up on,
// and the job still finishes.
func TestReindex_FailingPageGivenUp(t *testing.T) {
	eng := &fakeEngine{}
	sc := &fakeSidecar{failOn: "POISON"}
	st := newFakeReindexStore(
		page("good", "The biopsy is on Monday at the Elm Street clinic."),
		page("bad", "POISON page the extractor always chokes on."),
	)
	r, waits := reindexer(newPipelineWith(eng, sc, nil, nil), st)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The passes after the first retry only the bad page, fail, and
	// succeed at nothing: each waits out the backoff before the next, so
	// a short outage can't use up a page's attempts.
	backoffs := 0
	for _, w := range *waits {
		if w == r.Backoff {
			backoffs++
		}
	}
	if backoffs != maxPageAttempts-1 {
		t.Errorf("backed off %d time(s), want %d", backoffs, maxPageAttempts-1)
	}
	if st.attempts["bad"] != maxPageAttempts || st.done["bad"] {
		t.Errorf("bad page: attempts=%d done=%v, want %d attempts and given up", st.attempts["bad"], st.done["bad"], maxPageAttempts)
	}
	if !st.finished || !st.done["good"] {
		t.Errorf("finished=%v good done=%v", st.finished, st.done["good"])
	}
}

// An extractor that fails everything is an outage: the pass stops,
// nothing counts against the pages, and the job backs off.
func TestReindex_OutageDoesNotCountAgainstPages(t *testing.T) {
	sc := &fakeSidecar{err: fmt.Errorf("connection refused")}
	var pages []SaveEvent
	for i := 0; i < 6; i++ {
		pages = append(pages, page(fmt.Sprintf("p%d", i), "The biopsy is on Monday at the Elm Street clinic."))
	}
	st := newFakeReindexStore(pages...)
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := reindexer(newPipelineWith(&fakeEngine{}, sc, nil, nil), st)
	var backoffs int
	r.sleep = func(ctx context.Context, d time.Duration) error {
		if d == r.Backoff {
			backoffs++
			// The lock pins a pool connection: never hold it through a
			// backoff, which can go on as long as the outage does.
			if st.holding() {
				t.Error("the job held its lock through the backoff")
			}
			cancel() // stop the test at the first backoff
		}
		return ctx.Err()
	}
	_ = r.Run(ctx)
	if backoffs != 1 {
		t.Errorf("backoffs = %d, want the job to back off", backoffs)
	}
	if sc.n() != outageStreak {
		t.Errorf("extractions = %d, want the pass to stop after %d failures in a row", sc.n(), outageStreak)
	}
	for id, n := range st.attempts {
		if n != 0 {
			t.Errorf("%s: %d attempt(s) counted during an outage", id, n)
		}
	}
	if st.finished {
		t.Error("the job marked itself done during an outage")
	}
}

// Research evidence and pages deleted since the listing are skipped,
// not extracted, and don't hold the job open.
func TestReindex_SkipsResearchAndGonePages(t *testing.T) {
	sc := &fakeSidecar{}
	research := page("r1", "Raw web scratch from a research run, not knowledge.")
	research.BookSlug = "research:operator"
	st := newFakeReindexStore(research, page("gone", "deleted meanwhile"))
	delete(st.pages, "gone")
	r, _ := reindexer(newPipelineWith(&fakeEngine{}, sc, nil, nil), st)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sc.n() != 0 || !st.finished {
		t.Errorf("extractions=%d finished=%v, want none and finished", sc.n(), st.finished)
	}
}

// A save made while the job extracts a page wins: the job's result,
// from older content, is discarded, and the save's own run commits.
func TestReindex_LiveSaveWins(t *testing.T) {
	eng := &fakeEngine{}
	sc := &fakeSidecar{gate: make(chan struct{}), started: make(chan string, 4)}
	p := debounced(eng, sc, nil)
	st := newFakeReindexStore(page("page-uuid", "The dentist appointment is on Tuesday at 3pm."))
	r, _ := reindexer(p, st)
	done := make(chan error)
	go func() { done <- r.Run(context.Background()) }()

	<-sc.started // the job is extracting the old content
	p.PageSaved(saveWith("The dentist appointment is on Thursday at 3pm"))
	close(sc.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the save's run", func() bool { return len(eng.calls()) == 1 && idle(p) })
	if got := eng.calls()[0].facts[0].Content; !strings.Contains(got, "Thursday") {
		t.Errorf("committed %q; the job's older content overwrote a live save", got)
	}
}

// A page with a save pending is left to that save's run.
func TestReingest_LeavesPendingSaveToItsRun(t *testing.T) {
	sc := &fakeSidecar{}
	p := debounced(&fakeEngine{}, sc, nil)
	p.deps.Debounce = time.Hour
	p.PageSaved(saveWith("The dentist appointment is on Thursday at 3pm"))
	loads := 0
	out, err := p.Reingest(context.Background(), "page-uuid", func(context.Context) (SaveEvent, bool, error) {
		loads++
		return saveWith("older"), true, nil
	})
	if err != nil || out != Superseded || loads != 0 || sc.n() != 0 {
		t.Errorf("outcome=%v err=%v loads=%d extractions=%d, want Superseded with nothing read or run", out, err, loads, sc.n())
	}
}
