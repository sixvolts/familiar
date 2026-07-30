package safego

import (
	"errors"
	"sync"
	"testing"
)

// The whole point: a panic on a detached goroutine must not reach the
// runtime, because there it kills the process.
func TestRecoverContainsPanic(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer Recover("test worker")
		panic("boom")
	}()
	<-done // reaching here at all means the panic was contained
}

func TestGoContainsPanic(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	Go("test go", func() {
		defer wg.Done()
		panic(errors.New("boom"))
	})
	wg.Wait()
}

// RecoverWith exists because recovery alone leaves callers wedged: a
// research worker must still be marked failed, a backfill must still clear
// its running flag. The callback has to run, and receive the panic value.
func TestRecoverWithRunsCleanupAndPassesValue(t *testing.T) {
	var (
		mu     sync.Mutex
		ran    bool
		gotRec any
		done   = make(chan struct{})
	)
	go func() {
		defer close(done)
		defer RecoverWith("test cleanup", func(rec any) {
			mu.Lock()
			ran, gotRec = true, rec
			mu.Unlock()
		})
		panic("wedged")
	}()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if !ran {
		t.Fatal("cleanup did not run — the caller would stay wedged")
	}
	if gotRec != "wedged" {
		t.Errorf("cleanup got %v, want the recovered value", gotRec)
	}
}

// No panic means no cleanup: it is an error path, not a finaliser.
func TestRecoverWithSkipsCleanupOnSuccess(t *testing.T) {
	ran := false
	func() {
		defer RecoverWith("test", func(any) { ran = true })
	}()
	if ran {
		t.Error("cleanup must not run when nothing panicked")
	}
}

// Do is for ticker loops: it must swallow the panic AND report it, so the
// loop keeps ticking rather than silently retiring the daemon.
func TestDoReportsOutcomeAndKeepsCaller(t *testing.T) {
	if ok := Do("bad tick", func() { panic("boom") }); ok {
		t.Error("Do should report false on a panic")
	}
	if ok := Do("good tick", func() {}); !ok {
		t.Error("Do should report true on success")
	}

	// The loop-survival property, stated as a test: a panicking iteration
	// must not stop later iterations.
	ticks := 0
	for i := 0; i < 5; i++ {
		Do("loop body", func() {
			ticks++
			if i == 2 {
				panic("bad tick 2")
			}
		})
	}
	if ticks != 5 {
		t.Errorf("%d iterations ran, want 5 — one bad tick must not retire the loop", ticks)
	}
}

// A nil cleanup must not itself panic.
func TestRecoverWithNilCleanup(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer RecoverWith("test", nil)
		panic("boom")
	}()
	<-done
}
