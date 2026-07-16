package ctxbuild

import (
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/session"
)

// repeat returns a string of n 'x' characters. EstimateTokens(repeat(n)) == n/4.
func repeat(n int) string { return strings.Repeat("x", n) }

// turn is a small helper so tests read naturally.
func turn(role string, bytes int) session.Turn {
	return session.Turn{Role: role, Content: repeat(bytes), Timestamp: time.Now()}
}

func TestResolveBudgetRatios(t *testing.T) {
	cfg := DefaultConfig()
	b := cfg.Resolve()

	if b.Total != 32768-4096 {
		t.Fatalf("Total = %d, want %d", b.Total, 32768-4096)
	}
	// Sanity: every zone under Total, conv = remainder.
	sum := b.System + b.Memories + b.Tools + b.Conversation
	if sum != b.Total {
		t.Fatalf("zone sum %d != total %d", sum, b.Total)
	}
	if b.Conversation <= 0 {
		t.Fatalf("conversation zone should be positive, got %d", b.Conversation)
	}
	// Default ratios give conversation ~61% of remaining.
	if b.Conversation < b.System || b.Conversation < b.Memories {
		t.Fatalf("conversation should dominate, got %+v", b)
	}
}

func TestResolveOversubscribedRatiosClampConversation(t *testing.T) {
	cfg := Config{
		WindowSize:        1000,
		OutputReservation: 0,
		SystemPromptRatio: 0.5,
		MemoryRatio:       0.4,
		ToolResultRatio:   0.4, // sums to 1.3
	}
	b := cfg.Resolve()
	if b.Conversation != 0 {
		t.Fatalf("Conversation should clamp to 0, got %d", b.Conversation)
	}
}

func TestBuildFitsSystemPromptAndMemoriesWithinZones(t *testing.T) {
	cfg := Config{
		WindowSize: 4000, OutputReservation: 0,
		SystemPromptRatio: 0.1,
		MemoryRatio:       0.2, ToolResultRatio: 0.1,
	}
	// Budgets: sys=400, mem=800, tools=400, conv=2400 tokens.
	b := New(cfg)

	in := Input{
		SystemPrompt: repeat(2000), // 500 tokens → over 400 budget, should truncate
		Memories: []Memory{
			{Content: repeat(2000)}, // 500 tokens
			{Content: repeat(1200)}, // 300 tokens → together 800, fits
			{Content: repeat(400)},  // 100 tokens → would exceed 800, dropped
		},
	}
	out := b.Build(in)

	if got := EstimateTokens(out.SystemPrompt); got > 400 {
		t.Fatalf("system prompt not truncated: %d tokens", got)
	}
	if len(out.Memories) != 2 {
		t.Fatalf("expected 2 memories kept, got %d", len(out.Memories))
	}
	if out.TokenUsage.Memories > 800 {
		t.Fatalf("memory zone over budget: %d", out.TokenUsage.Memories)
	}
}

func TestBuildEvictsOldestTurnsNewestFirstKept(t *testing.T) {
	cfg := Config{
		WindowSize: 1000, OutputReservation: 0,
		// Zero out fixed zones so conversation = 1000 tokens.
	}
	b := New(cfg)

	// 10 turns of 200 bytes = 50 tokens each = 500 tokens total. All fit.
	var turns []session.Turn
	for i := 0; i < 10; i++ {
		turns = append(turns, turn("user", 200))
	}
	out := b.Build(Input{Turns: turns})
	if len(out.RecentTurns) != 10 || len(out.EvictedTurns) != 0 {
		t.Fatalf("all should fit: kept=%d evicted=%d", len(out.RecentTurns), len(out.EvictedTurns))
	}

	// Now 30 turns of 200 bytes = 50 tokens each = 1500 tokens total.
	// Budget is 1000, so ~20 newest kept, 10 oldest evicted.
	turns = nil
	for i := 0; i < 30; i++ {
		turns = append(turns, session.Turn{
			Role:    "user",
			Content: repeat(200),
		})
	}
	out = b.Build(Input{Turns: turns})
	if len(out.RecentTurns)+len(out.EvictedTurns) != 30 {
		t.Fatalf("lost turns: kept=%d evicted=%d", len(out.RecentTurns), len(out.EvictedTurns))
	}
	if len(out.EvictedTurns) == 0 {
		t.Fatalf("expected some turns to be evicted")
	}
	if out.TokenUsage.Conversation > 1000 {
		t.Fatalf("conversation over budget: %d", out.TokenUsage.Conversation)
	}
	// Evicted must be the oldest (contiguous prefix).
	expectedEvictCount := len(out.EvictedTurns)
	for i := 0; i < expectedEvictCount; i++ {
		if &out.EvictedTurns[i] == nil {
			t.Fatalf("nil evicted turn at %d", i)
		}
	}
}

