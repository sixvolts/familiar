package ctxbuild

import (
	"testing"

	"github.com/familiar/gateway/internal/session"
)

// A window cut inside a tool exchange opens on the exchange's tool
// results, whose assistant tool call fell outside it. The kept window
// must start at a user turn; the orphans are evicted with the rest.
func TestFitConversation_WindowOpensOnAUserTurn(t *testing.T) {
	turns := []session.Turn{
		turn("user", 40),
		{Role: "assistant", ToolCalls: []byte(`[{"id":"c1","name":"read_page","arguments":{}}]`)},
		{Role: "tool", Content: repeat(40), ToolCallID: "c1"},
		turn("assistant", 40),
		turn("user", 40),
		turn("assistant", 40),
	}
	// Room for the last four turns by content: tool, assistant, user,
	// assistant. The cut lands on the tool result.
	_, kept, evicted := fitConversation("", turns, 40)
	if len(kept) == 0 || kept[0].Role != "user" {
		roles := make([]string, len(kept))
		for i, k := range kept {
			roles[i] = k.Role
		}
		t.Fatalf("kept window opens on %v, want a user turn first", roles)
	}
	if len(kept)+len(evicted) != len(turns) {
		t.Errorf("kept %d + evicted %d != %d turns", len(kept), len(evicted), len(turns))
	}
}

// A tool-calling turn carries its payload in ToolCalls (a whole page
// for update_page) and has no text. Counting only Content, a 40 KB call
// cost nothing, was always kept, and the real prompt overran the packed
// estimate by 10k tokens.
func TestFitConversation_CountsToolCallArguments(t *testing.T) {
	args := `[{"id":"c1","name":"update_page","arguments":{"body":"` + repeat(40_000) + `"}}]`
	turns := []session.Turn{
		turn("user", 400),
		{Role: "assistant", ToolCalls: []byte(args)},
		{Role: "tool", Content: "updated", ToolCallID: "c1"},
		turn("assistant", 400),
		turn("user", 400),
		turn("assistant", 400),
	}
	_, kept, _ := fitConversation("", turns, 4000)
	for _, k := range kept {
		if len(k.ToolCalls) > 0 {
			t.Fatalf("a 10k-token tool call was kept in a 4000-token window")
		}
	}
	if len(kept) != 2 {
		t.Errorf("kept %d turns, want the last exchange (2)", len(kept))
	}

	// And a kept one is counted in the breakdown.
	small := []session.Turn{turn("user", 40), {Role: "assistant", ToolCalls: []byte(`[{"id":"c","name":"x","arguments":{"q":"` + repeat(400) + `"}}]`)}}
	out := New(Config{WindowSize: 32768, OutputReservation: 4096, SystemPromptRatio: 0.1, MemoryRatio: 0.1, ToolResultRatio: 0.1}).Build(Input{Turns: small})
	if out.TokenUsage.Conversation < 100 {
		t.Errorf("breakdown counts %d conversation tokens, want the call's ~110", out.TokenUsage.Conversation)
	}
}

// The personality prompt and relationship lines are sent whole, so they
// come out of the conversation zone: both were packed as if free.
func TestBuild_ReservesUserPromptAndRelationshipLines(t *testing.T) {
	var turns []session.Turn
	for i := 0; i < 60; i++ {
		turns = append(turns, turn("user", 200)) // 50 tokens each, more than the 2800-token zone
	}
	cfg := Config{WindowSize: 4000, SystemPromptRatio: 0.1, MemoryRatio: 0.1, ToolResultRatio: 0.1}
	plain := New(cfg).Build(Input{Turns: turns})
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = repeat(80) // ~21 tokens a line
	}
	loaded := New(cfg).Build(Input{Turns: turns, UserPrompt: repeat(1600), RelationshipLines: lines})
	// 400 + 420 reserved tokens is 16 turns.
	if len(loaded.RecentTurns) > len(plain.RecentTurns)-16 {
		t.Errorf("kept %d turns with a 400-token prompt and 420 tokens of triples, %d without: not reserved",
			len(loaded.RecentTurns), len(plain.RecentTurns))
	}
	if loaded.TokenUsage.Memories < 420 {
		t.Errorf("breakdown counts %d memory-zone tokens, want the triples' 420", loaded.TokenUsage.Memories)
	}
	if loaded.TokenUsage.Total > loaded.TokenUsage.Budget {
		t.Errorf("packed %d tokens into a %d budget", loaded.TokenUsage.Total, loaded.TokenUsage.Budget)
	}
}
