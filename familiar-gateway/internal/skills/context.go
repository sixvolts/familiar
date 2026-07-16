package skills

import "context"

// SessionContext is the per-request identity bag that travels on the
// dispatch context.Context, letting skills that need session / user /
// agent identity pull it out without a bespoke parameter for each tool.
//
// The pipeline populates this immediately before calling Registry.Execute
// (directly or via the tool loop). Skills that don't care ignore it;
// skills that do (memory, scheduling, anything that writes on behalf of
// the current user) read it with ContextFrom.
//
// All fields are optional. Missing identity is a legitimate state —
// e.g. a CLI session with no authenticated user — and skills must
// handle empty strings gracefully.
type SessionContext struct {
	SessionID string
	UserID    string
	AgentID   string
	ChannelID string

	// ShardID is the shard this invocation runs inside, or "" on the
	// trusted path. Shard-scoped skills (skillpacks) authorize
	// against it (SKILL-PACKAGES-SPEC Phase 2).
	ShardID string

	// ScopeTag identifies the shard scope this invocation is running in.
	// Memory-writing skills use it to tag new rows so the top-level
	// Familiar view can filter `isolated` shards out at retrieval. Empty
	// for the trusted-surface path (top-level Familiar environment).
	ScopeTag string

	// BookScope is the set of book IDs a shard-enveloped invocation may
	// address. It mirrors the shard's book_access for the run path the
	// console session boundary can't reach: scheduled actions and /v1
	// invokes run under the owner's own session, so the wiki skill
	// enforces the confinement here instead. Nil or empty means
	// "no restriction" (trusted path, or a shard whose book_access is
	// empty = all of the owner's books). A non-empty set restricts every
	// book lookup to these IDs; membership is still required on top.
	BookScope []string

	// ExcludeFromHot, when true, signals memory-writing skills to set
	// the same flag on the FactProto they emit so the engine routes
	// the write straight to the persistent tier and skips the hot RAM
	// stage. Set by the pipeline when running an isolated-visibility
	// shard. Trusted path leaves this false and behavior is unchanged
	// (FAMILIAR-SHARDS-PHASE1-FINDINGS Issue 3).
	ExcludeFromHot bool
}

type sessionCtxKey struct{}

// WithContext returns a child context carrying the provided SessionContext.
// Use it at the pipeline boundary, just before dispatching tool calls to
// the Registry.
func WithContext(ctx context.Context, sc SessionContext) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, sc)
}

// ContextFrom extracts the SessionContext previously installed with
// WithContext. The second return is false when no SessionContext is
// present — in that case the first return is the zero value.
func ContextFrom(ctx context.Context) (SessionContext, bool) {
	sc, ok := ctx.Value(sessionCtxKey{}).(SessionContext)
	return sc, ok
}
