package slack

import (
	"testing"
	"time"

	"github.com/familiar/gateway/internal/config"
)

func TestSplitMessage_Short(t *testing.T) {
	chunks := splitMessage("hello", 4000)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("expected 1 chunk 'hello', got %v", chunks)
	}
}

func TestSplitMessage_ExactLimit(t *testing.T) {
	msg := make([]byte, 4000)
	for i := range msg {
		msg[i] = 'a'
	}
	chunks := splitMessage(string(msg), 4000)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestSplitMessage_SplitsAtNewline(t *testing.T) {
	// Build a message that's 150 chars: 70 chars + newline + 79 chars.
	line1 := make([]byte, 70)
	for i := range line1 {
		line1[i] = 'a'
	}
	line2 := make([]byte, 79)
	for i := range line2 {
		line2[i] = 'b'
	}
	msg := string(line1) + "\n" + string(line2)

	chunks := splitMessage(msg, 100)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	// First chunk should end at the newline boundary.
	if chunks[0] != string(line1)+"\n" {
		t.Errorf("chunk[0] = %q, want %q", chunks[0], string(line1)+"\n")
	}
	if chunks[1] != string(line2) {
		t.Errorf("chunk[1] = %q, want %q", chunks[1], string(line2))
	}
}

func TestSplitMessage_NoNewline(t *testing.T) {
	msg := make([]byte, 200)
	for i := range msg {
		msg[i] = 'x'
	}
	chunks := splitMessage(string(msg), 100)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if len(chunks[0]) != 100 {
		t.Errorf("chunk[0] len = %d, want 100", len(chunks[0]))
	}
	if len(chunks[1]) != 100 {
		t.Errorf("chunk[1] len = %d, want 100", len(chunks[1]))
	}
}

func TestSplitMessage_Empty(t *testing.T) {
	chunks := splitMessage("", 4000)
	if len(chunks) != 1 || chunks[0] != "" {
		t.Errorf("expected 1 empty chunk, got %v", chunks)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("truncate = %q, want 'hello...'", got)
	}
	if got := truncate("hi", 5); got != "hi" {
		t.Errorf("truncate = %q, want 'hi'", got)
	}
}

// slackSessionID is a pure function — these tests exercise the spec
// §1.8 derivation rules directly without a live slack client.

func TestSlackSessionID_Length(t *testing.T) {
	id := slackSessionID("U123", "", true)
	if len(id) != 16 {
		t.Errorf("length = %d, want 16", len(id))
	}
}

func TestSlackSessionID_DMStableWithinDay(t *testing.T) {
	// Two calls in the same test run → same UTC date → identical id.
	a := slackSessionID("U123", "", true)
	b := slackSessionID("U123", "", true)
	if a != b {
		t.Errorf("same-day DM ids differ: %s vs %s", a, b)
	}
}

func TestSlackSessionID_DMDifferentUsersDiffer(t *testing.T) {
	a := slackSessionID("U123", "", true)
	b := slackSessionID("U999", "", true)
	if a == b {
		t.Errorf("different users produced same id: %s", a)
	}
}

func TestSlackSessionID_ThreadStable(t *testing.T) {
	// Thread anchor dominates — id must not depend on the wall clock.
	ts := "1700000000.000100"
	a := slackSessionID("U123", ts, false)
	b := slackSessionID("U123", ts, false)
	if a != b {
		t.Errorf("thread ids differ for identical inputs: %s vs %s", a, b)
	}
}

func TestSlackSessionID_ThreadDiffersFromDM(t *testing.T) {
	ts := "1700000000.000100"
	dm := slackSessionID("U123", "", true)
	thread := slackSessionID("U123", ts, false)
	if dm == thread {
		t.Errorf("DM and thread produced same id: %s", dm)
	}
}

func TestSlackSessionID_DifferentThreadsDiffer(t *testing.T) {
	a := slackSessionID("U123", "1700000000.000100", false)
	b := slackSessionID("U123", "1700000001.000200", false)
	if a == b {
		t.Errorf("distinct thread ts produced same id: %s", a)
	}
}

func TestSlackSessionID_DMRotatesOnDateChange(t *testing.T) {
	// The DM branch uses time.Now().UTC(). Re-derive the expected id via
	// the same code path and compare to an id computed with a different
	// frozen date seed to show the rotation is date-driven.
	//
	// Because slackSessionID takes no clock injection we can't advance
	// time directly — instead we assert that swapping "isDM=true,
	// threadTS=''" for the thread form with today's date still differs
	// from the next day's date when computed manually. This catches the
	// case where someone refactors the seed and drops the date.
	today := time.Now().UTC().Format("2006-01-02")
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	if today == tomorrow {
		t.Fatal("date math broke")
	}
	// Sentinel: the DM id should match the thread-form id keyed off the
	// UTC date string, confirming the DM branch is in fact date-keyed.
	dm := slackSessionID("U123", "", true)
	threadLikeToday := slackSessionID("U123", today, false)
	// These pass through different seeds ("slack:U:today" vs
	// "slack:U:today") — they should actually be equal because both
	// branches assemble the same seed when the thread form is passed
	// today's date string.
	if dm != threadLikeToday {
		t.Errorf("DM id not derived from UTC date: dm=%s thread=%s", dm, threadLikeToday)
	}
}

// External conversation keys (SLACK-CONTEXT). DMs key per-user
// (stable across days); threads key per (channel, ts). The DM form
// MUST equal DMExternalKey so the inbound adapter and the scheduled
// slack_dm deliverer resolve the same conversation.

func TestDMExternalKey_StablePerUser(t *testing.T) {
	if got := DMExternalKey("operator"); got != "slack:dm:operator" {
		t.Errorf("DMExternalKey = %q, want slack:dm:operator", got)
	}
	if DMExternalKey("a") == DMExternalKey("b") {
		t.Error("different users share a DM key")
	}
}

func TestSlackExternalKey_DMMatchesDMExternalKey(t *testing.T) {
	// The adapter (slackExternalKey) and the deliverer (DMExternalKey)
	// must agree — a drift here silently splits the digest and the
	// reply into two conversations.
	key, title := slackExternalKey("operator", "D123", "", true)
	if key != DMExternalKey("operator") {
		t.Errorf("adapter DM key %q != deliverer key %q", key, DMExternalKey("operator"))
	}
	if title == "" {
		t.Error("DM conversation should get a title on first create")
	}
}

func TestSlackExternalKey_ThreadPerChannelAndTS(t *testing.T) {
	a, _ := slackExternalKey("u", "C1", "171.5", false)
	b, _ := slackExternalKey("u", "C1", "171.6", false)
	c, _ := slackExternalKey("u", "C2", "171.5", false)
	if a == b || a == c {
		t.Errorf("thread keys not distinct by ts/channel: a=%s b=%s c=%s", a, b, c)
	}
	if a != "slack:thread:C1:171.5" {
		t.Errorf("thread key shape = %q", a)
	}
}

// userAllowed: empty allowlist → permissive; non-empty → exact match.

func newTestAdapter(allowed []string) *SlackAdapter {
	return &SlackAdapter{cfg: config.SlackConfig{AllowedUsers: allowed}}
}

func TestUserAllowed_EmptyListAllowsEveryone(t *testing.T) {
	a := newTestAdapter(nil)
	if !a.userAllowed("U123") {
		t.Error("empty allowlist should permit any user")
	}
	if !a.userAllowed("") {
		t.Error("empty allowlist should permit even empty id")
	}
}

func TestUserAllowed_ExplicitListExactMatch(t *testing.T) {
	a := newTestAdapter([]string{"U111", "U222"})
	if !a.userAllowed("U111") {
		t.Error("U111 should be allowed")
	}
	if !a.userAllowed("U222") {
		t.Error("U222 should be allowed")
	}
}

func TestUserAllowed_ExplicitListRejectsOthers(t *testing.T) {
	a := newTestAdapter([]string{"U111"})
	if a.userAllowed("U999") {
		t.Error("U999 should be denied")
	}
	if a.userAllowed("") {
		t.Error("empty id should be denied when allowlist set")
	}
}

func TestUserAllowed_CaseSensitive(t *testing.T) {
	// Slack user IDs are case-sensitive (U vs u); match must reflect that.
	a := newTestAdapter([]string{"U111"})
	if a.userAllowed("u111") {
		t.Error("lowercase variant should not match")
	}
}

// channelAllowed mirror for completeness — it's a sibling of userAllowed
// and the two filters must behave consistently.

func TestChannelAllowed(t *testing.T) {
	a := &SlackAdapter{cfg: config.SlackConfig{Channels: []string{"C1", "C2"}}}
	if !a.channelAllowed("C1") {
		t.Error("C1 should be allowed")
	}
	if a.channelAllowed("C999") {
		t.Error("C999 should be denied")
	}
}
