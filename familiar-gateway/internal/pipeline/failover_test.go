package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/llm"
	"github.com/familiar/gateway/internal/router"
)

// failoverStubProvider is a minimal llm.Provider for the completion-level
// failover unit tests.
type failoverStubProvider struct {
	content     string // token streamed / returned on success
	err         error  // non-nil → fail
	emitThenErr bool   // stream a token, THEN return err (mid-stream failure)
	calls       int
	got         llm.CompletionRequest // the last request
}

func (s *failoverStubProvider) Name() string                      { return "stub" }
func (s *failoverStubProvider) HealthCheck(context.Context) error { return nil }

func (s *failoverStubProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	s.calls++
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	return &llm.CompletionResponse{Content: s.content}, nil
}

func (s *failoverStubProvider) CompleteStream(ctx context.Context, req llm.CompletionRequest, onChunk func(string)) (*llm.CompletionResponse, error) {
	s.calls++
	s.got = req
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

// failoverPipeline has a primary with a 262k window and tools, and a
// backup with the given window and capabilities.
func failoverPipeline(backupWindow int, backupCaps ...string) *Pipeline {
	rr := router.NewRegistry([]config.ModelConfig{
		{ID: "primary", Provider: "llama-server", Endpoint: "http://p", ContextWindow: 262144, Capabilities: []string{"tools"}},
		{ID: "backup", Provider: "llama-server", Endpoint: "http://b", ContextWindow: backupWindow, Capabilities: backupCaps},
	})
	return &Pipeline{router: router.NewRouter(config.RouterConfig{Enabled: true}, rr)}
}

func failoverRun(p *Pipeline, req llm.CompletionRequest) (*failoverStubProvider, error) {
	backup := &failoverStubProvider{content: "from backup"}
	cands := []completionCandidate{
		{id: "primary", provider: &failoverStubProvider{err: errors.New("502 bad gateway")}},
		{id: "backup", provider: backup},
	}
	_, err := p.completeWithFailover(context.Background(), cands, req, nil, &RouteInfo{})
	return backup, err
}

// A request packed for the primary's window is not sent to a backup it
// can't fit: the user got the backup's context error instead of the
// primary's retryable one.
func TestCompleteWithFailover_SkipsABackupTheRequestDoesNotFit(t *testing.T) {
	req := llm.CompletionRequest{MaxTokens: 12000, Messages: []llm.Message{{Role: "user", Content: strings.Repeat("x", 4*120000)}}}
	backup, err := failoverRun(failoverPipeline(32768, "tools"), req)
	if backup.calls != 0 {
		t.Errorf("a 120k-token prompt was sent to a 32k backup")
	}
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want the primary's error", err)
	}
}

// When the prompt fits but prompt+MaxTokens doesn't, the backup gets the
// room it has.
func TestCompleteWithFailover_ShrinksMaxTokensToTheBackupsRoom(t *testing.T) {
	req := llm.CompletionRequest{MaxTokens: 12000, Messages: []llm.Message{{Role: "user", Content: strings.Repeat("x", 4*28000)}}}
	backup, err := failoverRun(failoverPipeline(32768, "tools"), req)
	if err != nil || backup.calls != 1 {
		t.Fatalf("backup calls %d, err %v", backup.calls, err)
	}
	if backup.got.MaxTokens != 32768-28000 {
		t.Errorf("MaxTokens = %d, want the %d left", backup.got.MaxTokens, 32768-28000)
	}
}

// A backup without tool support gets no tools; once this turn's tools
// have run, it is skipped (dropping tools strips their results too).
func TestCompleteWithFailover_ToolsOnlyToABackupThatHasThem(t *testing.T) {
	tools := []llm.ToolSpec{{Name: "read_page"}}
	fresh := llm.CompletionRequest{Tools: tools, Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	backup, err := failoverRun(failoverPipeline(32768), fresh)
	if err != nil || backup.calls != 1 || len(backup.got.Tools) != 0 {
		t.Errorf("fresh turn: calls %d tools %d err %v, want 1/0/nil", backup.calls, len(backup.got.Tools), err)
	}
	midLoop := llm.CompletionRequest{Tools: tools, Messages: []llm.Message{
		{Role: "user", Content: "read it"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c", Name: "read_page"}}},
		{Role: "tool", ToolCallID: "c", Content: "page"},
	}}
	backup, _ = failoverRun(failoverPipeline(32768), midLoop)
	if backup.calls != 0 {
		t.Error("mid-loop request sent to a backup without tool support")
	}
}
