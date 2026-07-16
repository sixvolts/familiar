// Package rerank is a thin client for a cross-encoder reranking
// service — a dedicated model (BGE-reranker, mxbai-rerank, …) served
// over an HTTP /rerank endpoint, Jina/Cohere-compatible (which is
// what llama.cpp's --reranking server mode exposes).
//
// A reranker scores (query, document) pairs jointly rather than
// comparing two independently-computed embeddings. It's the precision
// stage of retrieval: hybrid search casts a wide net (top ~50),
// rerank trims it to the few genuinely relevant memories that earn a
// slot in the prompt. See the chat-turn context review §5.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Client talks to one reranker endpoint. A nil *Client is safe: every
// method degrades to "reranking unavailable" so callers can treat the
// reranker as an optional precision upgrade.
type Client struct {
	endpoint string
	model    string
	http     *http.Client
}

// Scored is one document's rerank verdict: its index into the input
// slice and the cross-encoder relevance score (higher = better).
type Scored struct {
	Index int
	Score float64
}

// New builds a reranker client. endpoint is the base URL of the
// reranking server (e.g. "http://127.0.0.1:8300"); model is the
// served model name. Returns nil when endpoint is empty so callers
// can unconditionally construct and nil-check.
func New(endpoint, model string) *Client {
	if endpoint == "" {
		return nil
	}
	return &Client{
		endpoint: endpoint,
		model:    model,
		http:     &http.Client{Timeout: 5 * time.Second},
	}
}

// Available reports whether the client is wired (non-nil + endpoint set).
func (c *Client) Available() bool {
	return c != nil && c.endpoint != ""
}

type rerankRequest struct {
	Model     string   `json:"model,omitempty"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Rerank scores docs against query and returns them ordered by
// descending relevance. The returned slice has one Scored per input
// document; Index refers back into docs.
//
// On any failure (client nil, transport error, malformed response)
// Rerank returns nil + error — callers fall back to the input order.
// Reranking is a precision optimization, never a hard dependency.
func (c *Client) Rerank(ctx context.Context, query string, docs []string) ([]Scored, error) {
	if !c.Available() {
		return nil, fmt.Errorf("rerank: client not configured")
	}
	if len(docs) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(rerankRequest{
		Model:     c.model,
		Query:     query,
		Documents: docs,
	})
	if err != nil {
		return nil, fmt.Errorf("rerank: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rerank: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank: server returned %d", resp.StatusCode)
	}

	var parsed rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("rerank: decode response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("rerank: server error: %s", parsed.Error.Message)
	}
	if len(parsed.Results) == 0 {
		return nil, fmt.Errorf("rerank: empty results")
	}

	out := make([]Scored, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		// Guard against an out-of-range index from a misbehaving
		// server — a bad index would panic the caller's doc lookup.
		if r.Index < 0 || r.Index >= len(docs) {
			continue
		}
		out = append(out, Scored{Index: r.Index, Score: r.RelevanceScore})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("rerank: no valid result indices")
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out, nil
}
