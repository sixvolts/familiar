package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddingsProviderEmbed(t *testing.T) {
	var gotPath, gotModel, gotInput string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel, gotInput = req.Model, req.Input
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	p := NewEmbeddingsProvider("embeddings/test", srv.URL, "", "nomic-embed-text", 768, 0)
	vec, err := p.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Fatalf("unexpected vector: %v", vec)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("posted to %q, want /v1/embeddings", gotPath)
	}
	if gotModel != "nomic-embed-text" {
		t.Errorf("model = %q, want nomic-embed-text", gotModel)
	}
	// The nomic prefix must be preserved verbatim — every vector already
	// stored in an existing deployment was embedded with it.
	if gotInput != "search_query: hello" {
		t.Errorf("input = %q, want the search_query: prefix", gotInput)
	}
	if p.Dimension() != 768 {
		t.Errorf("Dimension() = %d, want 768", p.Dimension())
	}
}

func TestEmbeddingsProviderAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"model not loaded"}}`))
	}))
	defer srv.Close()

	p := NewEmbeddingsProvider("embeddings/test", srv.URL, "", "m", 0, 0)
	_, err := p.Embed(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "model not loaded") {
		t.Fatalf("want the API error surfaced, got %v", err)
	}
}

func TestEmbeddingsProviderEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := NewEmbeddingsProvider("embeddings/test", srv.URL, "", "m", 0, 0)
	if _, err := p.Embed(context.Background(), "x"); err == nil {
		t.Fatal("an empty data array should be an error, not a nil vector")
	}
}

func TestEmbeddingsProviderHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("health probed %q, want /v1/models", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewEmbeddingsProvider("embeddings/test", srv.URL, "", "m", 0, 0)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}

func TestEmbeddingsProviderHealthCheckFailures(t *testing.T) {
	t.Run("unauthorized", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		p := NewEmbeddingsProvider("embeddings/test", srv.URL, "k", "m", 0, 0)
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Fatal("401 should fail the health check")
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		p := NewEmbeddingsProvider("embeddings/test", srv.URL, "", "m", 0, 0)
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Fatal("5xx should fail the health check")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		p := NewEmbeddingsProvider("embeddings/test", "http://127.0.0.1:1", "", "m", 0, 0)
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Fatal("an unreachable endpoint should fail the health check")
		}
	})
}

// An embeddings endpoint can't generate text; routing chat at one must
// fail loudly rather than returning something empty.
func TestEmbeddingsProviderRejectsGeneration(t *testing.T) {
	p := NewEmbeddingsProvider("embeddings/test", "http://example.invalid", "", "m", 0, 0)
	if _, err := p.Complete(context.Background(), CompletionRequest{}); err == nil {
		t.Error("Complete should error on an embeddings provider")
	}
	if _, err := p.CompleteStream(context.Background(), CompletionRequest{}, func(string) {}); err == nil {
		t.Error("CompleteStream should error on an embeddings provider")
	}
}

// It must satisfy Provider so the registry can health-check it alongside
// the chat models.
func TestEmbeddingsProviderSatisfiesProvider(t *testing.T) {
	var _ Provider = (*EmbeddingsProvider)(nil)
}

func TestEmbeddingsProviderDefaultModel(t *testing.T) {
	p := NewEmbeddingsProvider("embeddings/test", "http://x", "", "", 0, 0)
	if p.Model() != "nomic-embed-text" {
		t.Fatalf("blank model should default to nomic-embed-text, got %q", p.Model())
	}
}

// An error the server answered with is an *EmbedAPIError, and it says
// whether the input was refused (skip this input) or the server is
// busy or unreachable upstream (wait it out). A non-JSON error body
// used to surface as a JSON parse failure, indistinguishable from a
// broken server.
func TestEmbeddingsProviderClassifiesErrors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		rejected bool
		msg      string
	}{
		{"llama.cpp too large", 500, `{"error":{"message":"input is too large to process"}}`, true, "input is too large"},
		{"bad request, plain body", 400, "context length exceeded", true, "context length exceeded"},
		{"model loading", 503, "Loading model", false, "Loading model"},
		{"rate limited", 429, `{"error":{"message":"slow down"}}`, false, "slow down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := NewEmbeddingsProvider("e", srv.URL, "", "m", 3, 0)
			_, err := p.Embed(context.Background(), "x")
			var apiErr *EmbedAPIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want an *EmbedAPIError", err)
			}
			if apiErr.Status != tc.status || !strings.Contains(apiErr.Message, tc.msg) {
				t.Errorf("got status %d message %q", apiErr.Status, apiErr.Message)
			}
			if apiErr.InputRejected() != tc.rejected {
				t.Errorf("InputRejected() = %v, want %v", apiErr.InputRejected(), tc.rejected)
			}
		})
	}
}
