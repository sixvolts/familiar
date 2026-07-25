package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EmbeddingsProvider talks to an OpenAI-compatible /v1/embeddings
// endpoint (llama.cpp --embedding, Ollama, a hosted embeddings API).
//
// It exists so the embedder can be an ordinary [[models]] entry with
// provider="embeddings": that puts it in the model registry, which means
// it gets the same heartbeat as every chat model and can therefore
// participate in a [roles.embedder] primary/backup failover chain. That
// matters more here than for any other role — memory storage is fully
// embedding-dependent, and a fact written with no embedding is
// unreachable by semantic search.
//
// Complete / CompleteStream are deliberately unimplemented: an
// embeddings server can't generate text, and a config that routes chat
// at one should fail loudly rather than mysteriously.
type EmbeddingsProvider struct {
	name     string
	endpoint string
	model    string
	apiKey   string
	// dimension is the declared embedding width. Informational here (the
	// server decides the real width); config validation uses it to reject
	// a backup whose vectors wouldn't be comparable to the primary's.
	dimension int
	client    *http.Client
}

// NewEmbeddingsProvider builds a provider for an embeddings endpoint.
// A zero timeout uses the 10s default the pipeline's embedder has always
// applied.
func NewEmbeddingsProvider(name, endpoint, apiKey, model string, dimension int, timeout time.Duration) *EmbeddingsProvider {
	if model == "" {
		model = "nomic-embed-text"
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &EmbeddingsProvider{
		name:      name,
		endpoint:  strings.TrimRight(endpoint, "/"),
		model:     model,
		apiKey:    apiKey,
		dimension: dimension,
		client:    &http.Client{Timeout: timeout},
	}
}

// Name returns the provider identifier.
func (p *EmbeddingsProvider) Name() string { return p.name }

// Dimension returns the declared embedding width (0 when unset).
func (p *EmbeddingsProvider) Dimension() int { return p.dimension }

// Model returns the served model name sent in each request.
func (p *EmbeddingsProvider) Model() string { return p.model }

// HealthCheck verifies the embeddings server is reachable. Uses
// GET /v1/models, which every OpenAI-compatible server (including
// llama.cpp in --embedding mode) answers, so the heartbeat costs no
// inference.
func (p *EmbeddingsProvider) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("building health request: %w", err)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s: unauthorized", p.name)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("%s: health HTTP %d", p.name, resp.StatusCode)
	}
	return nil
}

// embedRequest / embedResponse mirror the OpenAI embeddings wire shape.
type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed returns the dense vector for text.
//
// The "search_query: " prefix is required by nomic-embed-text-v1.5 for
// good retrieval (queries get search_query:, documents search_document:).
// It is applied unconditionally here because that is exactly what the
// pipeline's embedder did before this moved out of pipeline.go — every
// stored vector in existing deployments carries it, so changing the
// prefix now would silently mismatch the whole corpus.
func (p *EmbeddingsProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: p.model, Input: "search_query: " + text})
	if err != nil {
		return nil, fmt.Errorf("marshaling embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building embed request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading embed response: %w", err)
	}

	var er embedResponse
	if err := json.Unmarshal(respBytes, &er); err != nil {
		return nil, fmt.Errorf("parsing embed response: %w", err)
	}
	if er.Error != nil {
		return nil, fmt.Errorf("embed API error: %s", er.Error.Message)
	}
	if len(er.Data) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	return er.Data[0].Embedding, nil
}

// errNotAChatModel is returned by the generation methods — an
// embeddings endpoint cannot produce text.
func (p *EmbeddingsProvider) errNotAChatModel() error {
	return fmt.Errorf("%s: provider=\"embeddings\" serves /v1/embeddings only and cannot generate text; point chat at a chat model", p.name)
}

// Complete is unsupported on an embeddings endpoint.
func (p *EmbeddingsProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	return nil, p.errNotAChatModel()
}

// CompleteStream is unsupported on an embeddings endpoint.
func (p *EmbeddingsProvider) CompleteStream(ctx context.Context, req CompletionRequest, onChunk func(string)) (*CompletionResponse, error) {
	return nil, p.errNotAChatModel()
}
