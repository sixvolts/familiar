package ctxbuild

import "fmt"

// EstimateTokens returns an approximate token count for a string.
//
// We use len/4 as a deliberate under-engineering choice: accurate tokenization
// would require a BPE/tiktoken table per model, and the builder is managing a
// budget, not computing an exact fit. Over-reserving by 10% is fine; clipping
// a real message because our count was off by a token is not.
func EstimateTokens(s string) int {
	return len(s) / 4
}

// Budget holds the resolved per-zone token allowances derived from a Config.
type Budget struct {
	Total        int // WindowSize - OutputReservation
	System       int
	Memories     int
	Tools        int
	Conversation int // remainder after the three fixed zones
}

// Resolve converts a Config into concrete per-zone token counts.
//
// The conversation zone is whatever is left after subtracting the three fixed
// zones from Total. If the ratios sum to more than 1.0 the conversation zone
// will be zero (or negative, which the builder treats as "no room").
func (c Config) Resolve() Budget {
	total := c.WindowSize - c.OutputReservation
	if total < 0 {
		total = 0
	}
	sys := int(float64(total) * c.SystemPromptRatio)
	mem := int(float64(total) * c.MemoryRatio)
	tools := int(float64(total) * c.ToolResultRatio)
	conv := total - sys - mem - tools
	if conv < 0 {
		conv = 0
	}
	return Budget{
		Total:        total,
		System:       sys,
		Memories:     mem,
		Tools:        tools,
		Conversation: conv,
	}
}

// truncateToTokens returns s trimmed to at most maxTokens estimated tokens.
// Because EstimateTokens is len/4, we truncate at maxTokens*4 bytes. This may
// split a UTF-8 rune or a word — the builder uses it only as a last-resort
// safeguard when a single prebuilt string exceeds its zone.
func truncateToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if EstimateTokens(s) <= maxTokens {
		return s
	}
	cut := maxTokens * 4
	if cut > len(s) {
		return s
	}
	return s[:cut]
}

// CapToolResult head+tail truncates a single oversized tool result.
//
// Unlike truncateToTokens (head-only, last-resort), this keeps the
// START and END of the payload and drops the middle: a tool result's
// head usually carries the summary / first records and the tail
// carries totals, closing structure, or an error indicator — the
// middle is the most disposable. A marker records how many bytes
// were elided so the model knows the result was clipped.
//
// maxTokens <= 0 disables capping (returns s unchanged).
// CapToolResult is exported for the tool loop, which caps each
// result before appending it to the running message history.
func CapToolResult(s string, maxTokens int) string {
	if maxTokens <= 0 || EstimateTokens(s) <= maxTokens {
		return s
	}
	budget := maxTokens * 4
	// Reserve ~48 bytes for the elision marker, split the rest evenly
	// between head and tail.
	half := (budget - 48) / 2
	if half < 1 {
		return s[:budget]
	}
	elided := len(s) - 2*half
	if elided <= 0 {
		return s
	}
	return s[:half] +
		fmt.Sprintf("\n\n…[%d characters elided]…\n\n", elided) +
		s[len(s)-half:]
}
