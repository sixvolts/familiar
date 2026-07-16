package classifier

// Bridge between config.EffortConfig (toml shape) and EffortResolver
// (in-memory lookup). Lives here rather than in internal/config so
// the config package stays free of classifier-package types.

import "github.com/familiar/gateway/internal/config"

// ResolverFromConfig builds an EffortResolver by overlaying
// operator-supplied [effort.*] values onto the spec defaults.
// Each level is resolved independently — if the operator only
// configured [effort.thinking.high], every other level still
// returns its default.
//
// Zero values in the config behave as "use the default": e.g.
// `[effort.memory_depth.shallow] top_k = 0` falls back to the
// default top-k for "shallow" rather than meaning "literally
// zero results."
func ResolverFromConfig(cfg config.EffortConfig) *EffortResolver {
	r := DefaultResolver()

	// Thinking levels.
	for level, src := range map[ThinkingLevel]config.EffortThinkingLevel{
		ThinkingOff:    cfg.Thinking.Off,
		ThinkingLow:    cfg.Thinking.Low,
		ThinkingMedium: cfg.Thinking.Medium,
		ThinkingHigh:   cfg.Thinking.High,
	} {
		merged := r.Thinking[level]
		if src.Enabled != nil {
			merged.Enabled = *src.Enabled
		}
		if src.TokenBudget > 0 {
			merged.TokenBudget = src.TokenBudget
		}
		r.Thinking[level] = merged
	}

	// Memory depth levels.
	for level, src := range map[MemoryDepth]config.EffortMemoryDepthLevel{
		MemoryNone:    cfg.MemoryDepth.None,
		MemoryShallow: cfg.MemoryDepth.Shallow,
		MemoryDeep:    cfg.MemoryDepth.Deep,
	} {
		merged := r.Memory[level]
		if src.Skip {
			merged.Skip = true
		}
		if src.TopK > 0 {
			merged.TopK = src.TopK
		}
		if src.SimilarityThreshold > 0 {
			merged.SimilarityThreshold = src.SimilarityThreshold
		}
		r.Memory[level] = merged
	}

	// Search depth levels.
	for level, src := range map[SearchDepth]config.EffortSearchDepthLevel{
		SearchNone:    cfg.SearchDepth.None,
		SearchShallow: cfg.SearchDepth.Shallow,
		SearchDeep:    cfg.SearchDepth.Deep,
	} {
		merged := r.Search[level]
		if src.Skip {
			merged.Skip = true
		}
		if src.MaxSearches > 0 {
			merged.MaxSearches = src.MaxSearches
		}
		r.Search[level] = merged
	}

	return r
}
