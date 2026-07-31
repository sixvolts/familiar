package maintenance

import "testing"

// fakeRegistry backs the controller's injected resolvers.
type fakeRegistry struct {
	status  map[string]string
	labels  map[string]string
	primary string
}

func (f *fakeRegistry) statusOf(id string) string { return f.status[id] }
func (f *fakeRegistry) labelOf(id string) string  { return f.labels[id] }
func (f *fakeRegistry) primaryFn() string         { return f.primary }

func newTestController() (*Controller, *fakeRegistry) {
	reg := &fakeRegistry{
		status: map[string]string{
			"gpu-host/qwen": "online",
			"sidecar/gemma": "online",
		},
		labels: map[string]string{
			"gpu-host/qwen": "Heavy (Qwen)",
			"sidecar/gemma": "Gemma 4 26B",
		},
		primary: "gpu-host/qwen",
	}
	return New(reg.statusOf, reg.labelOf, reg.primaryFn), reg
}

func TestInactiveWithoutSelection(t *testing.T) {
	c, _ := newTestController()
	if st := c.State(); st.Active {
		t.Fatalf("expected inactive with no fallback selected, got %+v", st)
	}
	// Even enabling with no model is a no-op (SetState forces off).
	c.SetState(true, "")
	if active, _ := c.Active(); active {
		t.Fatal("enabling with no model must not activate")
	}
}

func TestManualActivation(t *testing.T) {
	c, reg := newTestController()
	c.SetState(true, "sidecar/gemma")

	active, id := c.Active()
	if !active || id != "sidecar/gemma" {
		t.Fatalf("manual: want active sidecar/gemma, got active=%v id=%q", active, id)
	}
	st := c.State()
	if st.Reason != "manual" {
		t.Fatalf("want reason=manual, got %q", st.Reason)
	}
	if st.Model != "Gemma 4 26B" {
		t.Fatalf("want fallback label, got %q", st.Model)
	}
	if st.Message != "Maintenance mode — using Gemma 4 26B" {
		t.Fatalf("unexpected message: %q", st.Message)
	}
	// Manual mode is independent of primary health.
	reg.status["gpu-host/qwen"] = "offline"
	if st := c.State(); st.Reason != "manual" {
		t.Fatalf("manual should win over auto, got reason %q", st.Reason)
	}
}

func TestAutoActivationOnPrimaryOffline(t *testing.T) {
	c, reg := newTestController()
	// A fallback is selected but the toggle is OFF.
	c.SetState(false, "sidecar/gemma")
	if active, _ := c.Active(); active {
		t.Fatal("should be inactive while primary is online and toggle off")
	}

	// Primary goes offline → auto-activate.
	reg.status["gpu-host/qwen"] = "offline"
	active, id := c.Active()
	if !active || id != "sidecar/gemma" {
		t.Fatalf("auto: want active sidecar/gemma, got active=%v id=%q", active, id)
	}
	if st := c.State(); st.Reason != "auto" || !st.PrimaryOffline {
		t.Fatalf("want reason=auto + PrimaryOffline, got %+v", st)
	}

	// Primary recovers → auto-clears.
	reg.status["gpu-host/qwen"] = "online"
	if active, _ := c.Active(); active {
		t.Fatal("auto should clear when primary recovers")
	}
}

// With a role chain wired, a healthy configured BACKUP must serve
// before the admin-selected maintenance model. Maintenance is the last
// tier — auto must not hijack the turn just because tier 0 is down.
func TestAutoDefersToHealthyChainBackup(t *testing.T) {
	c, reg := newTestController()
	reg.status["gpu-host/qwen-30b"] = "online"
	reg.labels["gpu-host/qwen-30b"] = "Backup (Qwen 30B)"
	// The chat chain has failed over to the backup (tier 1).
	c.WithServing(func() (string, int) { return "gpu-host/qwen-30b", 1 })
	c.SetState(false, "sidecar/gemma")

	reg.status["gpu-host/qwen"] = "offline" // primary down
	if active, _ := c.Active(); active {
		t.Fatal("a healthy configured backup must serve before maintenance auto-engages")
	}
	st := c.State()
	if !st.FailoverActive || st.ServingTier != 1 || st.ServingModel != "Backup (Qwen 30B)" {
		t.Fatalf("expected failover state naming the backup, got %+v", st)
	}
	if st.Message != "Primary model unavailable — using Backup (Qwen 30B)" {
		t.Fatalf("unexpected failover message: %q", st.Message)
	}
}

