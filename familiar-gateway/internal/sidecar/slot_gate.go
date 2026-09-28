package sidecar

// Slot priority gate, one per sidecar endpoint.
//
// A model server with one slot serves both critical-path work (classify,
// expand_queries: the user is waiting for the first token) and
// background work (post-turn extraction and the conflict/relationship
// batch, summarizing, titles, the memory backfills). Plain FIFO at the
// server means the next turn's classify can wait behind a 1500-token
// batch from the last one.
//
// True preemption isn't feasible at the HTTP layer — we can't cancel a
// partially-generated response cleanly. The next best thing: background
// work runs one request at a time per endpoint (acquireAsync) and never
// starts while a critical-path request is in flight; critical-path work
// only registers itself (syncEnter) and never waits. Tasks on different
// endpoints never contend. Client.background is the one entry point for
// background work.

import (
	"context"
	"sync"
	"time"
)

// slotGate serializes async access to one sidecar slot while letting
// sync access pass through unrestricted. Zero value is a usable gate.
type slotGate struct {
	mu sync.Mutex
	// asyncBusy is true while exactly one async holder owns the gate.
	asyncBusy bool
	// syncInFlight counts sync calls currently in flight. Async
	// callers refuse to start while this is non-zero.
	syncInFlight int
	// cond wakes waiters when state changes. Lazily created by
	// acquireAsync since the zero value is valid otherwise.
	cond *sync.Cond
}

// syncEnter / syncExit bracket a sync request. Cheap counters under a
// short critical section; never block. Always pair them.
func (g *slotGate) syncEnter() {
	g.mu.Lock()
	g.syncInFlight++
	g.mu.Unlock()
}
func (g *slotGate) syncExit() {
	g.mu.Lock()
	g.syncInFlight--
	if g.cond != nil {
		g.cond.Broadcast()
	}
	g.mu.Unlock()
}

// acquireAsync blocks until the gate is free of other async holders
// AND has no sync requests in flight. Honors ctx cancellation. Pair
// every successful acquire with releaseAsync.
//
// The wakeup goroutine is the cleanest sync.Cond/ctx bridge — Cond
// has no native ctx integration, so we periodically Broadcast() to
// re-check ctx.
func (g *slotGate) acquireAsync(ctx context.Context) error {
	g.mu.Lock()
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
	stopWakeup := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWakeup:
				return
			case <-ctx.Done():
				g.mu.Lock()
				g.cond.Broadcast()
				g.mu.Unlock()
				return
			case <-ticker.C:
				g.mu.Lock()
				g.cond.Broadcast()
				g.mu.Unlock()
			}
		}
	}()
	for g.asyncBusy || g.syncInFlight > 0 {
		if ctx.Err() != nil {
			g.mu.Unlock()
			close(stopWakeup)
			return ctx.Err()
		}
		g.cond.Wait()
	}
	g.asyncBusy = true
	g.mu.Unlock()
	close(stopWakeup)
	return nil
}

// releaseAsync gives the gate back. Safe to call after a successful
// acquireAsync; calling it without the gate is a programmer error
// (panic).
func (g *slotGate) releaseAsync() {
	g.mu.Lock()
	if !g.asyncBusy {
		g.mu.Unlock()
		panic("sidecar: releaseAsync called without holding the gate")
	}
	g.asyncBusy = false
	if g.cond != nil {
		g.cond.Broadcast()
	}
	g.mu.Unlock()
}
