package classifier

// Effort-level resolution. The classifier emits ordinal levels
// (Output struct in this package); operators tune the concrete
// budgets in [effort.*] config blocks; this file is the bridge.
//
// Defaults below are calibrated for a Qwen3.5-122B-class chat
// model on a 262K window. Adjust in gateway.toml without code
// changes per CHAT-REARCH §"Effort Level Configuration".

// ThinkingBudget is the resolved budget for one ThinkingLevel.
// Enabled=false means thinking off entirely; TokenBudget is the
// max-tokens budget when on.
type ThinkingBudget struct {
	Enabled     bool
	TokenBudget int
	// Effort is the backend's native thinking-depth selector (Qwen 3.8:
	// low/medium/xhigh). Empty leaves it unset so the server default
	// applies. Unlike TokenBudget — which only grants headroom and is
	// deliberately NOT allowed to bound the trusted path (see the comment
	// at the thinkingHeadroom assignment in pipeline.go) — this tells the
	// model to think less, which is safe: it cannot truncate a thought
	// mid-stream, it just produces a shorter one.
	Effort string
}

// MemoryBudget is the resolved retrieval shape for one MemoryDepth.
// Skip=true means no retrieval at all; TopK and SimilarityThreshold
// drive pgvector + hot-tier params when retrieving.
type MemoryBudget struct {
	Skip                bool
	TopK                int
	SimilarityThreshold float64
}

// SearchBudget is the resolved web-search shape for one SearchDepth.
type SearchBudget struct {
	Skip        bool
	MaxSearches int
}

// EffortResolver returns concrete budgets for a classifier level.
// Implementations are typically backed by config.EffortConfig with
// fall-through defaults; the resolver lives in this package so
// every consumer of classifier.Output can call into it without
// taking a config dependency.
//
// A zero value works: it returns the spec defaults for every
// level. Pipeline tests use this to avoid loading a real config.
type EffortResolver struct {
	Thinking map[ThinkingLevel]ThinkingBudget
	Memory   map[MemoryDepth]MemoryBudget
	Search   map[SearchDepth]SearchBudget
}

// DefaultResolver is the spec-default mapping. Operators override
// piecewise via [effort.*] config; missing levels fall back to
// these.
func DefaultResolver() *EffortResolver {
	return &EffortResolver{
		Thinking: map[ThinkingLevel]ThinkingBudget{
			ThinkingOff:    {Enabled: false},
			ThinkingLow:    {Enabled: true, TokenBudget: 500, Effort: "low"},
			ThinkingMedium: {Enabled: true, TokenBudget: 2000, Effort: "medium"},
			// High maps to medium, not xhigh, on purpose. The level is a small
			// model's one-shot guess, and xhigh is a large jump in latency for
			// a guess to be spending. Operators can raise it per-level via
			// [effort.thinking.high] effort = "xhigh".
			ThinkingHigh: {Enabled: true, TokenBudget: 8000, Effort: "medium"},
		},
		Memory: map[MemoryDepth]MemoryBudget{
			MemoryNone:    {Skip: true},
			MemoryShallow: {TopK: 5, SimilarityThreshold: 0.60},
			MemoryDeep:    {TopK: 20, SimilarityThreshold: 0.40},
		},
		Search: map[SearchDepth]SearchBudget{
			SearchNone:    {Skip: true},
			SearchShallow: {MaxSearches: 1},
			SearchDeep:    {MaxSearches: 5},
		},
	}
}

// ThinkingFor returns the resolved budget for a level. Falls back
// to DefaultResolver's value when the resolver doesn't carry the
// level.
func (r *EffortResolver) ThinkingFor(level ThinkingLevel) ThinkingBudget {
	if r != nil {
		if b, ok := r.Thinking[level]; ok {
			return b
		}
	}
	return DefaultResolver().Thinking[level]
}

// MemoryFor returns the resolved budget for a level.
func (r *EffortResolver) MemoryFor(level MemoryDepth) MemoryBudget {
	if r != nil {
		if b, ok := r.Memory[level]; ok {
			return b
		}
	}
	return DefaultResolver().Memory[level]
}

// SearchFor returns the resolved budget for a level.
func (r *EffortResolver) SearchFor(level SearchDepth) SearchBudget {
	if r != nil {
		if b, ok := r.Search[level]; ok {
			return b
		}
	}
	return DefaultResolver().Search[level]
}
