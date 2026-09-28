package pipeline

import "github.com/familiar/gateway/internal/shards"

// OverridesForShard translates a stored Shard row into the pipeline
// envelope. This is THE canonical translation — both the /v1 invoke
// adapter (shardapi) and the scheduled-actions runner build their
// envelopes here, so a new shard field can't reach one entry point
// and silently miss the other.
//
// Ephemeral shards get every "skip" flag set + no commit; persistent
// shards get session hydration enabled and commits on. An isolated
// shard's writes stay out of top-level retrieval through their scope
// tag.
func OverridesForShard(sh *shards.Shard) *ShardOverrides {
	ov := &ShardOverrides{
		ShardID:       sh.ID,
		SystemPrompt:  sh.SystemPrompt,
		ToolAllowlist: append([]string(nil), sh.ToolAllowlist...),
		ScopeTag:      sh.ScopeTag,
		BookAccess:    append([]string(nil), sh.BookAccess...),
		ModelOverride: sh.ModelPreference,
		TierHint:      sh.TierPreference,
		MaxTokens:     sh.MaxTokens,
	}
	// The stored temperature is what the owner set, 0 included: 0 means
	// deterministic sampling, and treating it as "unset" ran the
	// provider's default (0.7-0.8) instead.
	t := sh.Temperature
	ov.Temperature = &t
	// An allowlisted web search tool (web_search, brave_page_read,
	// search_news) is a grant: shard turns are stamped SearchNone (no
	// classifier) or classified like trusted turns, and either way the
	// search was refused or left to the classifier.
	for _, tool := range sh.ToolAllowlist {
		if braveTools[tool] {
			ov.SearchBudget = shardWebSearchBudget
		}
	}
	if sh.Persistence == shards.PersistenceEphemeral {
		ov.SkipSessionHydration = true
		ov.SkipCommit = true
		// Ephemeral means no side effects: no tool that writes. Saving
		// refuses them; a shard saved before a tool was known to write
		// still lists it, so it's dropped here too.
		kept := make([]string, 0, len(ov.ToolAllowlist))
		for _, tool := range ov.ToolAllowlist {
			if !shards.IsWriteCapable(tool) {
				kept = append(kept, tool)
			}
		}
		ov.ToolAllowlist = kept
	}
	return ov
}

// shardWebSearchBudget is how many web searches (braveTools calls) a
// turn of a shard that allowlists one may make.
const shardWebSearchBudget = 4

// braveTools are the tools that make a (billed) Brave request. Each call
// counts against the turn's web search budget and none runs on a turn
// with search disabled; only web_search did, so brave_page_read (up to
// 10 sources a call) and search_news ran uncounted, on shards and
// SearchNone turns too.
var braveTools = map[string]bool{"web_search": true, "brave_page_read": true, "search_news": true}

// EphemeralOverrides is the scheduled-actions "ephemeral" envelope:
// nothing but the prompt. No system prompt, no memory retrieval, no
// tools (empty non-nil allowlist = nothing advertised, everything
// refused at dispatch), no session hydration, no commits, no scope
// tag. A pure one-shot completion whose only context is the action's
// prompt text.
func EphemeralOverrides() *ShardOverrides {
	return &ShardOverrides{
		SkipSessionHydration: true,
		SkipCommit:           true,
		ToolAllowlist:        []string{},
	}
}
