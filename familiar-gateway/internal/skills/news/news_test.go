package news

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/config"
)

const sampleFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
<title>Test Feed</title>
<link>http://example.com</link>
<description>Example test feed</description>
<item>
  <title>First story</title>
  <link>http://example.com/1</link>
  <description>  First   summary with <b>html</b>.  </description>
  <pubDate>Mon, 01 Jan 2024 12:00:00 GMT</pubDate>
</item>
<item>
  <title>Second story</title>
  <link>http://example.com/2</link>
  <description>Second summary</description>
  <pubDate>Tue, 02 Jan 2024 12:00:00 GMT</pubDate>
</item>
</channel>
</rss>`

func newFeedServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
}

func TestGetNews_NoFeeds(t *testing.T) {
	s := New(config.NewsConfig{}, nil)
	params := json.RawMessage(`{"topics":["ai"]}`)
	res, err := s.Execute(context.Background(), "get_news", params)
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == "" {
		t.Error("expected error for empty feed config")
	}
}

func TestGetNews_UnknownTopic(t *testing.T) {
	srv := newFeedServer(t, sampleFeed)
	defer srv.Close()

	s := New(config.NewsConfig{
		Feeds: map[string][]string{"ai": {srv.URL}},
	}, nil)
	params := json.RawMessage(`{"topics":["sports"]}`)
	res, err := s.Execute(context.Background(), "get_news", params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Error, "sports") {
		t.Errorf("expected missing topic error, got %q (content=%q)", res.Error, res.Content)
	}
}

func TestGetNews_FetchesFeed(t *testing.T) {
	srv := newFeedServer(t, sampleFeed)
	defer srv.Close()

	s := New(config.NewsConfig{
		Feeds: map[string][]string{"ai": {srv.URL}},
	}, nil)

	params := json.RawMessage(`{"topics":["ai"],"max_items":5}`)
	res, err := s.Execute(context.Background(), "get_news", params)
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "First story") || !strings.Contains(res.Content, "Second story") {
		t.Errorf("content missing stories: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[ai]") {
		t.Errorf("content missing topic tag: %s", res.Content)
	}

	// Newest-first ordering: Second story (Jan 2) should precede First story (Jan 1).
	firstIdx := strings.Index(res.Content, "First story")
	secondIdx := strings.Index(res.Content, "Second story")
	if secondIdx > firstIdx {
		t.Errorf("expected newest-first ordering; got:\n%s", res.Content)
	}

	var articles []Article
	if err := json.Unmarshal(res.Data, &articles); err != nil {
		t.Fatal(err)
	}
	if len(articles) != 2 {
		t.Fatalf("expected 2 articles, got %d", len(articles))
	}
	if articles[0].Topic != "ai" {
		t.Errorf("article topic = %q, want ai", articles[0].Topic)
	}
}

func TestGetNews_CacheHit(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(sampleFeed))
	}))
	defer srv.Close()

	s := New(config.NewsConfig{
		Feeds:        map[string][]string{"ai": {srv.URL}},
		CacheMinutes: 60,
	}, nil)

	params := json.RawMessage(`{"topics":["ai"]}`)
	if _, err := s.Execute(context.Background(), "get_news", params); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(context.Background(), "get_news", params); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("expected 1 upstream fetch, got %d", hits)
	}
}

func TestGetNews_FeedFailureIsReported(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()

	s := New(config.NewsConfig{
		Feeds: map[string][]string{"ai": {bad.URL}},
	}, nil)
	params := json.RawMessage(`{"topics":["ai"]}`)
	res, err := s.Execute(context.Background(), "get_news", params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "failed to fetch") {
		t.Errorf("expected failure note in content: %s", res.Content)
	}
}

func TestSearchNews_NoBrave(t *testing.T) {
	s := New(config.NewsConfig{}, nil)
	params := json.RawMessage(`{"query":"ai"}`)
	res, err := s.Execute(context.Background(), "search_news", params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Error, "brave") {
		t.Errorf("expected brave-missing error, got %q", res.Error)
	}
}

func TestTrimSummary(t *testing.T) {
	if got := trimSummary("   hello\n\tworld   "); got != "hello world" {
		t.Errorf("collapse: got %q", got)
	}
	long := strings.Repeat("a", 400)
	if got := trimSummary(long); len(got) != 280 || !strings.HasSuffix(got, "...") {
		t.Errorf("cap: len=%d suffix=%q", len(got), got[len(got)-3:])
	}
}

func TestExecute_InvalidTool(t *testing.T) {
	s := New(config.NewsConfig{}, nil)
	_, err := s.Execute(context.Background(), "nonsense", nil)
	if err == nil {
		t.Error("expected error for unknown tool")
	}
}

func TestExecute_InvalidParams(t *testing.T) {
	s := New(config.NewsConfig{}, nil)
	res, err := s.Execute(context.Background(), "get_news", json.RawMessage(`{"topics": "oops"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == "" {
		t.Error("expected error for malformed topics")
	}
}

func TestTTLCacheExpiry(t *testing.T) {
	c := newTTLCache()
	c.set("k", []Article{{Title: "x"}}, 10*time.Millisecond)
	if _, ok := c.get("k"); !ok {
		t.Fatal("expected cache hit")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.get("k"); ok {
		t.Error("expected cache expiry")
	}
}
