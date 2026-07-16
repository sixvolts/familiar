package pipeline

import (
	"testing"

	"github.com/familiar/gateway/internal/classifier"
)

// buildLLMRequest must reserve the answer budget and add the thinking
// budget ON TOP — otherwise a deep-reasoning turn (thinking 8000 >
// the 4096 answer default) could spend its whole max_tokens inside
// <think> and emit an empty reply (EXTERNAL-READINESS-REVIEW.md).
func TestBuildLLMRequest_GrowsMaxTokensByThinkingBudget(t *testing.T) {
	p := &Pipeline{effort: classifier.DefaultResolver()}

	cases := []struct {
		level         classifier.ThinkingLevel
		wantMaxTokens int // 4096 answer + the level's thinking budget
		wantThinking  int
	}{
		{classifier.ThinkingOff, 4096, 0}, // disabled → answer only
		{classifier.ThinkingLow, 4096 + 500, 500},
		{classifier.ThinkingMedium, 4096 + 2000, 2000},
		{classifier.ThinkingHigh, 4096 + 8000, 8000}, // the bug case
	}
	for _, c := range cases {
		route := &routeResult{
			modelID:    "llama-server/test",
			classifier: classifier.Output{Thinking: c.level},
		}
		req := p.buildLLMRequest(nil, route, &RouteInfo{}, false, nil, nil, false)
		if req.MaxTokens != c.wantMaxTokens {
			t.Errorf("%s: MaxTokens = %d, want %d (answer must survive the thinking budget)",
				c.level, req.MaxTokens, c.wantMaxTokens)
		}
		if req.MaxThinkingTokens != c.wantThinking {
			t.Errorf("%s: MaxThinkingTokens = %d, want %d", c.level, req.MaxThinkingTokens, c.wantThinking)
		}
	}
}

// A shard's explicit MaxTokens is the ANSWER budget; the thinking
// budget still stacks on top so the override doesn't reintroduce the
// empty-reply trap.
func TestBuildLLMRequest_ShardOverrideAnswerBudgetPlusThinking(t *testing.T) {
	p := &Pipeline{effort: classifier.DefaultResolver()}
	route := &routeResult{
		modelID:    "llama-server/test",
		classifier: classifier.Output{Thinking: classifier.ThinkingHigh},
	}
	req := p.buildLLMRequest(nil, route, &RouteInfo{}, false, nil, &ShardOverrides{MaxTokens: 1000}, false)
	if req.MaxTokens != 1000+8000 {
		t.Errorf("MaxTokens = %d, want %d (override answer budget + thinking)", req.MaxTokens, 1000+8000)
	}
}