// When the whole chain is exhausted (the serving model is offline too),
// maintenance auto-engages as the final tier.
func TestAutoEngagesWhenChainExhausted(t *testing.T) {
	c, reg := newTestController()
	reg.status["gpu-host/qwen-30b"] = "offline"
	c.WithServing(func() (string, int) { return "gpu-host/qwen-30b", 1 })
	c.SetState(false, "sidecar/gemma")

	reg.status["gpu-host/qwen"] = "offline"
	active, id := c.Active()
	if !active || id != "sidecar/gemma" {
		t.Fatalf("exhausted chain should auto-engage maintenance, got active=%v id=%q", active, id)
	}
	if st := c.State(); st.Reason != "auto" {
		t.Fatalf("want reason=auto, got %q", st.Reason)
	}
}

// A chat role that resolves to NOTHING is "no chat model configured",
// not "every tier is down". Treating absence as exhaustion latched
// maintenance on permanently: State() reported Active=auto forever, so
// the admin console's banner never cleared after switching it off. The
// E2E suite caught this; these unit tests had blessed the wrong contract.
func TestAutoDoesNotEngageWhenChatRoleResolvesToNothing(t *testing.T) {
	c, reg := newTestController()
	c.WithServing(func() (string, int) { return "", 0 })
	c.SetState(false, "sidecar/gemma")

	// Primary healthy (and in the e2e shape, not even registered) — there
	// is no positive evidence anything is down.
	if active, _ := c.Active(); active {
		t.Fatal("an unresolvable chat role must not auto-engage maintenance — absence is not exhaustion")
	}

	// Toggling on and back off must leave it off, which is the exact
	// sequence the banner test walks.
	c.SetState(true, "sidecar/gemma")
	if active, _ := c.Active(); !active {
		t.Fatal("manual enable should still work")
	}
	c.SetState(false, "sidecar/gemma")
	if active, _ := c.Active(); active {
		t.Fatal("maintenance must clear when disabled — this is the banner that never went away")
	}

	// But a primary that IS positively offline still engages, via the
	// pre-roles signal.
	reg.status["gpu-host/qwen"] = "offline"
	if active, _ := c.Active(); !active {
		t.Fatal("a positively-offline primary should still auto-engage")
	}
}

// Serving the primary (tier 0) is the quiet path: no banner, no
// maintenance, nothing to report.
func TestNoBannerWhenServingPrimary(t *testing.T) {
	c, _ := newTestController()
	c.WithServing(func() (string, int) { return "gpu-host/qwen", 0 })
	c.SetState(false, "sidecar/gemma")
	st := c.State()
	if st.Active || st.FailoverActive || st.Message != "" {
		t.Fatalf("serving the primary should be silent, got %+v", st)
	}
}

// Manual drain still wins over an otherwise-healthy chain.
func TestManualWinsOverHealthyChain(t *testing.T) {
	c, _ := newTestController()
	c.WithServing(func() (string, int) { return "gpu-host/qwen", 0 })
	c.SetState(true, "sidecar/gemma")
	active, id := c.Active()
	if !active || id != "sidecar/gemma" {
		t.Fatalf("manual drain must win even with a healthy primary, got active=%v id=%q", active, id)
	}
}

func TestUnknownStatusIsNotOffline(t *testing.T) {
	c, reg := newTestController()
	c.SetState(false, "sidecar/gemma")
	reg.status["gpu-host/qwen"] = "unknown" // boot, pre-first-healthcheck
	if active, _ := c.Active(); active {
		t.Fatal("unknown primary health must not auto-activate")
	}
}

func TestKnown(t *testing.T) {
	c, _ := newTestController()
	if !c.Known("sidecar/gemma") {
		t.Fatal("registered model should be Known")
	}
	if c.Known("ghost/model") {
		t.Fatal("unregistered model must not be Known")
	}
	if c.Known("") {
		t.Fatal("empty id must not be Known")
	}
}

func TestLabelFallsBackToID(t *testing.T) {
	// A selected model whose label lookup returns "" still reports
	// its id (defensive — shouldn't happen once Known-gated).
	reg := &fakeRegistry{
		status:  map[string]string{"x/y": "online"},
		labels:  map[string]string{}, // no label
		primary: "x/y",
	}
	c := New(reg.statusOf, reg.labelOf, reg.primaryFn)
	c.SetState(true, "x/y")
	if st := c.State(); st.Model != "x/y" {
		t.Fatalf("want id fallback, got %q", st.Model)
	}
}
