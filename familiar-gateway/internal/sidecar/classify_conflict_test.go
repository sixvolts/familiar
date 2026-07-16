package sidecar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeChatServer spins up an httptest server that responds to
// /v1/chat/completions with a preset content string, and captures the
// last request body so assertions can verify the classifier prompt was
// shaped correctly.
func fakeChatServer(t *testing.T, responseContent string) (*httptest.Server, *capturedChatReq) {
	t.Helper()
	captured := &capturedChatReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, captured)
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": responseContent}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

type capturedChatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
}

func TestClassifyConflict_Labels(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"bare update", "UPDATE", ConflictUPDATE, false},
		{"bare duplicate", "DUPLICATE", ConflictDUPLICATE, false},
		{"bare contradicts", "CONTRADICTS", ConflictCONTRADICTS, false},
		{"bare add", "ADD", ConflictADD, false},
		{"lowercase update", "update", ConflictUPDATE, false},
		{"trailing period", "UPDATE.", ConflictUPDATE, false},
		{"leading whitespace", "  \nUPDATE\n", ConflictUPDATE, false},
		{"explained with label first", "UPDATE because the version bumped", ConflictUPDATE, false},
		{"unrecognised", "maybe?", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeChatServer(t, tc.raw)
			r := NewHTTPRouter(srv.URL)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			got, err := r.ClassifyConflict(ctx, "Gpu-host runs Ubuntu 22.04", "Gpu-host upgraded to Ubuntu 24.04")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyConflict_PromptShape(t *testing.T) {
	srv, captured := fakeChatServer(t, "UPDATE")
	r := NewHTTPRouter(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := r.ClassifyConflict(ctx, "existing fact content", "incoming fact content"); err != nil {
		t.Fatalf("classify: %v", err)
	}

	if len(captured.Messages) != 2 {
		t.Fatalf("want 2 messages (system+user), got %d", len(captured.Messages))
	}
	if captured.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", captured.Messages[0].Role)
	}
	user := captured.Messages[1].Content
	if !strings.Contains(user, "existing fact content") {
		t.Errorf("user prompt missing existing fact: %q", user)
	}
	if !strings.Contains(user, "incoming fact content") {
		t.Errorf("user prompt missing incoming fact: %q", user)
	}
	if captured.Temperature != 0.0 {
		t.Errorf("temperature = %v, want 0.0 for deterministic classification", captured.Temperature)
	}
	if captured.MaxTokens > 32 {
		t.Errorf("max_tokens = %d, expected a tight cap (<=32)", captured.MaxTokens)
	}
}

func TestClassifyConflict_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := NewHTTPRouter(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.ClassifyConflict(ctx, "a", "b")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}
