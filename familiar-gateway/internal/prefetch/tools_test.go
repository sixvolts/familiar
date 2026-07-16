package prefetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/familiar/gateway/internal/brave"
)

// --- formatSearchResults (pure) --------------------------------------------

func TestFormatSearchResults_TruncatesDescriptions(t *testing.T) {
	long := strings.Repeat("x", 500)
	results := []brave.SearchResult{
		{Title: "A", URL: "https://a", Description: long, Age: "1d"},
	}
	got := formatSearchResults(results, 100)
	// Must truncate to ~100 chars of description, appending "..."
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncation marker, got: %s", got)
	}
	// Full 500x description must NOT appear.
	if strings.Contains(got, long) {
		t.Error("long description not truncated")
	}
	if !strings.Contains(got, "[A](https://a)") {
		t.Errorf("expected markdown link, got: %s", got)
	}
	if !strings.Contains(got, "(1d)") {
		t.Errorf("expected age suffix, got: %s", got)
	}
}

func TestFormatSearchResults_OmitsAgeWhenEmpty(t *testing.T) {
	got := formatSearchResults([]brave.SearchResult{
		{Title: "T", URL: "https://u", Description: "d", Age: ""},
	}, 300)
	// Should NOT contain "( )" (empty age parenthesized)
	if strings.Contains(got, "( )") || strings.Contains(got, "()") {
		t.Errorf("empty age should be omitted, got: %s", got)
	}
}

func TestFormatSearchResults_ZeroDescLenKeepsFull(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := formatSearchResults([]brave.SearchResult{{Title: "T", URL: "u", Description: long}}, 0)
	if !strings.Contains(got, long) {
		t.Error("descLen=0 should mean no truncation")
	}
}

// --- Orchestrator.Execute with a stub Brave endpoint -----------------------

func TestOrchestrator_NoToolsNeededReturnsEmpty(t *testing.T) {
	o := NewOrchestrator(nil)
	out := o.Execute(context.Background(), nil, nil, "knowledge")
	if out.Context != "" || out.ResultCount != 0 {
		t.Errorf("expected empty result, got %+v", out)
	}
}

func TestOrchestrator_NilBraveReturnsEmpty(t *testing.T) {
	// web_search requested but no brave client configured.
	o := NewOrchestrator(nil)
	out := o.Execute(context.Background(), []string{"web_search"}, []string{"q"}, "knowledge")
	if out.Context != "" || out.ResultCount != 0 {
		t.Errorf("expected empty result without client, got %+v", out)
	}
}

func TestOrchestrator_UnknownToolIgnored(t *testing.T) {
	o := NewOrchestrator(nil)
	out := o.Execute(context.Background(), []string{"tarot_reading"}, nil, "knowledge")
	if out.Context != "" || out.ResultCount != 0 {
		t.Errorf("unknown tool should be silently ignored, got %+v", out)
	}
}

func TestOrchestrator_WebSearchFormatsAndCaps(t *testing.T) {
	// Two queries → orchestrator fans out concurrently → aggregates →
	// caps to maxTotalResults (3 on knowledge tier).
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{
			"web": {
				"results": [
					{"title": "R1", "url": "https://1", "description": "d1", "age": ""},
					{"title": "R2", "url": "https://2", "description": "d2", "age": ""}
				]
			}
		}`))
	}))
	defer srv.Close()

	bc := brave.New("test", 5)
	bc.SetBaseURL(srv.URL)
	o := NewOrchestrator(bc)

	out := o.Execute(context.Background(),
		[]string{"web_search"},
		[]string{"golang", "rust"},
		"knowledge",
	)
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("expected 2 upstream queries, got %d", hits)
	}
	// 2 queries × 2 results = 4, capped to 3 on knowledge tier.
	if out.ResultCount != 3 {
		t.Errorf("ResultCount = %d, want 3 (knowledge-tier cap)", out.ResultCount)
	}
	if len(out.Queries) != 2 {
		t.Errorf("Queries = %v", out.Queries)
	}
	if !strings.Contains(out.Context, "Web search results") {
		t.Errorf("missing header in Context: %s", out.Context)
	}
}

func TestOrchestrator_DeepTierGetsLargerBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"web": {"results": [
			{"title": "a","url":"u1","description":"d"},
			{"title": "b","url":"u2","description":"d"},
			{"title": "c","url":"u3","description":"d"},
			{"title": "d","url":"u4","description":"d"}
		]}}`))
	}))
	defer srv.Close()

	bc := brave.New("test", 5)
	bc.SetBaseURL(srv.URL)
	o := NewOrchestrator(bc)

	// 2 queries × 4 results = 8, capped to 6 on deep tier.
	out := o.Execute(context.Background(), []string{"web_search"}, []string{"a", "b"}, "deep")
	if out.ResultCount != 6 {
		t.Errorf("deep tier ResultCount = %d, want 6", out.ResultCount)
	}
}

func TestOrchestrator_QueryCapOfThree(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	bc := brave.New("test", 5)
	bc.SetBaseURL(srv.URL)
	o := NewOrchestrator(bc)

	// 5 queries submitted — orchestrator must cap to 3 upstream hits.
	o.Execute(context.Background(), []string{"web_search"},
		[]string{"a", "b", "c", "d", "e"}, "knowledge")

	if h := atomic.LoadInt32(&hits); h != 3 {
		t.Errorf("query cap not enforced: %d upstream hits", h)
	}
}
