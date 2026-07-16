package scheduled

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/actions"
	"github.com/familiar/gateway/internal/skills"
)

// fakeStore is an in-memory Store: actions keyed by owner, runs keyed
// by action id. It records the ownerID the skill asked for so we can
// assert user scoping.
type fakeStore struct {
	byOwner   map[string][]*actions.Action
	runs      map[string][]*actions.Run
	askedFor  string
	listRunsN int
}

func (f *fakeStore) List(_ context.Context, ownerID string, _ bool) ([]*actions.Action, error) {
	f.askedFor = ownerID
	return f.byOwner[ownerID], nil
}

func (f *fakeStore) ListRuns(_ context.Context, actionID string, limit int) ([]*actions.Run, error) {
	f.listRunsN = limit
	rs := f.runs[actionID]
	if len(rs) > limit {
		rs = rs[:limit]
	}
	return rs, nil
}

func tPtr(t time.Time) *time.Time { return &t }

func exec(t *testing.T, s *Skill, userID string, args map[string]any) skills.ToolResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	ctx := skills.WithContext(context.Background(), skills.SessionContext{UserID: userID})
	res, err := s.Execute(ctx, "recent_scheduled_runs", raw)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func TestRecentScheduledRuns_RequiresIdentity(t *testing.T) {
	s := New(&fakeStore{})
	res := exec(t, s, "", nil)
	if res.Error == "" {
		t.Fatalf("expected error result when no user identity, got content=%q", res.Content)
	}
}

func TestRecentScheduledRuns_ReturnsVerbatimOutputForUser(t *testing.T) {
	now := time.Now()
	fs := &fakeStore{
		byOwner: map[string][]*actions.Action{
			"operator": {{ID: "act-1", OwnerID: "operator", Name: "Daily Papers"}},
			"other":    {{ID: "act-x", OwnerID: "other", Name: "Secret"}},
		},
		runs: map[string][]*actions.Run{
			"act-1": {
				{ID: "r2", ActionID: "act-1", Status: actions.RunStatusOK, FinishedAt: tPtr(now.Add(-1 * time.Hour)), Output: "Today: EvoArena, RA-RFT, Agents-K1"},
				{ID: "r1", ActionID: "act-1", Status: actions.RunStatusOK, FinishedAt: tPtr(now.Add(-25 * time.Hour)), Output: "Yesterday: EvoArena, RA-RFT, Agents-K1"},
			},
		},
	}
	s := New(fs)
	res := exec(t, s, "operator", map[string]any{"limit": 2})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if fs.askedFor != "operator" {
		t.Errorf("store.List ownerID = %q, want operator (must scope to caller)", fs.askedFor)
	}
	// Verbatim output of both runs is present so the model can compare.
	for _, want := range []string{"Today: EvoArena", "Yesterday: EvoArena", "Daily Papers"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q\n---\n%s", want, res.Content)
		}
	}
	// Newest first.
	if strings.Index(res.Content, "Today:") > strings.Index(res.Content, "Yesterday:") {
		t.Errorf("runs not newest-first:\n%s", res.Content)
	}
	// Structured data carries the runs too.
	var dtos []runDTO
	if err := json.Unmarshal(res.Data, &dtos); err != nil {
		t.Fatalf("Data not valid JSON: %v", err)
	}
	if len(dtos) != 2 {
		t.Errorf("Data len = %d, want 2", len(dtos))
	}
}

func TestRecentScheduledRuns_NameFilterAndScoping(t *testing.T) {
	fs := &fakeStore{
		byOwner: map[string][]*actions.Action{
			"operator": {
				{ID: "act-1", OwnerID: "operator", Name: "Daily Papers"},
				{ID: "act-2", OwnerID: "operator", Name: "Weather Brief"},
			},
		},
		runs: map[string][]*actions.Run{
			"act-2": {{ID: "w1", ActionID: "act-2", Status: actions.RunStatusOK, FinishedAt: tPtr(time.Now()), Output: "70F and clear"}},
		},
	}
	s := New(fs)

	// Filter to the weather action by partial, case-insensitive name.
	res := exec(t, s, "operator", map[string]any{"action_name": "weather"})
	if !strings.Contains(res.Content, "70F and clear") {
		t.Errorf("filtered content missing the weather run:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "Daily Papers") {
		t.Errorf("filter leaked the non-matching action:\n%s", res.Content)
	}

	// No match → helpful message listing available actions, no error.
	res = exec(t, s, "operator", map[string]any{"action_name": "nonsense"})
	if res.Error != "" {
		t.Errorf("no-match should be a content message, not an error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "Daily Papers") || !strings.Contains(res.Content, "Weather Brief") {
		t.Errorf("no-match message should list available actions:\n%s", res.Content)
	}
}

func TestRecentScheduledRuns_LimitClamped(t *testing.T) {
	fs := &fakeStore{
		byOwner: map[string][]*actions.Action{"operator": {{ID: "a", OwnerID: "operator", Name: "X"}}},
		runs:    map[string][]*actions.Run{},
	}
	s := New(fs)
	exec(t, s, "operator", map[string]any{"limit": 9999})
	if fs.listRunsN > maxLimit {
		t.Errorf("ListRuns limit = %d, want clamped to %d", fs.listRunsN, maxLimit)
	}
}
