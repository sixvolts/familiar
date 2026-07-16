// Package ctxbuild assembles the LLM prompt context under a token budget.
//
// The builder replaces the gateway pipeline's prior ad-hoc concatenation of
// system prompt, memories, and conversation history. It divides the available
// context window into fixed-ratio zones (system, working context, memories,
// tool results) and lets the conversation history expand to fill the rest,
// evicting the oldest turns when the budget is exhausted.
package ctxbuild

import "github.com/familiar/gateway/internal/session"

// Config controls the token budget used by Builder.
//
// WindowSize and OutputReservation are absolute token counts. The three ratios
// carve up the remaining budget; anything left over after those three zones is
// the conversation zone.
type Config struct {
	WindowSize        int     `toml:"window_size"`
	OutputReservation int     `toml:"output_reservation"`
	SystemPromptRatio float64 `toml:"system_prompt_ratio"`
	MemoryRatio       float64 `toml:"memory_ratio"`
	ToolResultRatio   float64 `toml:"tool_result_ratio"`

	// MaxToolResultTokens caps a SINGLE tool result before it enters
	// the tool zone. A raw MCP / search payload can be tens of
	// thousands of tokens; left uncapped one result fills the whole
	// zone and crowds out everything else. Oversized results are
	// head+tail truncated (the start carries the summary, the tail
	// carries totals / closing structure / errors). Zero disables
	// per-result capping and falls back to zone-level eviction only.
	MaxToolResultTokens int `toml:"max_tool_result_tokens"`
}

// DefaultConfig returns spec defaults: 32K window, 4K reserved for output,
// and the zone ratios from FAMILIAR-PHASE2-SPEC.md §2.
func DefaultConfig() Config {
	return Config{
		WindowSize:          32768,
		OutputReservation:   4096,
		SystemPromptRatio:   0.10,
		MemoryRatio:         0.12,
		ToolResultRatio:     0.12,
		MaxToolResultTokens: 2000, // ~8KB — see chat-turn context review §6
	}
}

// Memory is one retrieved long-term memory, ready to be formatted into the
// system prompt. The builder is storage-agnostic; callers adapt their own
// memory types into this struct.
type Memory struct {
	Content    string
	Scope      string
	Similarity float64
}

// ToolResult is one tool invocation result accumulated during an agentic loop.
type ToolResult struct {
	Name    string
	Content string
}

// AssembledContext is the fully budgeted result of a Build call.
//
// The caller is responsible for turning this into provider-specific messages;
// the builder only decides what fits and what does not.
type AssembledContext struct {
	SystemPrompt string
	// UserPrompt is the per-user "assistant personality" block,
	// rendered as its own labeled section after SystemPrompt.
	UserPrompt          string
	Memories            []Memory
	RelationshipLines   []string
	ConversationSummary string
	RecentTurns         []session.Turn
	ToolResults         []ToolResult
	TokenUsage          TokenBreakdown
	EvictedTurns        []session.Turn
}

// TokenBreakdown records per-zone token usage for logging and tests.
//
// Headroom is Budget minus Total and can be negative if a zone overflowed
// (should not happen with truncation in place, but we surface it so tests
// can assert invariants).
type TokenBreakdown struct {
	System       int
	Memories     int
	Tools        int
	Conversation int
	Total        int
	Budget       int
	Headroom     int
}
