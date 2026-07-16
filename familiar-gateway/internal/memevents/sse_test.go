package memevents

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServeSSEEmitsAndReplays(t *testing.T) {
	b := NewBus(8, nil)

	// Land a few events before the client connects so the replay
	// path has something to surface.
	b.Emit("s1", KindCompactionStarted, CompactionStartedPayload{TurnCount: 8})
	b.Emit("s1", KindFactExtracted, FactExtractedPayload{FactID: "f-1", Content: "hi"})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /events/{session_id}", b.ServeSSE)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events/s1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}

	// Read until we see both replayed events, then close to unblock
	// the server's range over the subscription channel. The handler
	// keeps the connection open indefinitely (it's an SSE stream),
	// so we tear down by closing the body once we have what we want.
	type readResult struct {
		buf []byte
		err error
	}
	results := make(chan readResult, 1)
	go func() {
		out := make([]byte, 0, 4096)
		buf := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				out = append(out, buf[:n]...)
			}
			if strings.Contains(string(out), "FactExtracted") &&
				strings.Contains(string(out), "CompactionStarted") {
				results <- readResult{buf: out}
				return
			}
			if err != nil {
				results <- readResult{buf: out, err: err}
				return
			}
		}
	}()

	var got []byte
	select {
	case r := <-results:
		got = r.buf
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE events")
	}

	body := string(got)
	for _, want := range []string{
		"event: CompactionStarted",
		"event: FactExtracted",
		"\"fact_id\":\"f-1\"",
		"id: 1",
		"id: 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestServeSSEMissingSessionID(t *testing.T) {
	b := NewBus(0, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events/{session_id}", b.ServeSSE)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing session_id, got %d", resp.StatusCode)
	}
}
