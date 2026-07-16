package memengine

import (
	"context"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/memory"
	pb "github.com/familiar/gateway/proto/engine"
)

// Unit tests cover the surface that doesn't need a live Postgres
// pool: synthetic stubs (Ping, GetAgentIdentity, Briefing, sleep
// no-ops, vault unsupported) and degrade-when-unwired branches on
// the memory ops. SQL-touching paths (CommitFacts, DeleteFact,
// UpdateFact, AssembleContext with a query vector) are integration
// concerns deferred to a pool-backed test pass.

func TestPing(t *testing.T) {
	e := New(nil, nil, nil, "test-agent")
	resp, err := e.Ping(context.Background())
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if resp.Version != "memengine/in-process" {
		t.Errorf("version = %q", resp.Version)
	}
	if resp.MemoryTier != "pgvector" {
		t.Errorf("memory_tier = %q", resp.MemoryTier)
	}
}

func TestGetAgentIdentity(t *testing.T) {
	e := New(nil, nil, nil, "test-agent")
	id, err := e.GetAgentIdentity(context.Background())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id.AgentId != "test-agent" {
		t.Errorf("agent_id = %q", id.AgentId)
	}
	if len(id.Fingerprint) == 0 {
		t.Error("fingerprint empty")
	}
	// Fingerprint must be stable for the same agent — main.go
	// surfaces it on the boot banner and operators read it to spot
	// agent-id drift.
	id2, _ := e.GetAgentIdentity(context.Background())
	if id.Fingerprint != id2.Fingerprint {
		t.Errorf("fingerprint not stable: %q vs %q", id.Fingerprint, id2.Fingerprint)
	}
}

func TestVaultStubsReturnUnsupported(t *testing.T) {
	e := New(nil, nil, nil, "")
	if _, _, err := e.VaultGet(context.Background(), "any"); err != ErrUnsupported {
		t.Errorf("VaultGet err = %v, want ErrUnsupported", err)
	}
	if err := e.VaultSet(context.Background(), "k", "v"); err != ErrUnsupported {
		t.Errorf("VaultSet err = %v, want ErrUnsupported", err)
	}
}

func TestSleepStubs(t *testing.T) {
	e := New(nil, nil, nil, "")
	h, err := e.StartSleep(context.Background(), nil)
	if err != nil || h == "" {
		t.Errorf("StartSleep: handle=%q err=%v", h, err)
	}
	s, err := e.SleepStatus(context.Background(), h)
	if err != nil {
		t.Fatalf("SleepStatus: %v", err)
	}
	if !s.Completed {
		t.Error("idle stub should report completed=true")
	}
	if err := e.WakeSleep(context.Background(), h); err != nil {
		t.Errorf("WakeSleep: %v", err)
	}
}

func TestAssembleContextEmptyWithoutDeps(t *testing.T) {
	// With nil deps, AssembleContext should not panic and should
	// return a clean empty response. Mirrors the previous engine's
	// behavior when no memory matches.
	e := New(nil, nil, nil, "")
	resp, err := e.AssembleContext(context.Background(), "sess-1", "hi", &pb.VisibilityContext{}, 1000, 1000, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(resp.MemoryContext) != 0 {
		t.Errorf("expected zero memory hits; got %d", len(resp.MemoryContext))
	}
	if len(resp.ConversationHistory) != 0 {
		t.Errorf("expected zero conv turns; got %d", len(resp.ConversationHistory))
	}
	if resp.Error != "" {
		t.Errorf("error should be empty when degrading silently; got %q", resp.Error)
	}
}

func TestAssembleContextDegradesWithoutVector(t *testing.T) {
	// memStore wired but no query vector → memory branch skipped.
	// Mirrors the previous implementation which short-circuits on empty
	// query_vector.
	e := New(nil, stubMemStore{}, nil, "")
	resp, _ := e.AssembleContext(context.Background(), "sess", "hi", &pb.VisibilityContext{UserId: "u"}, 1000, 1000, nil)
	if len(resp.MemoryContext) != 0 {
		t.Errorf("expected no memory hits without query vector; got %d", len(resp.MemoryContext))
	}
}

func TestCommitFactsNoPoolDegrades(t *testing.T) {
	// Operator running without memory.local_dsn shouldn't crash
	// commits — the response carries an error string and the
	// caller proceeds. Same shape the gateway already accepts
	// from the gRPC engine on a transient outage.
	e := New(nil, nil, nil, "")
	resp, err := e.CommitFacts(context.Background(), "sess", []*pb.FactProto{{Content: "x"}})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected non-empty error when pool is unwired")
	}
}

// stubMemStore satisfies memory.MemoryStore for the no-vector test
// path. Real Search behavior is integration-tested elsewhere.
type stubMemStore struct{}

func (stubMemStore) Search(ctx context.Context, vector []float32, limit int, threshold float64, userID string) ([]memory.MemoryResult, error) {
	return nil, nil
}
func (stubMemStore) HybridSearch(ctx context.Context, queryText string, vector []float32, limit int, threshold float64, userID string) ([]memory.MemoryResult, error) {
	return nil, nil
}
func (stubMemStore) NearestSimilarity(ctx context.Context, vector []float32, scope string, userID string) (float64, bool, error) {
	return 0, false, nil
}
func (stubMemStore) NearestLiveFact(ctx context.Context, vector []float32, userID string) (memory.NearestFact, bool, error) {
	return memory.NearestFact{}, false, nil
}
func (stubMemStore) Close() error { return nil }

// Close must stop the consolidation cycle it owns, so a graceful
// shutdown drains the sleep goroutine instead of leaving it running
// against a closing pool. A nil-pool SleepCycle is a no-op cycle
// (Start closes doneCh immediately); Close still routes through Stop.
func TestMemEngine_CloseStopsSleepCycle(t *testing.T) {
	e := New(nil, nil, nil, "test")
	sc := NewSleepCycle(nil, "test", config.DefaultSleepConfig())
	sc.Start(context.Background())
	e.SetSleepCycle(sc)

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Second Close (mirrors the explicit-stop + deferred-Close path in
	// main) must not panic or block.
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Close on an engine with no sleep cycle wired is a clean no-op.
	if err := New(nil, nil, nil, "test").Close(); err != nil {
		t.Fatalf("Close without sleep cycle: %v", err)
	}
}

// factHash folds the owner into the content_hash so two different
// users with byte-identical fact content get different hashes and
// can't collide onto one row under the (agent_id, content_hash)
// dedup — the cross-tenant bug. Same user + same content stays
// stable (so genuine same-user duplicates still dedup), and global
// rows (empty user) share a bucket.
func TestFactHash_SeparatesUsersNotContent(t *testing.T) {
	content := "the sky is blue"

	if factHash("alice", content) == factHash("bob", content) {
		t.Error("different users with identical content produced the same hash — cross-tenant dedup collision")
	}
	if factHash("alice", content) != factHash("alice", content) {
		t.Error("same user + same content must hash identically for dedup to work")
	}
	if factHash("", content) != factHash("", content) {
		t.Error("global rows must hash consistently among themselves")
	}
	// The NUL separator prevents boundary ambiguity.
	if factHash("ab", "c") == factHash("a", "bc") {
		t.Error("user/content boundary is ambiguous — missing separator")
	}
}
