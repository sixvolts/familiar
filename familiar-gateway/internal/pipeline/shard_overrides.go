package pipeline

import "github.com/familiar/gateway/internal/shards"

// OverridesForShard translates a stored Shard row into the pipeline
// envelope. This is THE canonical translation — both the /v1 invoke
// adapter (shardapi) and the scheduled-actions runner build their
// envelopes here, so a new shard field can't reach one entry point
// and silently miss the other.
//
// Ephemeral shards get every "skip" flag set + no commit; persistent
// shards get session hydration enabled and commits on.
//
// Isolated-visibility shards also flip ExcludeFromHot so any commit
// the invocation produces (pipeline conversation fact, extracted
// facts, memory-skill writes) bypasses the engine's RAM cache and
// goes straight to pgvector. Without this flag the gateway-side
// pgvector filter would correctly hide the fact from a top-level
// search, but the engine's hot tier could still surface it for the
// brief window before consolidation
// (FAMILIAR-SHARDS-PHASE1-FINDINGS Issue 3).
func OverridesForShard(sh *shards.Shard) *ShardOverrides {
	ov := &ShardOverrides{
		ShardID:             sh.ID,
		SystemPrompt:        sh.SystemPrompt,
		SkipMemoryRetrieval: true, // shards never run the retrieval block
		ToolAllowlist:       append([]string(nil), sh.ToolAllowlist...),
		ScopeTag:            sh.ScopeTag,
		BookAccess:          append([]string(nil), sh.BookAccess...),
		ExcludeFromHot:      sh.Visibility == shards.VisibilityIsolated,
		ModelOverride:       sh.ModelPreference,
		TierHint:            sh.TierPreference,
		MaxTokens:           sh.MaxTokens,
	}
	if sh.Temperature != 0 {
		t := sh.Temperature
		ov.Temperature = &t
	}
	if sh.Persistence == shards.PersistenceEphemeral {
		ov.SkipSessionHydration = true
		ov.SkipCommit = true
	}
	return ov
}

// EphemeralOverrides is the scheduled-actions "ephemeral" envelope:
// nothing but the prompt. No system prompt, no memory retrieval, no
// tools (empty non-nil allowlist = nothing advertised, everything
// refused at dispatch), no session hydration, no commits, no scope
// tag. A pure one-shot completion whose only context is the action's
// prompt text.
func EphemeralOverrides() *ShardOverrides {
	return &ShardOverrides{
		SkipMemoryRetrieval:  true,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ToolAllowlist:        []string{},
	}
}
