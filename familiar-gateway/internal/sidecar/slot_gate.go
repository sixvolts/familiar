package sidecar

// CHAT-REARCH §"Smaller Hardening" — slot priority gate.
//
// The small sidecar slot handles both sync pre-turn work (classifier,
// query expansion) and async post-turn work (fact extraction). Under
// rapid-follow-up load, async work from turn N can still be running
// when a classifier call for turn N+1 needs to fly. Plain FIFO at the
// upstream slot means the user's next response waits behind async
// extraction.
//
// True preemption isn't feasible at the HTTP layer — we can't cancel
// a partially-generated llama-server response cleanly. The next best
// thing: rate-limit async to one request in flight per slot, AND
// have async yield briefly to any sync request that appears while
// it's waiting. Sync requests bypass the gate entirely.
//
// Operators with hardware to spare can sidestep the contention
// entirely by configuring a separate small_async slot (CHAT-REARCH
// §"Concurrency Concern"). The gate is the cheap fallback for
// single-instance deployments.

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
