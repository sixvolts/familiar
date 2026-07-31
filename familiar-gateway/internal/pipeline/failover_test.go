package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/familiar/gateway/internal/llm"
)

// failoverStubProvider is a minimal llm.Provider for the completion-level
// failover unit tests.
type failoverStubProvider struct {
	content     string // token streamed / returned on success
	err         error  // non-nil → fail
	emitThenErr bool   // stream a token, THEN return err (mid-stream failure)
}

func (s *failoverStubProvider) Name() string                      { return "stub" }
func (s *failoverStubProvider) HealthCheck(context.Context) error { return nil }

func (s *failoverStubProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &llm.CompletionResponse{Content: s.content}, nil
}

func (s *failoverStubProvider) CompleteStream(ctx context.Context, req llm.CompletionRequest, onChunk func(string)) (*llm.CompletionResponse, error) {
	if s.emitThenErr {
		onChunk(s.content)
		return nil, s.err
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.content != "" {
		onChunk(s.content)
	}
	return &llm.CompletionResponse{Content: s.content}, nil
}

// A candidate that errors before its first visible token hands off to the
// next chain member, and the served model is recorded on RouteInfo.
func TestCompleteWithFailover_FailsOverBeforeFirstToken(t *testing.T) {
	p := &Pipeline{}
	cands := []completionCandidate{
		{id: "primary", provider: &failoverStubProvider{err: errors.New("connection reset")}},
		{id: "backup", provider: &failoverStubProvider{content: "hello"}},
	}
	info := &RouteInfo{ModelID: "primary"}
	var got string
	resp, err := p.completeWithFailover(context.Background(), cands, llm.CompletionRequest{},
		func(s string) { got += s }, info)
	if err != nil {
		t.Fatalf("expected failover to succeed, got %v", err)
	}
	if resp == nil || resp.Content != "hello" || got != "hello" {
		t.Fatalf("backup did not serve: resp=%+v streamed=%q", resp, got)
	}
	if info.ModelID != "backup" {
		t.Fatalf("info.ModelID = %q, want backup after failover", info.ModelID)
	}
}

// A candidate that streams a visible token and THEN errors must NOT fail
// over — you can't swap models mid-answer. The error propagates.
func TestCompleteWithFailover_NoFailoverAfterFirstToken(t *testing.T) {
	p := &Pipeline{}
	cands := []completionCandidate{
		{id: "primary", provider: &failoverStubProvider{content: "partial", emitThenErr: true, err: errors.New("mid-stream drop")}},
		{id: "backup", provider: &failoverStubProvider{content: "SHOULD NOT SERVE"}},
	}
	var got string
	_, err := p.completeWithFailover(context.Background(), cands, llm.CompletionRequest{},
		func(s string) { got += s }, &RouteInfo{})
	if err == nil {
		t.Fatal("mid-stream error must propagate, not fail over to the backup")
	}
	if got != "partial" {
		t.Fatalf("expected the partial token to have streamed, got %q", got)
	}
}

// A cancelled parent context is terminal — no failover, even though a
// later candidate would succeed.
func TestCompleteWithFailover_ContextCancelIsTerminal(t *testing.T) {
	p := &Pipeline{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cands := []completionCandidate{
		{id: "primary", provider: &failoverStubProvider{err: context.Canceled}},
		{id: "backup", provider: &failoverStubProvider{content: "SHOULD NOT SERVE"}},
	}
	if _, err := p.completeWithFailover(ctx, cands, llm.CompletionRequest{}, nil, &RouteInfo{}); err == nil {
		t.Fatal("a cancelled context must be terminal, not a failover trigger")
	}
}
