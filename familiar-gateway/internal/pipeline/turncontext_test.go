package pipeline

import (
	"context"
	"testing"
	"time"
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
