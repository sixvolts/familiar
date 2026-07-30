package memengine

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/familiar/gateway/proto/engine"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func factFor(userID, content string) *pb.FactProto {
	now := time.Now()
	return &pb.FactProto{
		Content:      content,
		UserId:       userID,
		Scope:        "user",
		SourceType:   "test",
		CreatedAt:    timestamppb.New(now),
		LastAccessed: timestamppb.New(now),
	}
}

func countMemories(t *testing.T, e *MemEngine) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("counting memories: %v", err)
	}
	return n
}

// A batch where one row violates a constraint must REPORT the failure while
// still landing the rows that were fine. Before this, per-row failures were
// recorded only on resp.Error — which no call site reads — so the write
// silently vanished and `remember` still answered "Got it, I'll remember
// that."
//
// The failing row here has an empty user_id, which the memories table
// rejects (NOT NULL, plus CHECK user_id <> ”).
func TestCommitFactsPartialFailureReportsError(t *testing.T) {
	e := setupReembedTest(t) // isolated schema, migrated
	ctx := context.Background()

	resp, err := e.CommitFacts(ctx, "sess-partial", []*pb.FactProto{
		factFor("user-ok", "this one is fine"),
		factFor("", "this one has no user and must fail"),
		factFor("user-ok", "this one is also fine"),
	})

	if err == nil {
		t.Fatal("a batch with a failing row must return an error — silence is what let callers confirm phantom saves")
	}
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("error should say how many failed, got: %v", err)
	}
	if resp == nil {
		t.Fatal("response must still be returned alongside the error")
	}
	// Partial success is preserved: one bad fact must not discard the others.
	if resp.Committed != 2 {
		t.Errorf("Committed = %d, want 2 (the two valid rows)", resp.Committed)
	}
	if n := countMemories(t, e); n != 2 {
		t.Errorf("%d rows in memories, want 2 — the good rows should still land", n)
	}
}

// The happy path must stay quiet, or every caller starts logging noise and
// the signal is lost.
func TestCommitFactsAllSucceedNoError(t *testing.T) {
	e := setupReembedTest(t)
	resp, err := e.CommitFacts(context.Background(), "sess-ok", []*pb.FactProto{
		factFor("user-ok", "alpha"),
		factFor("user-ok", "beta"),
	})
	if err != nil {
		t.Fatalf("a fully successful commit must not return an error: %v", err)
	}
	if resp.Committed != 2 {
		t.Errorf("Committed = %d, want 2", resp.Committed)
	}
	if resp.Error != "" {
		t.Errorf("resp.Error = %q, want empty", resp.Error)
	}
}

// An empty batch is a no-op, not a failure — callers pass whatever
// extraction produced, including nothing.
func TestCommitFactsEmptyBatchIsNotAnError(t *testing.T) {
	e := setupReembedTest(t)
	resp, err := e.CommitFacts(context.Background(), "sess-empty", nil)
	if err != nil {
		t.Fatalf("empty batch should not error: %v", err)
	}
	if resp.Committed != 0 {
		t.Errorf("Committed = %d, want 0", resp.Committed)
	}
}

// Every row failing must error too, and land nothing.
func TestCommitFactsAllFailReportsError(t *testing.T) {
	e := setupReembedTest(t)
	resp, err := e.CommitFacts(context.Background(), "sess-allbad", []*pb.FactProto{
		factFor("", "no user a"),
		factFor("", "no user b"),
	})
	if err == nil {
		t.Fatal("expected an error when every row fails")
	}
	if !strings.Contains(err.Error(), "2 of 2") {
		t.Errorf("error should report 2 of 2, got: %v", err)
	}
	if resp.Committed != 0 {
		t.Errorf("Committed = %d, want 0", resp.Committed)
	}
	if n := countMemories(t, e); n != 0 {
		t.Errorf("%d rows landed, want 0", n)
	}
}
