package sidecar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/modelrole"
)

// A summarizer reply that is empty (a reasoning model spent its tokens
// thinking) or a refusal doesn't replace the rolling summary: the caller
// would save it and drop the turns it covered.
func TestSummarize_RejectsEmptyAndRefusals(t *testing.T) {
	for _, bad := range []string{"", "   ", "I'm sorry, but I can't summarize this conversation for you.", "<think>long thoughts</think>"} {
		srv, _ := fakeChatServer(t, bad)
		got, err := NewHTTPRouter(srv.URL).Summarize(context.Background(), "They chose Postgres.", []Turn{{Role: "user", Content: "hi"}})
		if err == nil || got != "They chose Postgres." {
			t.Errorf("reply %q: got (%q, %v), want the previous summary and an error", bad, got, err)
		}
	}
	srv, _ := fakeChatServer(t, "<think>plan</think>They chose Postgres over MySQL and set up nightly backups.")
	got, err := NewHTTPRouter(srv.URL).Summarize(context.Background(), "", []Turn{{Role: "user", Content: "hi"}})
	if err != nil || got != "They chose Postgres over MySQL and set up nightly backups." {
		t.Errorf("got (%q, %v), want the summary without its think block", got, err)
	}
}

// Large-document extraction gets a large output budget, and a reply cut
// off by max_tokens keeps the complete facts before the cut (it used to
// fail to parse and lose every one).
func TestExtractFactsLarge_BudgetAndTruncatedReply(t *testing.T) {
	truncated := `{"facts":[{"content":"server-1 has 6 GPUs","category":"technical_fact"},` +
		`{"content":"server-1 runs Ubuntu","category":"technical_fact"},{"content":"server-1 IP is 10.0`
	srv, captured := fakeChatServer(t, truncated)
	res, err := NewHTTPRouter(srv.URL).ExtractFactsLarge(context.Background(), []Turn{{Role: "user", Content: "doc"}})
	if err != nil {
		t.Fatalf("truncated reply: %v", err)
	}
	if captured.MaxTokens != extractLargeMaxTokens {
		t.Errorf("max_tokens = %d, want %d", captured.MaxTokens, extractLargeMaxTokens)
	}
	if len(res.Facts) != 2 || res.Facts[1].Content != "server-1 runs Ubuntu" {
		t.Errorf("facts = %+v, want the two complete ones", res.Facts)
	}
}

// Background work waits for an in-flight critical-path call on its
// endpoint. Only extraction used to; a summary or batch went straight
// through and held the slot the next turn's classify needed.
func TestBackgroundTasksWaitForCriticalPath(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{
			"content": `{"decisions":[],"relationships":[]}`}}}})
	}))
	defer srv.Close()
	c := newTestClient(
		map[string][]string{TaskClassify: {"m"}, TaskConflict: {"m"}, TaskSummarize: {"m"}},
		map[string]string{"m": modelrole.StatusOnline},
		map[string]string{"m": srv.URL},
	)
	for _, call := range []func(){
		func() { _, _ = c.BatchClassifyAndRelate(context.Background(), BatchExtractInput{}) },
		func() {
			_, _ = c.Summarize(context.Background(), "", []Turn{{Role: "user", Content: strings.Repeat("x", 10)}})
		},
	} {
		hits.Store(0)
		gate := c.gateForTask(TaskClassify)
		gate.syncEnter()
		done := make(chan struct{})
		go func() { call(); close(done) }()
		time.Sleep(150 * time.Millisecond)
		if hits.Load() != 0 {
			t.Error("background call went out while a classify was in flight")
		}
		gate.syncExit()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("background call never ran after the classify finished")
		}
		if hits.Load() != 1 {
			t.Errorf("hits = %d, want 1", hits.Load())
		}
	}
}

// The client's large route sends the large budget (it called the chat
// extraction on the large model).
func TestClientExtractFactsLarge_UsesTheLargeBudget(t *testing.T) {
	srv, captured := fakeChatServer(t, `{"facts":[],"relationships":[]}`)
	c := newTestClient(
		map[string][]string{TaskExtractLarge: {"big"}},
		map[string]string{"big": modelrole.StatusOnline},
		map[string]string{"big": srv.URL},
	)
	if _, err := c.ExtractFactsLarge(context.Background(), []Turn{{Role: "user", Content: "doc"}}); err != nil {
		t.Fatal(err)
	}
	if captured.MaxTokens != extractLargeMaxTokens {
		t.Errorf("max_tokens = %d, want %d", captured.MaxTokens, extractLargeMaxTokens)
	}
}
