package modelrole

import "testing"

// healthMap is a mutable HealthFn backing for tests.
type healthMap map[string]string

func (h healthMap) fn(id string) string { return h[id] }

func chains() map[string][]string {
	return map[string][]string{
		"chat":     {"primary", "backup", "fallback"},
		"classify": {"small"},
	}
}

func TestResolveColdStartPrefersPrimary(t *testing.T) {
	// Everything unknown (never probed) — the primary must win.
	r := New(chains(), healthMap{}.fn)
	id, tier, ok := r.Resolve("chat")
	if !ok || id != "primary" || tier != 0 {
		t.Fatalf("cold start: got (%q, %d, %v), want (primary, 0, true)", id, tier, ok)
	}
}

func TestResolveFailsOverToBackup(t *testing.T) {
	h := healthMap{"primary": StatusOffline, "backup": StatusOnline}
	r := New(chains(), h.fn)
	id, tier, _ := r.Resolve("chat")
	if id != "backup" || tier != 1 {
		t.Fatalf("primary offline: got (%q, %d), want (backup, 1)", id, tier)
	}
}

func TestResolveFallsThroughToFallback(t *testing.T) {
	h := healthMap{"primary": StatusOffline, "backup": StatusOffline, "fallback": StatusOnline}
	r := New(chains(), h.fn)
	id, tier, _ := r.Resolve("chat")
	if id != "fallback" || tier != 2 {
		t.Fatalf("primary+backup offline: got (%q, %d), want (fallback, 2)", id, tier)
	}
}

func TestResolveAllOfflineReturnsPrimary(t *testing.T) {
	h := healthMap{"primary": StatusOffline, "backup": StatusOffline, "fallback": StatusOffline}
	r := New(chains(), h.fn)
	id, tier, ok := r.Resolve("chat")
	if !ok || id != "primary" || tier != 0 {
		t.Fatalf("all offline: got (%q, %d, %v), want best-effort (primary, 0, true)", id, tier, ok)
	}
}

func TestResolveAutoFailback(t *testing.T) {
	// Primary offline → backup; primary recovers → primary again, with
	// no state carried between calls.
	h := healthMap{"primary": StatusOffline, "backup": StatusOnline}
	r := New(chains(), h.fn)
	if id, _, _ := r.Resolve("chat"); id != "backup" {
		t.Fatalf("expected backup while primary offline, got %q", id)
	}
	h["primary"] = StatusOnline
	if id, tier, _ := r.Resolve("chat"); id != "primary" || tier != 0 {
		t.Fatalf("expected auto-failback to primary, got (%q, %d)", id, tier)
	}
}

func TestResolvePriorityOverOnlineBackup(t *testing.T) {
	// A merely-unknown primary is preferred over an online backup:
	// priority order dominates, only "offline" demotes.
	h := healthMap{"primary": StatusUnknown, "backup": StatusOnline}
	r := New(chains(), h.fn)
	if id, _, _ := r.Resolve("chat"); id != "primary" {
		t.Fatalf("unknown primary should still win over online backup, got %q", id)
	}
}

func TestResolveUnconfiguredRole(t *testing.T) {
	r := New(chains(), healthMap{}.fn)
	if _, _, ok := r.Resolve("does-not-exist"); ok {
		t.Fatal("expected ok=false for an unconfigured role")
	}
	if got := r.ResolveID("does-not-exist"); got != "" {
		t.Fatalf("ResolveID for unknown role should be empty, got %q", got)
	}
}

func TestNilHealthTreatsAllUsable(t *testing.T) {
	r := New(chains(), nil)
	if id, _, _ := r.Resolve("chat"); id != "primary" {
		t.Fatalf("nil health: expected primary, got %q", id)
	}
}

func TestSnapshotReportsActiveTier(t *testing.T) {
	h := healthMap{"primary": StatusOffline, "backup": StatusOnline, "fallback": StatusUnknown}
	r := New(chains(), h.fn)
	var chat *RoleStatus
	for i := range r.Snapshot() {
		if s := r.Snapshot()[i]; s.Role == "chat" {
			chat = &s
		}
	}
	if chat == nil {
		t.Fatal("chat role missing from snapshot")
	}
	if chat.ActiveTier != 1 || chat.ActiveID != "backup" {
		t.Fatalf("snapshot active: got tier=%d id=%q, want 1/backup", chat.ActiveTier, chat.ActiveID)
	}
	if len(chat.Candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(chat.Candidates))
	}
	if !chat.Candidates[1].Active {
		t.Fatal("backup candidate should be marked active")
	}
}

func TestSetHealthSwaps(t *testing.T) {
	r := New(chains(), nil)
	h := healthMap{"primary": StatusOffline, "backup": StatusOnline}
	r.SetHealth(h.fn)
	if id, _, _ := r.Resolve("chat"); id != "backup" {
		t.Fatalf("after SetHealth expected backup, got %q", id)
	}
}
