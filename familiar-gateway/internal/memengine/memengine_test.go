package memengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	resp, err := e.AssembleContext(context.Background(), "sess-1", "hi", &pb.VisibilityContext{}, nil)
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
	resp, _ := e.AssembleContext(context.Background(), "sess", "hi", &pb.VisibilityContext{UserId: "u"}, nil)
	if len(resp.MemoryContext) != 0 {
		t.Errorf("expected no memory hits without query vector; got %d", len(resp.MemoryContext))
	}
}

// AssembleContext no longer retrieves memory at all — even with a store
// that WOULD return a hit and a non-nil query vector, its MemoryContext
// must be empty. The pipeline's searchPgVector is the sole memory
// authority now; this pins that the engine's dense pass stays retired.
func TestAssembleContextNeverRetrievesMemory(t *testing.T) {
	e := New(nil, hitMemStore{}, nil, "")
	resp, err := e.AssembleContext(context.Background(), "sess", "hi",
		&pb.VisibilityContext{UserId: "u"}, []float32{0.1, 0.2, 0.3})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	if len(resp.MemoryContext) != 0 {
		t.Fatalf("engine retrieved memory (%d hits) — the dense pass should be gone", len(resp.MemoryContext))
	}
}

func TestCommitFactsNoPoolReturnsError(t *testing.T) {
	// A commit with no pool stores nothing, so it must SAY so. The old
	// contract here returned (resp, nil) with the reason only on
	// resp.Error — a field no call site reads — which is how `remember`
	// came to answer "Got it, I'll remember that" for writes that never
	// happened. Callers all branch on err, so err is what has to carry it.
	e := New(nil, nil, nil, "")
	resp, err := e.CommitFacts(context.Background(), "sess", []*pb.FactProto{{Content: "x"}})
	if err == nil {
		t.Fatal("expected an error when the pool is unwired — a silent no-op lets callers confirm a phantom save")
	}
	if resp == nil {
		t.Fatal("response must still be non-nil so callers can read Committed")
	}
	if resp.Error == "" {
		t.Error("expected the reason on resp.Error too (kept for the admin surfaces)")
	}
	if resp.Committed != 0 {
		t.Errorf("Committed = %d, want 0", resp.Committed)
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
func (stubMemStore) NearestLiveFacts(ctx context.Context, vector []float32, userID, scopeTag string, limit int) ([]memory.NearestFact, error) {
	return nil, nil
}
func (stubMemStore) ReinforceFacts(ctx context.Context, ids []string) error { return nil }
func (stubMemStore) Close() error                                           { return nil }

// hitMemStore returns a memory hit for any query — used to prove the
// engine's retrieval path is truly gone (a returned hit must NOT surface).
type hitMemStore struct{ stubMemStore }

func (hitMemStore) Search(ctx context.Context, vector []float32, limit int, threshold float64, userID string) ([]memory.MemoryResult, error) {
	return []memory.MemoryResult{{ID: "m1", Content: "would-be memory", Similarity: 0.99}}, nil
}
func (hitMemStore) HybridSearch(ctx context.Context, queryText string, vector []float32, limit int, threshold float64, userID string) ([]memory.MemoryResult, error) {
	return []memory.MemoryResult{{ID: "m1", Content: "would-be memory", Similarity: 0.99}}, nil
}

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
	h := func(user, content string) string { return factHash(user, "", "", "", content) }

	if h("alice", content) == h("bob", content) {
		t.Error("different users with identical content produced the same hash — cross-tenant dedup collision")
	}
	if h("alice", content) != h("alice", content) {
		t.Error("same user + same content must hash identically for dedup to work")
	}
	if h("", content) != h("", content) {
		t.Error("global rows must hash consistently among themselves")
	}
	// The NUL separator prevents boundary ambiguity.
	if h("ab", "c") == h("a", "bc") {
		t.Error("user/content boundary is ambiguous — missing separator")
	}
}

// The scope and, for wiki rows, the owning page are part of a fact's
// identity: otherwise a shard's or a page's fact merges onto the
// owner's top-level row, and a page save deletes knowledge another
// page or an explicit `remember` also holds. Unscoped rows keep the
// original formula so every existing top-level hash still matches.
func TestFactHash_SeparatesScopesAndPages(t *testing.T) {
	const user, content = "alice", "Deploys go to staging first."
	top := factHash(user, "", "conversation_extraction", "sess-1", content)
	sum := sha256.Sum256([]byte(user + "\x00" + content))
	if top != hex.EncodeToString(sum[:]) {
		t.Error("an unscoped fact's hash changed; every existing top-level row would stop deduping")
	}
	if top != factHash(user, "", "remember", "other-session", content) {
		t.Error("source must not split top-level facts: a restatement from another session is the same fact")
	}
	shard := factHash(user, "shard:recipes", "conversation_extraction", "sess-1", content)
	if shard == top {
		t.Error("a shard's fact hashes like the owner's top-level fact; the upsert would merge them")
	}
	pageA := factHash(user, "book:b1", "wiki_page", "page:a", content)
	pageB := factHash(user, "book:b1", "wiki_page", "page:b", content)
	if pageA == pageB {
		t.Error("two pages' identical facts share a row; saving one page would delete the other's")
	}
	if pageA == factHash(user, "book:b1", "remember", "page:a", content) {
		t.Error("a non-wiki fact in the book scope hashes like the page's own fact")
	}
}
