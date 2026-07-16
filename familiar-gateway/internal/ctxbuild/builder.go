package ctxbuild

import "github.com/familiar/gateway/internal/session"

// Input is everything the Builder needs to assemble one context.
//
// The builder is pure: it does no IO. Callers retrieve memories, load session
// history, and resolve the working context themselves, then hand the results
// in. This keeps the builder trivially unit-testable and lets the pipeline
// parallelise the retrieval steps.
//
// Turns is newest-last (matching session.Session.RecentTurns). Memories is
// already filtered to the caller's relevance threshold, in preferred order.
// Summary is the existing rolling summary for the session, if any.
//
// ReservedTokens lets callers reserve space for content that isn't in the
// Input but still has to fit alongside the assembled context — primarily the
// current user message, which the pipeline appends after assembly. The
// reservation is drawn from the conversation zone, since that's the only
// elastic zone and history is the cheapest thing to evict.
type Input struct {
	SystemPrompt string
	// UserPrompt is the per-user "assistant personality" prompt,
	// authored by the user in their profile. It's behavioral (how
	// the assistant should act) rather than factual, so it gets its
	// own labeled section right after the admin system prompt — see
	// flattenAssembled in the pipeline. Shares the System zone's
	// token budget.
	UserPrompt        string
	Summary           string
	Turns             []session.Turn
	Memories          []Memory
	RelationshipLines []string
	ToolResults       []ToolResult
	ReservedTokens    int
}

// Builder assembles AssembledContext values under a token budget.
//
// Builders are safe for concurrent use: they hold only an immutable Config.
type Builder struct {
	cfg Config
}

// New constructs a Builder with the given config.
func New(cfg Config) *Builder {
	return &Builder{cfg: cfg}
}

// Build runs the assembly algorithm from FAMILIAR-PHASE2-SPEC.md §2.
//
// Order of operations:
//  1. Resolve per-zone budgets from config.
//  2. System prompt: take as-is, truncate if it overflows its zone.
//  3. Memories: include in order until the memory budget is exhausted.
//  4. Tool results: newest-first, drop oldest to fit the tool budget.
//  5. Conversation: if a summary is present, reserve 40% of the conv budget
//     for it and 60% for verbatim turns; otherwise give turns the full zone.
//     Walk turns newest-first, keep what fits, evict the rest.
//  6. Fill the TokenBreakdown.
func (b *Builder) Build(in Input) AssembledContext {
	budget := b.cfg.Resolve()

	// Reserve space for caller-provided overhead (e.g. the incoming user
	// message) by shrinking the conversation zone. This is the only elastic
	// zone, so eating into it first keeps memories and the system prompt
	// intact at the cost of a few more evicted turns.
	if in.ReservedTokens > 0 {
		budget.Conversation -= in.ReservedTokens
		if budget.Conversation < 0 {
			budget.Conversation = 0
		}
	}

	out := AssembledContext{
		SystemPrompt: truncateToTokens(in.SystemPrompt, budget.System),
		UserPrompt:   truncateToTokens(in.UserPrompt, budget.System),
	}

	out.Memories = fitMemories(in.Memories, budget.Memories)
	out.RelationshipLines = in.RelationshipLines
	out.ToolResults = fitToolResults(in.ToolResults, budget.Tools, b.cfg.MaxToolResultTokens)

	summary, turns, evicted := fitConversation(in.Summary, in.Turns, budget.Conversation)
	out.ConversationSummary = summary
	out.RecentTurns = turns
	out.EvictedTurns = evicted

	out.TokenUsage = breakdown(out, budget)
	return out
}

// fitMemories includes memories in order until the zone is full.
// Spec §2: "Take top-k results that fit within the memory budget."
func fitMemories(mems []Memory, zoneBudget int) []Memory {
	if zoneBudget <= 0 || len(mems) == 0 {
		return nil
	}
	var kept []Memory
	used := 0
	for _, m := range mems {
		tk := EstimateTokens(m.Content)
		if used+tk > zoneBudget {
			break
		}
		kept = append(kept, m)
		used += tk
	}
	return kept
}

// fitToolResults caps each result, then keeps the newest that fit the zone.
//
// Two stages of defense (chat-turn context review §6):
//  1. Per-result cap — every result is head+tail truncated to
//     maxResultTokens first, so one giant payload can't monopolise
//     the zone (or get kept whole as the always-keep newest entry).
//  2. Zone eviction — older results are dropped first: in an agentic
//     loop the most recent call is almost always the one the model
//     needs to reason about next.
func fitToolResults(results []ToolResult, zoneBudget, maxResultTokens int) []ToolResult {
	if zoneBudget <= 0 || len(results) == 0 {
		return nil
	}
	kept := make([]ToolResult, 0, len(results))
	used := 0
	for i := len(results) - 1; i >= 0; i-- {
		content := CapToolResult(results[i].Content, maxResultTokens)
		tk := EstimateTokens(content)
		if used+tk > zoneBudget && len(kept) > 0 {
			break
		}
		kept = append([]ToolResult{{Name: results[i].Name, Content: content}}, kept...)
		used += tk
	}
	return kept
}

// fitConversation applies the 60/40 turn-vs-summary split from spec §2.
//
// Returns (possibly truncated) summary, the kept turns in original order,
// and the evicted turns (oldest first) so the caller can feed them to the
// eviction-time summariser and fact extractor.
func fitConversation(summary string, turns []session.Turn, zoneBudget int) (string, []session.Turn, []session.Turn) {
	if zoneBudget <= 0 {
		return "", nil, append([]session.Turn(nil), turns...)
	}

	summaryBudget := 0
	if summary != "" {
		summaryBudget = zoneBudget * 40 / 100
	}
	turnBudget := zoneBudget - summaryBudget

	// Walk newest-first, keep turns that fit.
	keptRev := make([]session.Turn, 0, len(turns))
	used := 0
	cutoff := 0 // index of first kept turn in the original slice
	for i := len(turns) - 1; i >= 0; i-- {
		tk := EstimateTokens(turns[i].Content)
		if used+tk > turnBudget && len(keptRev) > 0 {
			cutoff = i + 1
			break
		}
		keptRev = append(keptRev, turns[i])
		used += tk
		cutoff = i
	}

	// Reverse back into chronological order.
	kept := make([]session.Turn, len(keptRev))
	for i, t := range keptRev {
		kept[len(keptRev)-1-i] = t
	}

	var evicted []session.Turn
	if cutoff > 0 {
		evicted = append([]session.Turn(nil), turns[:cutoff]...)
	}

	fittedSummary := truncateToTokens(summary, summaryBudget)
	return fittedSummary, kept, evicted
}

// breakdown totals the tokens across every zone of an assembled context.
func breakdown(ctx AssembledContext, budget Budget) TokenBreakdown {
	bd := TokenBreakdown{
		// UserPrompt shares the System zone, so it's folded into the
		// System count to keep the breakdown's totals honest.
		System: EstimateTokens(ctx.SystemPrompt) + EstimateTokens(ctx.UserPrompt),
		Budget: budget.Total,
	}
	for _, m := range ctx.Memories {
		bd.Memories += EstimateTokens(m.Content)
	}
	for _, t := range ctx.ToolResults {
		bd.Tools += EstimateTokens(t.Content)
	}
	bd.Conversation = EstimateTokens(ctx.ConversationSummary)
	for _, t := range ctx.RecentTurns {
		bd.Conversation += EstimateTokens(t.Content)
	}
	bd.Total = bd.System + bd.Memories + bd.Tools + bd.Conversation
	bd.Headroom = bd.Budget - bd.Total
	return bd
}
