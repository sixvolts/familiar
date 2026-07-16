package sidecar

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestSlotGate_AsyncSerial confirms two concurrent acquireAsync calls
// don't both hold the gate at once.
func TestSlotGate_AsyncSerial(t *testing.T) {
	g := &slotGate{}
	ctx := context.Background()

	var inFlight int
	var max int
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.acquireAsync(ctx); err != nil {
				t.Errorf("acquireAsync: %v", err)
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > max {
				max = inFlight
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			g.releaseAsync()
		}()
	}
	wg.Wait()

	if max != 1 {
		t.Errorf("expected max 1 async holder at a time, got %d", max)
	}
}

// TestSlotGate_AsyncWaitsForSync confirms an async caller blocks while
// a sync request is in flight, then unblocks once sync exits.
func TestSlotGate_AsyncWaitsForSync(t *testing.T) {
	g := &slotGate{}
	ctx := context.Background()

	g.syncEnter()

	asyncDone := make(chan struct{})
	go func() {
		_ = g.acquireAsync(ctx)
		close(asyncDone)
	}()

	select {
	case <-asyncDone:
		t.Fatal("async acquired the gate while sync was in flight")
	case <-time.After(150 * time.Millisecond):
		// good — async is correctly blocked
	}

	g.syncExit()

	select {
	case <-asyncDone:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("async never acquired the gate after sync exited")
	}
	g.releaseAsync()
}

// TestSlotGate_CtxCancellation confirms acquireAsync returns ctx.Err()
// when the caller times out before the gate frees up.
func TestSlotGate_CtxCancellation(t *testing.T) {
	g := &slotGate{}
	g.syncEnter()
	defer g.syncExit()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := g.acquireAsync(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ctx.Err(), got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("acquireAsync took %v — wakeup goroutine should fire faster", elapsed)
	}
}
