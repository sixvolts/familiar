package brave

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTruncateBytes(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		max  int
		want string
	}{
		{"short", []byte("abc"), 10, "abc"},
		{"exact", []byte("hello"), 5, "hello"},
		{"truncated", []byte("0123456789"), 5, "01234..."},
		{"empty", []byte{}, 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateBytes(tc.in, tc.max); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientNew_CapsMaxResults(t *testing.T) {
	// 0 / negative → 3 (default), > 10 → 10.
	cases := []struct {
		in, want int
	}{
		{0, 3},
		{-5, 3},
		{1, 1},
		{10, 10},
		{50, 10},
	}
	for _, tc := range cases {
		c := New("key", tc.in)
		if c.maxResults != tc.want {
			t.Errorf("New(%d).maxResults = %d, want %d", tc.in, c.maxResults, tc.want)
		}
	}
}

func TestClientSearch_ParsesResults(t *testing.T) {
	var gotQuery, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotKey = r.Header.Get("X-Subscription-Token")
		_, _ = w.Write([]byte(`{
			"web": {
				"results": [
					{"title": "Go", "url": "https://go.dev", "description": "Go language", "age": "1 day ago"},
					{"title": "Rust", "url": "https://rust-lang.org", "description": "Rust language", "age": ""}
				]
			}
		}`))
	}))
	defer srv.Close()

	c := New("test-key", 5)
	c.SetBaseURL(srv.URL)

	results, err := c.Search(context.Background(), "systems languages")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "systems languages" {
		t.Errorf("q = %q", gotQuery)
	}
	if gotKey != "test-key" {
		t.Errorf("X-Subscription-Token = %q", gotKey)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Title != "Go" || results[0].URL != "https://go.dev" {
		t.Errorf("first result: %+v", results[0])
	}
	if results[0].Age != "1 day ago" {
		t.Errorf("age dropped: %+v", results[0])
	}
}

func TestClientSearch_HTTPErrorWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	c := New("key", 3)
	c.SetBaseURL(srv.URL)
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestClientSearch_RequestsExtraSnippets(t *testing.T) {
	// Regression guard: the request must include extra_snippets=true
	// so Brave returns the longer passages alongside the primary
	// description. Without this the LLM only sees one-line teasers.
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	c := New("k", 3)
	c.SetBaseURL(srv.URL)
	if _, err := c.Search(context.Background(), "anything"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(gotURL, "extra_snippets=true") {
		t.Errorf("URL missing extra_snippets=true: %s", gotURL)
	}
	if !strings.Contains(gotURL, "result_filter=web") {
		t.Errorf("URL missing result_filter=web: %s", gotURL)
	}
}

func TestClientSearch_ParsesExtraSnippets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"Go","url":"https://go.dev","description":"primary",
			 "extra_snippets":["passage one","passage two","passage three"]}
		]}}`))
	}))
	defer srv.Close()

	c := New("k", 3)
	c.SetBaseURL(srv.URL)
	results, err := c.Search(context.Background(), "go")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	got := results[0].ExtraSnippets
	if len(got) != 3 {
		t.Fatalf("extra_snippets = %d, want 3 (got %v)", len(got), got)
	}
	if got[0] != "passage one" || got[2] != "passage three" {
		t.Errorf("extra_snippets unmarshalled wrong: %v", got)
	}
}

func TestClientSearch_EmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"web": {"results": []}}`))
	}))
	defer srv.Close()

	c := New("key", 3)
	c.SetBaseURL(srv.URL)
	results, err := c.Search(context.Background(), "nothing")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}
