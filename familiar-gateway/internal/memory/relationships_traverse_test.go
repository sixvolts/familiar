package memory

import (
	"context"
	"testing"

	"github.com/familiar/gateway/internal/testutil"
)

// seedEdges stores a fixed graph for the traversal tests. The graph
// intentionally has a 3-hop chain, a branch, and one cycle so the
// depth cap and de-duplication paths both get exercised.
//
// Note the distinct predicates on homelab's two outgoing edges: the
// schema's (subject, predicate, user_id_key) unique index means a
// second "homelab contains X" upsert would OVERWRITE the first
// (that's the on-conflict update path UpsertRelationships exists
// for), so a branch needs two different predicates to actually
// produce two edges.
func seedEdges(t *testing.T, s *PgRelationshipStore) {
	t.Helper()
	edges := []Relationship{
		{Subject: "operator", Predicate: "owns", Object: "homelab", UserID: "owner"},
		{Subject: "homelab", Predicate: "contains", Object: "gpu-host", UserID: "owner"},
		{Subject: "gpu-host", Predicate: "has_gpu", Object: "gpu-x", UserID: "owner"},
		{Subject: "homelab", Predicate: "hosts", Object: "host-a", UserID: "owner"},
		{Subject: "host-a", Predicate: "has_ip", Object: "10.0.0.40", UserID: "owner"},
		{Subject: "operator", Predicate: "drives", Object: "sedan", UserID: "owner"},
		// Cycle: host-a connects back to gpu-host.
		{Subject: "host-a", Predicate: "connects_to", Object: "gpu-host", UserID: "owner"},
	}
	if err := s.UpsertRelationships(context.Background(), edges); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
}

func TestTraverseFrom_Depth1(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	seedEdges(t, s)

	got, err := s.TraverseFrom(context.Background(), "operator", "owner", 1, 50)
	if err != nil {
		t.Fatalf("TraverseFrom: %v", err)
	}
	// Depth 1 builds the frontier {operator, homelab, sedan} and then
	// returns EVERY edge touching a frontier entity — that's the
	// "one-hop neighbours of a mentioned entity" augmentation the
	// pipeline relies on, so homelab's outgoing edges ride along with
	// operator's own. What must NOT appear is anything anchored two
	// hops out (gpu-host's and host-a's edges).
	want := map[string]bool{
		"operator|owns|homelab":     true,
		"operator|drives|sedan":     true,
		"homelab|contains|gpu-host": true,
		"homelab|hosts|host-a":      true,
	}
	gotKeys := map[string]bool{}
	for _, r := range got {
		gotKeys[r.Subject+"|"+r.Predicate+"|"+r.Object] = true
	}
	for k := range want {
		if !gotKeys[k] {
			t.Errorf("depth 1 missing edge %s: %+v", k, got)
		}
	}
	for _, far := range []string{"gpu-host|has_gpu|gpu-x", "host-a|has_ip|10.0.0.40", "host-a|connects_to|gpu-host"} {
		if gotKeys[far] {
			t.Errorf("depth 1 leaked far edge %s: %+v", far, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("depth 1 = %d rows, want %d: %+v", len(got), len(want), got)
	}
}

func TestTraverseFrom_Depth2ReachesGPU(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	seedEdges(t, s)

	got, err := s.TraverseFrom(context.Background(), "operator", "owner", 2, 50)
	if err != nil {
		t.Fatalf("TraverseFrom: %v", err)
	}
	// Depth 2 puts gpu-host and host-a on the frontier, and every edge
	// touching a frontier entity comes back — including
	// gpu-host→has_gpu→gpu-x. This is the spec's key property: "what
	// machines does Operator own that have GPUs" is answerable at
	// depth 2 because the has_gpu edge surfaces as soon as gpu-host
	// (two hops from operator) enters the frontier.
	foundContainsGpuHost := false
	foundHasGpu := false
	for _, r := range got {
		if r.Subject == "homelab" && r.Predicate == "contains" && r.Object == "gpu-host" {
			foundContainsGpuHost = true
		}
		if r.Subject == "gpu-host" && r.Predicate == "has_gpu" && r.Object == "gpu-x" {
			foundHasGpu = true
		}
	}
	if !foundContainsGpuHost {
		t.Errorf("depth 2 should include homelab→contains→gpu-host: %+v", got)
	}
	if !foundHasGpu {
		t.Errorf("depth 2 should surface gpu-host→has_gpu→gpu-x via the frontier: %+v", got)
	}
}

func TestTraverseFrom_Depth3ReachesGPU(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	seedEdges(t, s)

	got, err := s.TraverseFrom(context.Background(), "operator", "owner", 3, 50)
	if err != nil {
		t.Fatalf("TraverseFrom: %v", err)
	}
	foundHasGpu := false
	for _, r := range got {
		if r.Subject == "gpu-host" && r.Predicate == "has_gpu" && r.Object == "gpu-x" {
			foundHasGpu = true
		}
	}
	if !foundHasGpu {
		t.Errorf("depth 3 should reach gpu-host→has_gpu→gpu-x: %+v", got)
	}
}

func TestTraverseFrom_CycleTerminates(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	seedEdges(t, s)

	// The seeded cycle is host-a → gpu-host → (implicit back via homelab
	// → contains → host-a). A depth-3 traversal from host-a must not
	// spin — the UNION in the CTE deduplicates the frontier.
	got, err := s.TraverseFrom(context.Background(), "host-a", "owner", 3, 50)
	if err != nil {
		t.Fatalf("TraverseFrom: %v", err)
	}
	if len(got) == 0 {
		t.Error("expected at least one row starting from host-a")
	}
}

func TestTraverseFrom_DepthClamp(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	seedEdges(t, s)

	// depth 99 should be clamped to 3 and therefore return the same
	// set as TestTraverseFrom_Depth3ReachesGPU.
	clamped, err := s.TraverseFrom(context.Background(), "operator", "owner", 99, 50)
	if err != nil {
		t.Fatalf("clamped TraverseFrom: %v", err)
	}
	explicit, err := s.TraverseFrom(context.Background(), "operator", "owner", 3, 50)
	if err != nil {
		t.Fatalf("explicit TraverseFrom: %v", err)
	}
	if len(clamped) != len(explicit) {
		t.Errorf("clamp mismatch: clamped=%d explicit=%d", len(clamped), len(explicit))
	}
}

func TestEntityVocab_FindIn(t *testing.T) {
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "relationships")
	s, err := NewPgRelationshipStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	seedEdges(t, s)

	vocab := NewEntityVocab(s, "owner", 0)
	if err := vocab.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if vocab.Size() == 0 {
		t.Fatal("vocab empty after refresh")
	}

	haystack := "Earlier the operator mentioned that gpu-host runs gpu-x accelerators under Ubuntu."
	found := vocab.FindIn(haystack)
	seen := map[string]bool{}
	for _, n := range found {
		seen[n] = true
	}
	if !seen["operator"] {
		t.Error("expected to find operator in haystack")
	}
	if !seen["gpu-host"] {
		t.Error("expected to find gpu-host in haystack")
	}
	if !seen["gpu-x"] {
		t.Error("expected to find gpu-x in haystack")
	}
}
