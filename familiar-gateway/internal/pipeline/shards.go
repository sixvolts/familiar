package pipeline

import (
	"context"
	"log"
	"strings"

	"github.com/familiar/gateway/internal/llm"
	"github.com/familiar/gateway/internal/session"
)

// ShardOverrides parameterizes one turn of the pipeline to run inside a
// shard's capability envelope. Pass nil for the trusted-surface path —
// the pipeline's historical behavior is byte-identical when this is
// nil. Every field documents precisely what it replaces.
//
// The fields mirror FAMILIAR-SHARDS-PHASE1-SPEC §"Execution":
//
//   - SystemPrompt replaces the layered (base + tier + tool_policy)
//     prompt. The shard's prompt is the only non-user context the LLM
//     sees.
//   - SkipMemoryRetrieval disables engine.AssembleContext, pgvector
//     search, working-context zone, and relationship graph injection.
//   - SkipSessionHydration skips the persistent-summary load. Set for
//     ephemeral shards.
//   - SkipCommit skips commitAndExtract entirely. Set for ephemeral
//     shards; their invocations leave no session trace.
//   - ToolAllowlist, together with ShardOverrides being non-nil,
//     restricts what tools the LLM can even see AND what tools the
//     registry will dispatch. nil or empty means no tools.
//   - ScopeTag is plumbed into skills.SessionContext so memory-write
//     tools tag rows with the shard's scope.
//   - ModelOverride pins a specific model (bypasses the router).
//   - TierHint is honored when ModelOverride is empty; maps to the
//     same tier1..tier4 aliases the route_override prefix uses.
//   - MaxTokens / Temperature are the shard's LLM sampling settings;
//     zero MaxTokens falls through to the pipeline default, Temperature
//     is a pointer because 0.0 is a valid explicit value.
//   - SearchBudget grants the envelope web_search calls; zero keeps
//     the shard-path default (search disabled — shard turns bypass the
//     classifier and are stamped SearchNone). Stored shards don't set
//     it; it exists for purpose-built envelopes (research workers,
//     RESEARCH-SKILL-SPEC §6.1).
type ShardOverrides struct {
	ShardID string

	SystemPrompt string

	SkipMemoryRetrieval  bool
	SkipSessionHydration bool
	SkipCommit           bool

	ToolAllowlist []string

	ScopeTag string

	// BookAccess confines wiki tool dispatch to this set of book IDs.
	// It carries the shard's book_access into the run paths that don't
	// cross the console session boundary (scheduled actions, /v1
	// invokes), where authz.go's session-level book intersection never
	// runs. Nil/empty = no restriction (matches the shard config's
	// "empty book_access = all owner books" semantics); the wiki skill
	// reads it off SessionContext.BookScope and denies anything else.
	BookAccess []string

	// ExcludeFromHot, when true, instructs the engine to bypass the
	// hot RAM tier on every commit this invocation produces — both
	// the pipeline's conversation fact and any facts a tool dispatch
	// writes via the memory skill. Set by the shardapi handler when
	// the shard's visibility is `isolated`. Closes the leak path
	// where an isolated-shard write could be returned by top-level
	// retrieval through the engine's hot cache before the
	// gateway-side pgvector filter could re-hide it
	// (FAMILIAR-SHARDS-PHASE1-FINDINGS Issue 3).
	ExcludeFromHot bool

	ModelOverride string
	TierHint      string

	MaxTokens   int
	Temperature *float32

	// SearchBudget grants this envelope N web_search calls for the
	// turn. Shard turns bypass the classifier and are stamped
	// SearchNone (shardClassifierOutput), which hard-disables
	// web_search at dispatch; a positive SearchBudget lifts that for
	// purpose-built envelopes (research workers). Zero keeps today's
	// behavior — existing shards and ephemeral action envelopes never
	// search.
	SearchBudget int
}

// toolAllowlistSet converts a slice to a lookup set. Returns nil when
// the input is nil so callers can distinguish "trusted path / no
// filtering" from "shard path with empty allowlist / block everything."
// Inside the shard path, callers treat nil-and-empty the same.
func toolAllowlistSet(tools []string) map[string]bool {
	if tools == nil {
		return nil
	}
	set := make(map[string]bool, len(tools))
	for _, t := range tools {
		set[t] = true
	}
	return set
}

