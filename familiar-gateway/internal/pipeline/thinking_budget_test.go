package pipeline

import (
	"testing"

	"github.com/familiar/gateway/internal/classifier"
)

// On llama-server the <think> tokens come out of the same allowance as
// the answer, so buildLLMRequest must reserve the answer budget and add
// thinking headroom ON TOP — otherwise a long reasoning pass spends the
// whole max_tokens inside <think> and emits an empty reply
// (EXTERNAL-READINESS-REVIEW.md). That invariant still holds.
//
// What changed: on the trusted path the classifier's level no longer
// sets that headroom. The number is a ceiling, not a target — it can
// truncate a model mid-reasoning but can never make one reason more —
// so a small model's wrong "low" guess used to be a guillotine while a
// wrong "high" guess cost nothing. The trusted path now grants the
// maximum configured headroom at every enabled level.
func TestBuildLLMRequest_TrustedPathGrantsFlatThinkingHeadroom(t *testing.T) {
	p := &Pipeline{effort: classifier.DefaultResolver()}
	const answer = 4096
	flat := classifier.DefaultResolver().ThinkingFor(classifier.ThinkingHigh).TokenBudget

	cases := []struct {
		level         classifier.ThinkingLevel
		wantMaxTokens int
		wantThinking  int
	}{
		// Thinking off means off: no headroom at all, answer only.
		{classifier.ThinkingOff, answer, 0},
		// Every enabled level gets the same ceiling, so no verdict can
		// truncate reasoning that the model wanted to finish.
		{classifier.ThinkingLow, answer + flat, flat},
		{classifier.ThinkingMedium, answer + flat, flat},
		{classifier.ThinkingHigh, answer + flat, flat},
	}
	for _, c := range cases {
		route := &routeResult{
			modelID:    "llama-server/test",
			classifier: classifier.Output{Thinking: c.level},
		}
		req := p.buildLLMRequest(nil, route, &RouteInfo{}, false, nil, nil, false)
		if req.MaxTokens != c.wantMaxTokens {
			t.Errorf("%s: MaxTokens = %d, want %d (answer must survive the thinking headroom)",
				c.level, req.MaxTokens, c.wantMaxTokens)
		}
		if req.MaxThinkingTokens != c.wantThinking {
			t.Errorf("%s: MaxThinkingTokens = %d, want %d", c.level, req.MaxThinkingTokens, c.wantThinking)
		}
		// The answer budget must always survive whatever reasoning does.
		if req.MaxTokens-req.MaxThinkingTokens != answer {
			t.Errorf("%s: answer budget = %d, want %d", c.level,
				req.MaxTokens-req.MaxThinkingTokens, answer)
		}
	}
}

// Shard envelopes keep the level-derived budget. Their MaxTokens is set
// server-side on purpose, and a research worker must not be able to
// spend the deep-reasoning allowance — flattening there would raise a
// tier2 worker's ceiling 16x on a box with one inference slot.
func TestBuildLLMRequest_ShardOverrideKeepsLevelBudget(t *testing.T) {
	p := &Pipeline{effort: classifier.DefaultResolver()}
	res := classifier.DefaultResolver()

	for _, level := range []classifier.ThinkingLevel{
		classifier.ThinkingLow, classifier.ThinkingMedium, classifier.ThinkingHigh,
	} {
		route := &routeResult{
			modelID:    "llama-server/test",
			classifier: classifier.Output{Thinking: level},
		}
		req := p.buildLLMRequest(nil, route, &RouteInfo{}, false, nil,
			&ShardOverrides{MaxTokens: 1000}, false)
		want := 1000 + res.ThinkingFor(level).TokenBudget
		if req.MaxTokens != want {
			t.Errorf("%s: MaxTokens = %d, want %d (override answer budget + that level's thinking)",
				level, req.MaxTokens, want)
		}
	}

	// A low-tier worker must NOT inherit the trusted path's flat ceiling.
	route := &routeResult{
		modelID:    "llama-server/test",
		classifier: classifier.Output{Thinking: classifier.ThinkingLow},
	}
	req := p.buildLLMRequest(nil, route, &RouteInfo{}, false, nil,
		&ShardOverrides{MaxTokens: 512}, false)
	if flat := res.ThinkingFor(classifier.ThinkingHigh).TokenBudget; req.MaxTokens >= 512+flat {
		t.Errorf("shard MaxTokens = %d — a tier-low worker must not get the flat %d headroom",
			req.MaxTokens, flat)
	}
}
