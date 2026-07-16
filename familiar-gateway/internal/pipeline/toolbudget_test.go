package pipeline

import (
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/ctxbuild"
)

// budgetToolResult must (a) head+tail cap a single oversized result,
// (b) let results through while under the cumulative budget, and (c)
// once the running total would exceed the budget, drop the payload for
// a short synthesize notice so the tool loop can't overflow the context
// window into a hard provider 400.
func TestBudgetToolResult(t *testing.T) {
	const perResult = 50 // ~200 bytes
	const budget = 120   // ~480 bytes total across the turn

	// Under budget: a small result passes through and increments used.
	c, used := budgetToolResult("notes", "a short note", perResult, budget, 0)
	if c != "a short note" {
		t.Errorf("small result altered: %q", c)
	}
	if used != ctxbuild.EstimateTokens("a short note") {
		t.Errorf("used = %d, want %d", used, ctxbuild.EstimateTokens("a short note"))
	}

	// A single huge result is head+tail capped to ~perResult tokens.
	huge := strings.Repeat("x", 10_000)
	c, _ = budgetToolResult("wiki", huge, perResult, budget, 0)
	if ctxbuild.EstimateTokens(c) > perResult+20 {
		t.Errorf("result not capped: %d tokens", ctxbuild.EstimateTokens(c))
	}
	if !strings.Contains(c, "elided") {
		t.Errorf("capped result missing elision marker: %q", c[:min(80, len(c))])
	}

	// Already at budget: the next result is dropped for a notice.
	c, used2 := budgetToolResult("search", strings.Repeat("y", 4000), perResult, budget, budget)
	if !strings.Contains(c, "output omitted") || !strings.Contains(c, "search") {
		t.Errorf("over-budget result not collapsed to a notice: %q", c)
	}
	if used2 <= budget {
		t.Errorf("running total should still advance past budget, got %d", used2)
	}

	// budget <= 0 disables the cumulative check (per-result cap only).
	c, _ = budgetToolResult("x", "keep me", perResult, 0, 1_000_000)
	if c != "keep me" {
		t.Errorf("zero budget should not collapse: %q", c)
	}
}