// filterToolSpecs narrows the registered tool set to the shard's
// allowlist and projects the result into the provider-neutral
// llm.ToolSpec shape. The registry does the filtering and the
// unknown-tool warn log (see skills.Registry.FilterToolDefinitions);
// this function is a thin adapter. A nil registry yields nil, matching
// skillToolSpecs's "registry missing ⇒ no tools" behavior.
//
// Per FAMILIAR-SHARDS-PHASE1-FINDINGS, a single `[shards] tools`
// line is emitted whenever a shard request is finalized — bridges
// the gap between "what the operator put in the allowlist" and
// "what was actually advertised to the LLM" so the
// allowlist-doesn't-match-registry failure mode is one greppable
// log line away from a diagnosis.
func (p *Pipeline) filterToolSpecs(allowlist []string) []llm.ToolSpec {
	if p.skillRegistry == nil || len(allowlist) == 0 {
		log.Printf("[shards] tools: requested=%v advertised=[] (registry=%t)",
			allowlist, p.skillRegistry != nil)
		return nil
	}
	defs := p.skillRegistry.FilterToolDefinitions(allowlist)
	specs := make([]llm.ToolSpec, 0, len(defs))
	advertised := make([]string, 0, len(defs))
	for _, d := range defs {
		specs = append(specs, llm.ToolSpec{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.Parameters,
		})
		advertised = append(advertised, d.Name)
	}
	dropped := diffNames(allowlist, advertised)
	log.Printf("[shards] tools: requested=%v advertised=%v dropped=%v",
		allowlist, advertised, dropped)
	if len(specs) == 0 {
		return nil
	}
	return specs
}

// diffNames returns the entries in `requested` that did not survive
// into `kept`. Used by filterToolSpecs's diagnostic log so an operator
// can see "you allowlisted X but it wasn't advertised because the
// registry doesn't know about it" in a single grep.
func diffNames(requested, kept []string) []string {
	keptSet := make(map[string]bool, len(kept))
	for _, n := range kept {
		keptSet[n] = true
	}
	var out []string
	for _, n := range requested {
		if !keptSet[n] {
			out = append(out, n)
		}
	}
	return out
}

// buildShardMessages composes the minimal message slice for a shard
// invocation: the shard's system prompt (if any), optionally prior
// session turns for persistent shards, and the incoming user message.
// Nothing else — no retrieved memories, no working context, no
// relationship graph, no tool results block. The shard is the whole
// context envelope.
//
// This intentionally does NOT go through ctxbuild. ctxbuild exists to
// pack the trusted-surface's many context zones under a token budget;
// shards have no zones to pack. If a future shard use-case turns out
// to need budgeted packing (e.g., very long system prompts that need
// to crowd out session turns), we can fold ctxbuild back in at that
// point — there's no value in it yet.
func (p *Pipeline) buildShardMessages(sess *session.Session, userMsg string, overrides *ShardOverrides, info *RouteInfo) []llm.Message {
	var messages []llm.Message
	if overrides.SystemPrompt != "" {
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: overrides.SystemPrompt,
		})
	}
	if !overrides.SkipSessionHydration && sess != nil {
		// Persistent shards see prior turns the same way the trusted
		// path does — the session.Session struct carries them in memory
		// after hydration. The tool shape must survive the replay: an
		// assistant turn that only carried tool_calls has empty content,
		// and OpenAI-shaped servers reject an assistant message with
		// neither content nor tool_calls.
		turns := sess.RecentTurns(20)
		// If the window opens mid tool-exchange (a tool result whose
		// assistant tool-call parent was cut off), the transcript is
		// invalid; trim to the first user turn.
		for len(turns) > 0 && turns[0].Role != "user" {
			turns = turns[1:]
		}
		for _, t := range turns {
			messages = append(messages, llm.Message{
				Role:       t.Role,
				Content:    t.Content,
				ToolCalls:  unmarshalToolCalls(t.ToolCalls),
				ToolCallID: t.ToolCallID,
			})
		}
	}
	messages = append(messages, llm.Message{Role: "user", Content: userMsg})

	// MemHits stays 0 for shards — no retrieval happened. Leave the
	// field at its zero value so observability metrics don't have to
	// special-case shard invocations.
	info.MemHits = 0
	return messages
}