func TestBuildSummary60_40Split(t *testing.T) {
	cfg := Config{WindowSize: 1000, OutputReservation: 0}
	b := New(cfg)

	// Conv budget = 1000. With summary: turns get 60%=600, summary gets 40%=400.
	// 20 turns of 200 bytes = 50 tokens each = 1000 tokens total — more than
	// turnBudget (600) so some should be evicted even though total history
	// would fit in the full conversation zone.
	var turns []session.Turn
	for i := 0; i < 20; i++ {
		turns = append(turns, turn("user", 200))
	}
	out := b.Build(Input{
		Summary: repeat(2000), // 500 tokens → will truncate to 400
		Turns:   turns,
	})
	if out.ConversationSummary == "" {
		t.Fatalf("summary should be kept")
	}
	if EstimateTokens(out.ConversationSummary) > 400 {
		t.Fatalf("summary over its 40%% budget: %d tokens", EstimateTokens(out.ConversationSummary))
	}
	// Without the 60/40 split all 20 turns (1000 tokens) would fit. With the
	// split, turnBudget is 600, so at most 12 turns fit.
	if len(out.RecentTurns) > 12 {
		t.Fatalf("60/40 split not enforced: %d turns kept", len(out.RecentTurns))
	}
	if len(out.EvictedTurns) == 0 {
		t.Fatalf("expected evictions due to summary reservation")
	}
}

func TestBuildNoSummaryUsesFullConversationZone(t *testing.T) {
	cfg := Config{WindowSize: 1000, OutputReservation: 0}
	b := New(cfg)

	// 15 turns × 200 bytes = 50 tokens × 15 = 750 tokens. No summary → all fit.
	var turns []session.Turn
	for i := 0; i < 15; i++ {
		turns = append(turns, turn("user", 200))
	}
	out := b.Build(Input{Turns: turns})
	if len(out.RecentTurns) != 15 {
		t.Fatalf("all turns should fit without summary, kept %d", len(out.RecentTurns))
	}
	if len(out.EvictedTurns) != 0 {
		t.Fatalf("no evictions expected, got %d", len(out.EvictedTurns))
	}
}

func TestBuildToolResultsDropOldest(t *testing.T) {
	cfg := Config{
		WindowSize: 2000, OutputReservation: 0,
		ToolResultRatio: 0.25, // 500 tokens
	}
	b := New(cfg)

	results := []ToolResult{
		{Name: "oldest", Content: repeat(1600)}, // 400 tokens
		{Name: "middle", Content: repeat(800)},  // 200 tokens
		{Name: "newest", Content: repeat(400)},  // 100 tokens
	}
	// Budget 500. Newest-first: 100 + 200 = 300 fits; adding 400 → 700 > 500.
	// So keep middle + newest.
	out := b.Build(Input{ToolResults: results})
	if len(out.ToolResults) != 2 {
		t.Fatalf("expected 2 tool results kept, got %d", len(out.ToolResults))
	}
	if out.ToolResults[0].Name != "middle" || out.ToolResults[1].Name != "newest" {
		t.Fatalf("wrong results kept: %+v", out.ToolResults)
	}
}

