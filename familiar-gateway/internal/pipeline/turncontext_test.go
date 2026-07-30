package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/sidecar"
)

type tcKey struct{}

// A turn's generation context must survive the REQUEST context being
// cancelled (an SSE client disconnecting) while still carrying its
// values — that's what lets an abandoned stream finish and persist
// instead of being truncated.
func TestTurnContext_SurvivesClientDisconnect(t *testing.T) {
	p := &Pipeline{}
	reqCtx, reqCancel := context.WithCancel(context.WithValue(context.Background(), tcKey{}, "drew"))

	turnCtx, cancel := p.turnContext(reqCtx, "sess-disconnect")
	defer cancel()

	// Client disconnects.
	reqCancel()

	if err := turnCtx.Err(); err != nil {
		t.Fatalf("turn context was cancelled by the client disconnect: %v", err)
	}
	if v, _ := turnCtx.Value(tcKey{}).(string); v != "drew" {
		t.Errorf("turn context lost request values: got %q, want drew", v)
	}
}

// It must still stop on gateway shutdown (the lifetime context).
func TestTurnContext_YieldsToShutdown(t *testing.T) {
	life, shutdown := context.WithCancel(context.Background())
	p := &Pipeline{}
	p.SetLifetime(life)

	turnCtx, cancel := p.turnContext(context.Background(), "sess-shutdown")
	defer cancel()

	if turnCtx.Err() != nil {
		t.Fatal("turn context cancelled before shutdown")
	}
	shutdown() // gateway going down

	select {
	case <-turnCtx.Done():
		// expected — AfterFunc propagates the shutdown
	case <-time.After(2 * time.Second):
		t.Fatal("turn context did not cancel on shutdown")
	}
}

// With no lifetime wired (tests, odd deploys) it degrades to a
// cap-only bound and still ignores request cancellation.
func TestTurnContext_NoLifetimeStillDetaches(t *testing.T) {
	p := &Pipeline{} // lifetime nil
	reqCtx, reqCancel := context.WithCancel(context.Background())
	turnCtx, cancel := p.turnContext(reqCtx, "sess-nolifetime")
	defer cancel()
	reqCancel()
	if err := turnCtx.Err(); err != nil {
		t.Fatalf("nil-lifetime turn context cancelled by request: %v", err)
	}
}

// StopTurn cuts a registered in-flight turn: the turn's context cancels
// with the errUserStopped cause (so providers salvage the partial), and
// a second stop of the same session — or a stop of an unknown one —
// reports nothing to stop.
func TestStopTurn_CancelsRegisteredTurn(t *testing.T) {
	p := &Pipeline{}
	turnCtx, cancel := p.turnContext(context.Background(), "sess-stop")
	defer cancel()

	if p.StopTurn("sess-other") {
		t.Fatal("StopTurn reported success for an unregistered session")
	}
	if !p.StopTurn("sess-stop") {
		t.Fatal("StopTurn reported nothing to stop for a live turn")
	}
	select {
	case <-turnCtx.Done():
		if got := context.Cause(turnCtx); got != errUserStopped {
			t.Fatalf("cancel cause = %v, want errUserStopped", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StopTurn did not cancel the turn context")
	}
}

// A turn's teardown deregisters it so a later stop of the same session id
// (e.g. the next turn hasn't started, or already finished) is a no-op
// rather than cancelling a stale/absent context.
func TestStopTurn_DeregistersOnTeardown(t *testing.T) {
	p := &Pipeline{}
	_, cancel := p.turnContext(context.Background(), "sess-teardown")
	cancel()
	if p.StopTurn("sess-teardown") {
		t.Fatal("StopTurn succeeded after the turn was torn down")
	}
}

// A turn parked in CLASSIFICATION must be stoppable. Before the turn
// context was created ahead of beginTurn, classification ran outside the
// stop registry entirely, so the Stop button could not cut a turn stuck
// waiting on a hung classifier — the exact case a user would press it in.
//
// Note this test fails if the ordering in handleStream is reverted, which
// is the point: the fix was previously unverified in either direction.
func TestStopTurn_CancelsTurnStuckInClassification(t *testing.T) {
	// A classifier that blocks until the test releases it, so the turn is
	// guaranteed to be inside classification when Stop arrives.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	clf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done(): // cancelled by the Stop we are testing
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"thinking\":\"low\",\"memory_depth\":\"none\",\"search_depth\":\"none\"}"}}]}`))
	}))
	defer clf.Close()
	defer close(release)

	srv := fakeOpenAIServer("done")
	defer srv.Close()

	pl := makePipeline(&mockEngine{}, srv)
	routes := classifyOnlyRoutes{endpoint: clf.URL}
	pl.sidecarClient = sidecar.NewClient(
		config.SidecarConfig{Enabled: true, RequestTimeoutMs: 30000}, // long, so Stop is what cuts it
		config.RouterConfig{}, routes, routes)

	sess := pl.sessions.GetOrCreate("cli", "stop-user")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = pl.HandleStream(context.Background(), sess, "hello",
			nil, func(string) {}, nil, nil)
	}()

	// Wait until we are demonstrably inside the classifier call.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("classifier was never reached")
	}

	// The turn must already be registered — this is what the reordering buys.
	if !pl.StopTurn(sess.ID) {
		t.Fatal("StopTurn returned false for a turn parked in classification: " +
			"the turn context is not registered until after beginTurn")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn did not unwind after StopTurn")
	}
}

// A client disconnect during classification must also cut it: nothing has
// been produced yet, so there is no answer worth finishing. This is the
// half of prepContext that the turn context alone would not give us —
// turnContext is deliberately detached from the request.
//
// The classifier handler here blocks until the test releases it and never
// watches its own request context, so the ONLY thing that can free the
// client is prepContext honouring reqCtx. If it does not, the call sits
// there until the sidecar timeout and this test fails on the deadline.
func TestPrepContext_ClientDisconnectCancelsClassification(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	clf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		once.Do(func() { close(entered) })
		<-release // deliberately NOT watching r.Context()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"thinking\":\"low\",\"memory_depth\":\"none\",\"search_depth\":\"none\"}"}}]}`))
	}))
	// LIFO: release the handler before Close waits on it.
	defer clf.Close()
	defer close(release)

	srv := fakeOpenAIServer("done")
	defer srv.Close()

	pl := makePipeline(&mockEngine{}, srv)
	routes := classifyOnlyRoutes{endpoint: clf.URL}
	pl.sidecarClient = sidecar.NewClient(
		// Long enough that the sidecar deadline is not what frees us.
		config.SidecarConfig{Enabled: true, RequestTimeoutMs: 30000},
		config.RouterConfig{}, routes, routes)

	sess := pl.sessions.GetOrCreate("cli", "disc-user")
	reqCtx, disconnect := context.WithCancel(context.Background())

	classifyReturned := make(chan struct{})
	go func() {
		defer close(classifyReturned)
		_, _, _ = pl.HandleStream(reqCtx, sess, "hello", nil, func(string) {}, nil, nil)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("classifier was never reached")
	}
	disconnect()

	// The turn itself is detached and still finishes (that is by design),
	// but it can only get past classification if the disconnect cut the
	// call — the handler is still parked.
	select {
	case <-classifyReturned:
	case <-time.After(8 * time.Second):
		t.Fatal("classification did not unwind on client disconnect — " +
			"prepContext is not honouring the request context")
	}
}