// HandleShard is the shard-invocation entry point. It takes the same
// basic inputs as Handle but runs the turn inside the envelope
// described by `overrides`. The overrides pointer must be non-nil —
// callers that want the trusted path should use Handle.
//
// convCtx is deliberately not exposed here: shards bypass the sidecar
// router (ModelOverride/TierHint determines the model), so the
// conversation-context blob the sidecar consumes isn't meaningful.
func (p *Pipeline) HandleShard(ctx context.Context, sess *session.Session, userMsg string, overrides *ShardOverrides) (string, *RouteInfo, error) {
	if overrides == nil {
		// Fail loud. Silent fallback to Handle would silently pass the
		// invocation through the trusted pipeline, which is the exact
		// security boundary shards exist to enforce.
		panic("pipeline.HandleShard: overrides must not be nil; use Handle for the trusted path")
	}
	return p.handle(ctx, sess, userMsg, nil, overrides)
}

// HandleShardStream is the streaming counterpart to HandleShard.
func (p *Pipeline) HandleShardStream(
	ctx context.Context,
	sess *session.Session,
	userMsg string,
	overrides *ShardOverrides,
	onChunk func(string),
	onReasoningChunk func(string),
	onStatus func(string),
) (string, *RouteInfo, error) {
	if overrides == nil {
		panic("pipeline.HandleShardStream: overrides must not be nil; use HandleStream for the trusted path")
	}
	return p.handleStream(ctx, sess, userMsg, nil, onChunk, onReasoningChunk, onStatus, overrides)
}

// scopeTagFor extracts the scope_tag to stamp on skill-dispatch
// SessionContext. Trusted path uses empty string; shard path uses the
// shard's configured scope_tag. The helper keeps the nil-check in one
// place.
// shardIDFor mirrors scopeTagFor for the shard identity itself —
// shard-scoped skills authorize against it via SessionContext.
func shardIDFor(overrides *ShardOverrides) string {
	if overrides == nil {
		return ""
	}
	return overrides.ShardID
}

func scopeTagFor(overrides *ShardOverrides) string {
	if overrides == nil {
		return ""
	}
	return overrides.ScopeTag
}

// searchBudgetFor mirrors scopeTagFor for the envelope's web_search
// grant. Trusted path (nil overrides) yields 0 = no override; the
// classifier/tier budget applies unchanged.
func searchBudgetFor(overrides *ShardOverrides) int {
	if overrides == nil {
		return 0
	}
	return overrides.SearchBudget
}

// bookScopeFor mirrors scopeTagFor for the wiki book allowlist. Trusted
// path (nil overrides) yields nil = no restriction.
func bookScopeFor(overrides *ShardOverrides) []string {
	if overrides == nil {
		return nil
	}
	return overrides.BookAccess
}

// excludeFromHotFor mirrors scopeTagFor for the RAM-bypass flag. Used
// by every commit site that needs to stamp ExcludeFromHot on a
// FactProto and by runCompletion when populating skills.SessionContext.
func excludeFromHotFor(overrides *ShardOverrides) bool {
	if overrides == nil {
		return false
	}
	return overrides.ExcludeFromHot
}

// shardModelOverride resolves overrides.ModelOverride / overrides.TierHint
// into a concrete model ID, falling back to nil when neither is set.
// Returns (modelID, forcedComplexity, true) when an override applies,
// or ("", "", false) when the caller should run the normal router.
//
// The TierHint path reuses the same tier1..tier4 aliases the existing
// `tier N:` prefix command uses, so shards named with a tier hint get
// the same classifier-free routing the prefix form does.
func (p *Pipeline) shardModelOverride(overrides *ShardOverrides) (modelID, complexity string, ok bool) {
	if overrides == nil {
		return "", "", false
	}
	if overrides.ModelOverride != "" {
		return overrides.ModelOverride, overrides.TierHint, true
	}
	switch strings.ToLower(overrides.TierHint) {
	case "tier1", "trivial":
		return p.router.GetSidecarModelID(), "trivial", true
	case "tier2", "knowledge":
		return p.router.GetSidecarModelID(), "knowledge", true
	case "tier3", "technical":
		return p.router.GetChatModelID(), "technical", true
	case "tier4", "deep", "deep_reasoning":
		return p.router.GetChatModelID(), "deep_reasoning", true
	}
	return "", "", false
}