// A single oversized tool result must be head+tail capped before it
// reaches the zone — without the cap one giant payload is kept whole
// (it's the always-keep newest entry) and monopolises the budget.
func TestBuildToolResultPerResultCap(t *testing.T) {
	cfg := Config{
		WindowSize: 8000, OutputReservation: 0,
		ToolResultRatio:     0.5, // 4000-token zone
		MaxToolResultTokens: 100, // cap each result at ~400 bytes
	}
	b := New(cfg)

	huge := repeat(40000) // 10000 tokens raw — way over the 100 cap
	out := b.Build(Input{ToolResults: []ToolResult{{Name: "search", Content: huge}}})
	if len(out.ToolResults) != 1 {
		t.Fatalf("expected the result kept (capped), got %d", len(out.ToolResults))
	}
	got := out.ToolResults[0].Content
	if EstimateTokens(got) > 110 {
		t.Errorf("result not capped: %d tokens", EstimateTokens(got))
	}
	if !strings.Contains(got, "characters elided") {
		t.Errorf("capped result missing elision marker: %q", got)
	}
	// Head + tail of the original must both survive.
	if !strings.HasPrefix(got, huge[:50]) {
		t.Error("capped result lost its head")
	}
	if !strings.HasSuffix(got, huge[len(huge)-50:]) {
		t.Error("capped result lost its tail")
	}
}

func TestBuildTokenBreakdownMatchesAssembled(t *testing.T) {
	cfg := DefaultConfig()
	b := New(cfg)

	out := b.Build(Input{
		SystemPrompt: repeat(400),
		Memories:     []Memory{{Content: repeat(200)}},
		Turns: []session.Turn{
			turn("user", 100),
			turn("assistant", 100),
		},
	})

	wantSys := EstimateTokens(out.SystemPrompt)
	wantMem := EstimateTokens(out.Memories[0].Content)
	wantConv := 0
	for _, t := range out.RecentTurns {
		wantConv += EstimateTokens(t.Content)
	}
	wantTotal := wantSys + wantMem + wantConv
	if out.TokenUsage.Total != wantTotal {
		t.Fatalf("breakdown Total=%d want %d", out.TokenUsage.Total, wantTotal)
	}
	if out.TokenUsage.Headroom != out.TokenUsage.Budget-wantTotal {
		t.Fatalf("Headroom mismatch: %+v", out.TokenUsage)
	}
}

func TestBuildReservedTokensShrinksConversation(t *testing.T) {
	cfg := Config{WindowSize: 1000, OutputReservation: 0}
	b := New(cfg)

	// 10 turns × 200 bytes = 50 tokens each = 500 tokens, all fit in 1000.
	var turns []session.Turn
	for i := 0; i < 10; i++ {
		turns = append(turns, turn("user", 200))
	}
	// Reserve 600 tokens — conversation zone shrinks from 1000 → 400, so
	// only ~8 turns (400 tokens) can fit.
	out := b.Build(Input{Turns: turns, ReservedTokens: 600})
	if len(out.RecentTurns) > 8 {
		t.Fatalf("reservation not honored: %d turns kept", len(out.RecentTurns))
	}
	if len(out.EvictedTurns) == 0 {
		t.Fatalf("expected evictions under reservation pressure")
	}
	if out.TokenUsage.Conversation > 400 {
		t.Fatalf("conversation exceeded reserved budget: %d", out.TokenUsage.Conversation)
	}
}

func TestBuildZeroConversationZoneEvictsEverything(t *testing.T) {
	cfg := Config{
		WindowSize:        1000,
		OutputReservation: 0,
		SystemPromptRatio: 0.5, MemoryRatio: 0.5, // conv = 0
	}
	b := New(cfg)
	in := Input{
		Turns: []session.Turn{turn("user", 40), turn("assistant", 40)},
	}
	out := b.Build(in)
	if len(out.RecentTurns) != 0 {
		t.Fatalf("no turns should fit when conversation zone is 0")
	}
	if len(out.EvictedTurns) != 2 {
		t.Fatalf("both turns should be evicted, got %d", len(out.EvictedTurns))
	}
}
