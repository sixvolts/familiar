package brave

// Covers brave.Context against the real payload shape: grounding keyed by
// arbitrary category names, duplicate snippets across categories, and the
// error codes worth distinguishing. Written because the endpoint was added
// from a reference implementation rather than from live traffic, so the
// parsing is the part most likely to be subtly wrong.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestContext_ParsesGroundingAndDedupes(t *testing.T) {
	var gotQuery, gotCount, gotFresh, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		gotFresh = r.URL.Query().Get("freshness")
		gotToken = r.Header.Get("X-Subscription-Token")
		// Two arbitrary category keys, and a snippet repeated across them —
		// both behaviours the real API exhibits.
		_, _ = w.Write([]byte(`{"grounding":{
			"web":[{"title":"Alpha","url":"https://a.example","snippets":["one","one","two"]}],
			"news":[{"title":"Beta","url":"https://b.example","snippets":["three"]}]
		}}`))
	}))
	defer srv.Close()

	c := New("test-key", 5)
	c.SetContextURL(srv.URL)

	out, err := c.Context(context.Background(), "widgets", 4, "pw")
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if gotQuery != "widgets" || gotCount != "4" || gotFresh != "pw" {
		t.Errorf("request params: q=%q count=%q freshness=%q", gotQuery, gotCount, gotFresh)
	}
	if gotToken != "test-key" {
		t.Errorf("token not sent, got %q", gotToken)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 sources across both categories, got %d", len(out))
	}
	// Find Alpha regardless of map iteration order.
	var alpha *ContextSource
	for i := range out {
		if out[i].Title == "Alpha" {
			alpha = &out[i]
		}
	}
	if alpha == nil {
		t.Fatalf("Alpha missing from %+v", out)
	}
	if len(alpha.Snippets) != 2 {
		t.Errorf("duplicate snippet not deduped: %+v", alpha.Snippets)
	}
}

func TestContext_ClampsCountAndRequiresQuery(t *testing.T) {
	var gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCount = r.URL.Query().Get("count")
		_, _ = w.Write([]byte(`{"grounding":{}}`))
	}))
	defer srv.Close()

	c := New("k", 5)
	c.SetContextURL(srv.URL)

	if _, err := c.Context(context.Background(), "", 3, ""); err == nil {
		t.Error("empty query should error")
	}
	if _, err := c.Context(context.Background(), "q", 99, ""); err != nil {
		t.Fatalf("Context: %v", err)
	}
	if gotCount != "10" {
		t.Errorf("count should clamp to the endpoint max of 10, got %q", gotCount)
	}
}

func TestContext_ErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{http.StatusUnauthorized, "token rejected"},
		{http.StatusUnprocessableEntity, "unprocessable"},
		{http.StatusTooManyRequests, "rate limited"},
		{http.StatusInternalServerError, "HTTP 500"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte("nope"))
		}))
		c := New("k", 5)
		c.SetContextURL(srv.URL)
		_, err := c.Context(context.Background(), "q", 3, "")
		if err == nil {
			t.Errorf("status %d: expected an error", tc.code)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: error %q should mention %q", tc.code, err, tc.want)
		}
		srv.Close()
	}
}

// Sources come out in a fixed order, ranked ("generic") pages first, and
// a page listed in two categories is one source with merged snippets.
// The categories were flattened in random map order before the count
// cut, so which sources survived changed from call to call.
func TestContext_OrderIsFixedAndPagesMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"grounding":{
			"map":[{"title":"Map pin","url":"https://m.example","snippets":["m"]}],
			"news":[{"title":"Alpha again","url":"https://a.example","snippets":["one","three"]}],
			"aaa":[{"title":"Delta","url":"https://d.example","snippets":["d"]}],
			"generic":[{"title":"Alpha","url":"https://a.example","snippets":["one","two"]},
			           {"title":"Gamma","url":"https://g.example","snippets":["g"]}]
		}}`))
	}))
	defer srv.Close()
	c := New("k", 5)
	c.SetContextURL(srv.URL)
	for i := 0; i < 20; i++ {
		out, err := c.Context(context.Background(), "q", 4, "")
		if err != nil {
			t.Fatalf("Context: %v", err)
		}
		var urls []string
		for _, o := range out {
			urls = append(urls, o.URL)
		}
		if strings.Join(urls, " ") != "https://a.example https://g.example https://d.example https://m.example" {
			t.Fatalf("sources = %v, want generic first, then the other categories by name", urls)
		}
		if strings.Join(out[0].Snippets, ",") != "one,two,three" {
			t.Fatalf("merged snippets = %v", out[0].Snippets)
		}
	}
}

// Context has its own, longer client timeout than Search's: a slower
// page-body response failed with "Client.Timeout exceeded" even though
// the tool call had time left.
func TestContext_OutlivesTheSearchTimeout(t *testing.T) {
	oldS, oldC := searchTimeout, contextTimeout
	searchTimeout, contextTimeout = 50*time.Millisecond, 2*time.Second
	defer func() { searchTimeout, contextTimeout = oldS, oldC }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"grounding":{"generic":[{"title":"A","url":"https://a.example","snippets":["x"]}]},"web":{"results":[]}}`))
	}))
	defer srv.Close()
	c := New("k", 5)
	c.SetContextURL(srv.URL)
	c.SetBaseURL(srv.URL)
	if _, err := c.Context(context.Background(), "q", 1, ""); err != nil {
		t.Errorf("Context cut by the search timeout: %v", err)
	}
	if _, err := c.Search(context.Background(), "q"); err == nil {
		t.Error("Search outlived its own (shortened) timeout; the test isn't measuring anything")
	}
}
