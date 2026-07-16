package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewBlankEndpointReturnsNil(t *testing.T) {
	if New("", "model") != nil {
		t.Error("New with blank endpoint should return nil")
	}
	if (*Client)(nil).Available() {
		t.Error("nil client must report Available()=false")
	}
}

// Rerank returns results sorted by descending score, with Index
// pointing back into the input docs.
func TestRerankSortsByScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// Server returns out-of-order scores; client must sort.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.2},
				{"index": 1, "relevance_score": 0.9},
				{"index": 2, "relevance_score": 0.5},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "bge-reranker")
	got, err := c.Rerank(context.Background(), "query", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	want := []int{1, 2, 0} // 0.9, 0.5, 0.2
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	for i, idx := range want {
		if got[i].Index != idx {
			t.Errorf("position %d: got index %d, want %d", i, got[i].Index, idx)
		}
	}
}

// An out-of-range index from a misbehaving server must be dropped,
// not returned (a bad index would panic the caller's doc lookup).
func TestRerankDropsOutOfRangeIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.8},
				{"index": 99, "relevance_score": 0.9}, // out of range
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	got, err := c.Rerank(context.Background(), "q", []string{"only-one"})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(got) != 1 || got[0].Index != 0 {
		t.Fatalf("expected just the valid index 0, got %+v", got)
	}
}

func TestRerankEmptyDocsNoError(t *testing.T) {
	c := New("http://unused", "")
	got, err := c.Rerank(context.Background(), "q", nil)
	if err != nil || got != nil {
		t.Errorf("empty docs should be a clean no-op, got %v / %v", got, err)
	}
}

func TestRerankServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if _, err := c.Rerank(context.Background(), "q", []string{"a"}); err == nil {
		t.Error("expected an error on 500 response")
	}
}
