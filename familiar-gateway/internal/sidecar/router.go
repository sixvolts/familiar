package sidecar

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HTTPRouter talks to a local llama-server (Gemma 4 26B-A4B) over the
// OpenAI-compatible /v1/chat/completions endpoint. Used by the sidecar
// Client for classification, summarization, fact extraction, etc.
type HTTPRouter struct {
	endpoint string // e.g., "http://127.0.0.1:8200"
	client   *http.Client
}

// NewHTTPRouter creates a router that talks to a local llama-server
// with the default 10s request ceiling — right for the small,
// fast tasks that dominate sidecar traffic.
func NewHTTPRouter(endpoint string) *HTTPRouter {
	return NewHTTPRouterWithTimeout(endpoint, 10*time.Second)
}

// NewHTTPRouterWithTimeout is NewHTTPRouter with an explicit request
// ceiling. The large-document extract route uses a generous one: a big
// model reading a multi-KB note in one pass runs far past 10s, and
// http.Client.Timeout is a hard ceiling that a longer context deadline
// can't lift.
func NewHTTPRouterWithTimeout(endpoint string, timeout time.Duration) *HTTPRouter {
	return &HTTPRouter{
		endpoint: strings.TrimRight(endpoint, "/"),
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// HealthCheck verifies the sidecar llama-server is reachable.
func (r *HTTPRouter) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sidecar health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// truncate is shared across sidecar files (summarize.go) for log/error formatting.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
