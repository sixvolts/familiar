// Package safego contains panics in detached goroutines.
//
// Go's rule is unforgiving: a panic on a goroutine nobody recovers takes
// down the whole process. That is survivable inside an http handler, which
// net/http recovers for you, and it is fatal everywhere else — and this
// gateway does a lot of work everywhere else. Post-turn extraction,
// research workers, scheduled actions, the wiki knowledge hook and every
// skill dispatch all parse or act on model-authored input, which is the
// least trustworthy data in the system, on goroutines that no HTTP server
// is watching. One malformed tool argument reaching an unguarded index
// expression is enough to kill a running gateway and every in-flight
// conversation with it.
//
// This package has no dependencies beyond the standard library on purpose:
// internal/pipeline already had a private version of this, but
// internal/pipeline imports internal/skills, so the packages that most need
// it could not reach it. A leaf package can be imported from anywhere.
//
// A note on where NOT to use Recover: do not put it at the top of a
// long-lived ticker loop's goroutine. Recovering there returns from the
// goroutine, which silently retires the daemon for the rest of the process
// lifetime — a sweep or heartbeat that has quietly stopped is worse than a
// crash, because systemd restarts a crash. Wrap the per-iteration work
// instead, so one bad tick is skipped and the loop keeps ticking.
package safego

import (
	"log"
	"runtime/debug"
)

// Recover contains a panic and logs it with its stack. Use as the first
// statement of a detached goroutine's body:
//
//	go func() {
//	    defer safego.Recover("post-turn extract")
//	    ...
//	}()
//
// label identifies the work in the log line; make it something you would
// want to read at 3am.
//
// MUST be deferred DIRECTLY. recover() only returns non-nil when called by
// the function the runtime deferred, so wrapping this in a helper of your
// own silently stops it working — the helper becomes the deferred function
// and Recover ends up a frame too deep:
//
//	defer safego.Recover("x")               // works
//	defer myWrapper("x")                    // does NOT, if myWrapper calls
//	                                        // safego.Recover internally
//
// If you want a named wrapper (to bake in a label prefix, say), have the
// wrapper call recover() itself. internal/pipeline.recoverBackground is
// that shape, and its comment says why.
func Recover(label string) {
	if r := recover(); r != nil {
		log.Printf("[panic] %s recovered: %v\n%s", label, r, debug.Stack())
	}
}

// RecoverWith is Recover plus a cleanup callback, for goroutines that own
// terminal state.
//
// Recovery alone is not enough when something is waiting on the goroutine
// to record an outcome: a research worker that panics must still be marked
// failed or the run's gap-fill never retries it, and a backfill that panics
// must still clear its running flag or the endpoint returns 409 forever.
// Swallowing the panic without that leaves the process alive and the
// feature wedged, which is a worse failure than the crash.
//
// onPanic runs only on a panic, and receives the recovered value. It runs
// inside the deferred call, so keep it short and make sure it cannot panic
// itself — a panic in onPanic is not recovered.
func RecoverWith(label string, onPanic func(recovered any)) {
	if r := recover(); r != nil {
		log.Printf("[panic] %s recovered: %v\n%s", label, r, debug.Stack())
		if onPanic != nil {
			onPanic(r)
		}
	}
}

// Go runs fn on a new goroutine with panic recovery. Convenience for the
// common shape; use a plain `go func()` with a deferred Recover when the
// body needs a WaitGroup, named returns, or other bookkeeping.
func Go(label string, fn func()) {
	go func() {
		defer Recover(label)
		fn()
	}()
}

// Do runs fn SYNCHRONOUSLY with panic recovery, and reports whether it
// completed without panicking.
//
// This is the form to use inside a long-lived ticker loop:
//
//	for {
//	    select {
//	    case <-ctx.Done():
//	        return
//	    case <-ticker.C:
//	        safego.Do("embedding back-fill sweep", func() { s.RunOnce(ctx) })
//	    }
//	}
//
// Wrapping the per-iteration work rather than the goroutine means one bad
// tick is logged and skipped while the loop keeps running. A recover at the
// goroutine's top would instead return from the loop, silently retiring the
// daemon for the rest of the process lifetime — and a heartbeat or sweep
// that has stopped without saying so is worse than a crash, because a crash
// gets restarted.
func Do(label string, fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[panic] %s recovered (continuing): %v\n%s", label, r, debug.Stack())
			ok = false
		}
	}()
	fn()
	return true
}
