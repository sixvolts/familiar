package native

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// A shutdown returns once the in-flight request has finished. Serve
// returns as soon as Shutdown begins, and Run returned then, so the
// process exited mid-request while the drain was still going.
func TestServe_WaitsForInFlightRequests(t *testing.T) {
	var finished atomic.Bool
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, ln, 5*time.Second) }()

	go func() { _, _ = http.Get("http://" + ln.Addr().String() + "/") }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		if !finished.Load() {
			t.Error("serve returned before the in-flight request finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve never returned")
	}
}
