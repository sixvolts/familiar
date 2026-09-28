package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/skills"
	pb "github.com/familiar/gateway/proto/engine"
)

// forget_fact and correct_fact act on a query only when it clearly
// names one memory. They used to take the nearest row at any score, so
// "forget my sister's birthday", with no such memory, deleted "Mom's
// birthday is May 5".

// rankedEngine serves fixed semantic matches and records edits.
type rankedEngine struct {
	fakeEngine
	results []*pb.MemoryResultProto
	updated []string
}

func (e *rankedEngine) QueryMemory(context.Context, *pb.MemoryQueryRequest) (*pb.MemoryQueryResponse, error) {
	return &pb.MemoryQueryResponse{Results: e.results}, nil
}

func (e *rankedEngine) UpdateFact(_ context.Context, _, id, _ string, _ []float32, _ *pb.VisibilityContext) (*pb.UpdateFactResponse, error) {
	e.updated = append(e.updated, id)
	return &pb.UpdateFactResponse{Updated: true}, nil
}

func match(id, content string, score float32) *pb.MemoryResultProto {
	return &pb.MemoryResultProto{Fact: &pb.FactProto{Id: id, Content: content}, RelevanceScore: score}
}

// matchFixture: alice owns every row except "global-row".
func matchFixture(results ...*pb.MemoryResultProto) (*Skill, *rankedEngine, *fakeManager) {
	eng := &rankedEngine{results: results}
	mgr := &fakeManager{owners: map[string]string{}}
	for _, r := range results {
		if r.Fact.Id != "global-row" {
			mgr.owners[r.Fact.Id] = "alice"
		}
	}
	embed := func(context.Context, string) ([]float32, error) { return []float32{1}, nil }
	return New(eng, nil, embed, WithManager(mgr)), eng, mgr
}

func aliceCtx() context.Context {
	return skills.WithContext(context.Background(), skills.SessionContext{UserID: "alice"})
}

func TestForgetFact_WeakMatchAsksForID(t *testing.T) {
	s, _, mgr := matchFixture(match("mom-bday", "Mom's birthday is May 5", 0.62))
	res, err := s.Execute(aliceCtx(), "forget_fact", json.RawMessage(`{"query":"my sister's birthday"}`))
	if err != nil {
		t.Fatal(err)
	}
	if mgr.deleteCalled {
		t.Fatalf("a weak match was deleted: %q", res.Content)
	}
	if !strings.Contains(res.Content, "mom-bday") || !strings.Contains(res.Content, "nothing was changed") {
		t.Errorf("want the candidate listed and nothing changed, got %q", res.Content)
	}
}

func TestForgetFact_CloseRunnerUpAsksForID(t *testing.T) {
	s, _, mgr := matchFixture(
		match("mom-bday", "Mom's birthday is May 5", 0.86),
		match("dad-bday", "Dad's birthday is June 2", 0.84),
	)
	res, _ := s.Execute(aliceCtx(), "forget_fact", json.RawMessage(`{"query":"the birthday"}`))
	if mgr.deleteCalled {
		t.Fatalf("an ambiguous match was deleted: %q", res.Content)
	}
	if !strings.Contains(res.Content, "mom-bday") || !strings.Contains(res.Content, "dad-bday") {
		t.Errorf("want both candidates listed, got %q", res.Content)
	}
}

func TestForgetFact_BelowFloorIsNotFound(t *testing.T) {
	s, _, mgr := matchFixture(match("tea", "User likes tea", 0.31))
	res, _ := s.Execute(aliceCtx(), "forget_fact", json.RawMessage(`{"query":"my sister's birthday"}`))
	if mgr.deleteCalled || !strings.Contains(res.Content, "No memory found") {
		t.Errorf("an unrelated row: deleted=%v content=%q, want not found", mgr.deleteCalled, res.Content)
	}
}

func TestForgetFact_ClearMatchDeletes(t *testing.T) {
	// The global row outranks alice's but isn't hers to delete, so it
	// is neither a candidate nor the runner-up.
	s, _, mgr := matchFixture(
		match("global-row", "Office wifi password", 0.93),
		match("wifi", "Home wifi password is hunter2", 0.91),
		match("tea", "User likes tea", 0.55),
	)
	res, err := s.Execute(aliceCtx(), "forget_fact", json.RawMessage(`{"query":"my wifi password"}`))
	if err != nil {
		t.Fatal(err)
	}
	if mgr.lastDelID != "wifi" {
		t.Errorf("clear match: deleted %q (content %q), want wifi", mgr.lastDelID, res.Content)
	}
}

func TestCorrectFact_WeakMatchAsksForID(t *testing.T) {
	s, eng, _ := matchFixture(match("mom-bday", "Mom's birthday is May 5", 0.62))
	res, err := s.Execute(aliceCtx(), "correct_fact",
		json.RawMessage(`{"query":"my sister's birthday","new_content":"Sister's birthday is June 3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.updated) != 0 {
		t.Fatalf("a weak match was overwritten: %q", res.Content)
	}
	if !strings.Contains(res.Content, "mom-bday") {
		t.Errorf("want the candidate listed, got %q", res.Content)
	}
}

// Given the id from that list, correct_fact edits that row, still only
// if it is the caller's.
func TestCorrectFact_ExplicitID(t *testing.T) {
	s, eng, _ := matchFixture(match("mom-bday", "Mom's birthday is May 5", 0.62), match("global-row", "x", 0.1))
	if _, err := s.Execute(aliceCtx(), "correct_fact",
		json.RawMessage(`{"id":"mom-bday","new_content":"Mom's birthday is May 6"}`)); err != nil {
		t.Fatal(err)
	}
	if len(eng.updated) != 1 || eng.updated[0] != "mom-bday" {
		t.Fatalf("explicit id: updated %v, want [mom-bday]", eng.updated)
	}
	res, _ := s.Execute(aliceCtx(), "correct_fact",
		json.RawMessage(`{"id":"global-row","new_content":"y"}`))
	if len(eng.updated) != 1 || !strings.Contains(res.Content, "not found") {
		t.Errorf("an id the caller doesn't own was edited: updated=%v content=%q", eng.updated, res.Content)
	}
}
