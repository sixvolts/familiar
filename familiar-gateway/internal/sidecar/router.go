package sidecar

import (
	"net/http"
	"strings"
	"time"
)

// HTTPRouter talks to one sidecar task's model server over the
// OpenAI-compatible /v1/chat/completions endpoint. The Client builds one
// per endpoint and model for classification, summarization, fact
// extraction, etc.
type HTTPRouter struct {
	endpoint string // e.g., "http://127.0.0.1:8200"
	// model is the name requests send as `model`: the resolved model's
	// configured name (config.ModelConfig.RequestModel). It used to be a
	// hardcoded "gemma-4-26b-a4b", which llama-server ignores but which
	// sent a failover to an ollama or vLLM backup after a model that
	// isn't there.
	model  string
	client *http.Client
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

// truncate is shared across sidecar files (summarize.go) for log/error formatting.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
