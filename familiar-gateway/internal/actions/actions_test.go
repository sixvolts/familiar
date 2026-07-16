package actions

// Store + runner tests (FAMILIAR_TEST_DSN-gated, like the other
// DB-backed suites). The runner is tested at its seams: a fake
// Invoke stands in for the pipeline and fake deliverers record what
// reached them, while the store + ledger run against real Postgres —
// the semantics worth pinning (overlap skip, breaker trip, owner
// gate, delivery fan-out) all live in the store/runner contract,
// not in robfig/cron's clock.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/pageevents"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/shards"
)

// storeForTest migrates into a dedicated `actions_test` schema (the
// memory package's pattern): the shards suite TRUNCATEs
// scheduled_actions in public as part of ITS isolation, and `go test
// ./...` runs packages in parallel against one database — sharing
// public would let that truncate eat this package's rows mid-test.
func storeForTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("FAMILIAR_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping: FAMILIAR_TEST_DSN not set")
	}
	ctx := context.Background()

	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open (admin): %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS actions_test`); err != nil {
		t.Fatalf("create actions_test schema: %v", err)
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := db.Open(dsn + sep + "options=" + url.QueryEscape("-csearch_path=actions_test,public"))
	if err != nil {
		t.Fatalf("db.Open (actions_test schema): %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	s, err := NewStore(pool)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func seedOwner(t *testing.T, s *Store, id string) string {
	t.Helper()
	_, err := s.pool.ExecContext(context.Background(), `
		INSERT INTO users (id, display_name, status, role)
		VALUES ($1, $1, 'approved', 'user')
		ON CONFLICT (id) DO UPDATE SET status = 'approved'`, id)
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	return id
}

func validAction(owner string) *Action {
	return &Action{
		OwnerID: owner,
		Name:    "test action",
		Prompt:  "say something",
		Cron:    "0 7 * * *",
		ReportTargets: []Target{
			{Kind: "log"},
		},
	}
}

// ── Validation ────────────────────────────────────────────────────

func TestValidateRejectsBadShapes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Action)
	}{
		{"no name", func(a *Action) { a.Name = " " }},
		{"no prompt", func(a *Action) { a.Prompt = "" }},
		{"no schedule", func(a *Action) { a.Cron = "" }},
		{"both schedules", func(a *Action) { now := time.Now(); a.RunAt = &now }},
		{"bad cron", func(a *Action) { a.Cron = "every tuesday" }},
		{"six fields", func(a *Action) { a.Cron = "0 0 7 * * *" }},
		{"bad tz", func(a *Action) { a.Timezone = "Mars/Olympus" }},
		{"no targets", func(a *Action) { a.ReportTargets = nil }},
		{"bad target kind", func(a *Action) { a.ReportTargets = []Target{{Kind: "carrier_pigeon"}} }},
		{"slack without channel", func(a *Action) { a.ReportTargets = []Target{{Kind: "slack"}} }},
		{"page without ids", func(a *Action) { a.ReportTargets = []Target{{Kind: "page"}} }},
		{"conversation without id", func(a *Action) { a.ReportTargets = []Target{{Kind: "conversation"}} }},
		{"push without id", func(a *Action) { a.ReportTargets = []Target{{Kind: "push"}} }},
		{"timeout too small", func(a *Action) { a.TimeoutSeconds = 5 }},
		{"timeout too large", func(a *Action) { a.TimeoutSeconds = 7200 }},
		{"bad policy", func(a *Action) { a.DeliveryPolicy = "sometimes" }},
		{"negative budget", func(a *Action) { a.MaxRunsPerDay = -1 }},
		{"budget too large", func(a *Action) { a.MaxRunsPerDay = 5000 }},
		{"unknown trigger kind", func(a *Action) { a.TriggerKind = "full_moon" }},
		{"page_saved with cron", func(a *Action) { a.TriggerKind = TriggerPageSaved; a.WatchBookID = "x" }},
		{"page_saved without book", func(a *Action) {
			a.TriggerKind = TriggerPageSaved
			a.Cron = ""
		}},
		{"webhook without token", func(a *Action) {
			a.TriggerKind = TriggerWebhook
			a.Cron = ""
		}},
		{"bad min interval", func(a *Action) { a.MinIntervalSeconds = -5 }},
	}
	for _, c := range cases {
		a := validAction("v-owner")
		c.mutate(a)
		if err := Validate(a); err == nil {
			t.Errorf("%s: Validate accepted a bad action", c.name)
		}
	}
}

func TestValidateAppliesDefaults(t *testing.T) {
	a := validAction("v-owner")
	if err := Validate(a); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if a.TimeoutSeconds != 600 || a.MaxConsecutiveFailures != 5 ||
		a.Timezone != "UTC" || a.DeliveryPolicy != "always" {
		t.Errorf("defaults not applied: %+v", a)
	}
}

// The timezones the UI offers must all be loadable in the binary we
// ship. This fails without the embedded tzdata (internal/actions imports
// time/tzdata) on hosts that lack a system zoneinfo database — the exact
// regression that made non-UTC actions unusable. Also confirms Validate
// accepts them.
func TestValidate_SupportedTimezonesLoad(t *testing.T) {
	zones := []string{
		"UTC",
		"America/New_York",    // Eastern
		"America/Chicago",     // Central
		"America/Denver",      // Mountain
		"America/Los_Angeles", // Pacific
		"Europe/Paris",        // CET
	}
	for _, z := range zones {
		if _, err := time.LoadLocation(z); err != nil {
			t.Errorf("LoadLocation(%q) failed — is time/tzdata embedded? %v", z, err)
		}
		a := validAction("tz-owner")
		a.Timezone = z
		if err := Validate(a); err != nil {
			t.Errorf("Validate rejected supported timezone %q: %v", z, err)
		}
	}
}

// The "none" target validates with no fields — the run is recorded in
// the ledger but nothing is delivered.
func TestValidate_NoneTargetIsValid(t *testing.T) {
	a := validAction("none-owner")
	a.ReportTargets = []Target{{Kind: "none"}}
	if err := Validate(a); err != nil {
		t.Errorf("none target should be valid: %v", err)
	}
}

// A push target with a conversation_id validates (handlers auto-create
// the id when omitted, same as a conversation target).
func TestValidate_PushTargetWithConversationID(t *testing.T) {
	a := validAction("push-owner")
	a.ReportTargets = []Target{{Kind: "push", ConversationID: "11111111-1111-1111-1111-111111111111"}}
	if err := Validate(a); err != nil {
		t.Errorf("push target with conversation_id should be valid: %v", err)
	}
}

// ── Store CRUD + scoping ──────────────────────────────────────────

func TestStore_CRUDAndOwnerScoping(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	owner := seedOwner(t, s, "act-owner")
	intruder := seedOwner(t, s, "act-intruder")

	created, err := s.Create(ctx, validAction(owner))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || !created.Enabled {
		t.Fatalf("created = %+v", created)
	}

	// Owner reads it; the intruder's read is indistinguishable from
	// nonexistence; admin reads anything.
	if _, err := s.Get(ctx, created.ID, owner, false); err != nil {
		t.Errorf("owner Get: %v", err)
	}
	if _, err := s.Get(ctx, created.ID, intruder, false); !errors.Is(err, ErrActionNotFound) {
		t.Errorf("intruder Get err = %v, want ErrActionNotFound", err)
	}
	if _, err := s.Get(ctx, created.ID, "", true); err != nil {
		t.Errorf("admin Get: %v", err)
	}

	// Lists are scoped the same way.
	ownerList, _ := s.List(ctx, owner, false)
	if len(ownerList) == 0 {
		t.Error("owner list empty")
	}
	intruderList, _ := s.List(ctx, intruder, false)
	for _, a := range intruderList {
		if a.ID == created.ID {
			t.Error("intruder list leaked the action")
		}
	}

	// Update round-trips and refuses the intruder.
	created.Name = "renamed"
	created.OwnerID = owner
	if _, err := s.Update(ctx, created, false); err != nil {
		t.Fatalf("owner Update: %v", err)
	}
	hijack := *created
	hijack.OwnerID = intruder
	if _, err := s.Update(ctx, &hijack, false); !errors.Is(err, ErrActionNotFound) {
		t.Errorf("intruder Update err = %v, want ErrActionNotFound", err)
	}

	// Toggle + delete.
	if err := s.SetEnabled(ctx, created.ID, owner, false, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, _ := s.Get(ctx, created.ID, owner, false)
	if got.Enabled {
		t.Error("disable didn't stick")
	}
	if err := s.Delete(ctx, created.ID, intruder, false); !errors.Is(err, ErrActionNotFound) {
		t.Errorf("intruder Delete err = %v", err)
	}
	if err := s.Delete(ctx, created.ID, owner, false); err != nil {
		t.Fatalf("owner Delete: %v", err)
	}
}

func TestStore_ReenableResetsFailureCounter(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	owner := seedOwner(t, s, "act-reenable")
	a, err := s.Create(ctx, validAction(owner))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	runID, _ := s.StartRun(ctx, a.ID, "manual")
	if _, err := s.FinishRun(ctx, runID, a.ID, RunResult{Status: RunStatusError, Error: "boom"}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if err := s.SetEnabled(ctx, a.ID, owner, false, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := s.SetEnabled(ctx, a.ID, owner, false, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _ := s.Get(ctx, a.ID, owner, false)
	if got.ConsecutiveFailures != 0 {
		t.Errorf("re-enable left consecutive_failures = %d", got.ConsecutiveFailures)
	}
}

// ── Runner ────────────────────────────────────────────────────────

type runnerHarness struct {
	store     *Store
	runner    *Runner
	mu        sync.Mutex
	delivered []string // texts that reached the fake deliverer
	invoked   int
	overrides []*pipeline.ShardOverrides
}

func newHarness(t *testing.T, invoke InvokeFunc, opts ...func(*Deps)) *runnerHarness {
	t.Helper()
	h := &runnerHarness{store: storeForTest(t)}
	if invoke == nil {
		invoke = func(ctx context.Context, sess *session.Session, prompt string, ov *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
			h.mu.Lock()
			h.invoked++
			h.overrides = append(h.overrides, ov)
			h.mu.Unlock()
			return "report text", &pipeline.RouteInfo{ModelID: "test/fake"}, nil
		}
	}
	deps := Deps{
		Store:    h.store,
		Sessions: session.NewManager(),
		Invoke:   invoke,
		UserStatus: func(ctx context.Context, userID string) (string, error) {
			return "approved", nil
		},
		Deliverers: map[string]DeliverFunc{
			"log": func(ctx context.Context, ownerID, actionID string, tg Target, name, text string) error {
				h.mu.Lock()
				h.delivered = append(h.delivered, text)
				h.mu.Unlock()
				return nil
			},
		},
	}
	for _, o := range opts {
		o(&deps)
	}
	r, err := NewRunner(deps)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	h.runner = r
	return h
}

// waitRun polls the ledger until the run leaves "running".
func waitRun(t *testing.T, s *Store, actionID, runID string) *Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := s.ListRuns(context.Background(), actionID, 50)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, r := range runs {
			if r.ID == runID && r.Status != RunStatusRunning {
				return r
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("run never finished")
	return nil
}

func TestRunner_RunNowHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	owner := seedOwner(t, h.store, "run-owner")
	a, _ := h.store.Create(ctx, validAction(owner))

	runID, err := h.runner.RunNow(ctx, a.ID, owner, false)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	run := waitRun(t, h.store, a.ID, runID)
	if run.Status != RunStatusOK {
		t.Fatalf("run status = %s (%s)", run.Status, run.Error)
	}
	if run.Trigger != "manual" || run.ModelID != "test/fake" || run.Output != "report text" {
		t.Errorf("run = %+v", run)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.delivered) != 1 || h.delivered[0] != "report text" {
		t.Errorf("delivered = %v", h.delivered)
	}
	got, _ := h.store.Get(ctx, a.ID, owner, false)
	if got.LastStatus != RunStatusOK || got.ConsecutiveFailures != 0 {
		t.Errorf("action after run = %+v", got)
	}
}

func TestRunner_OwnerGateSkips(t *testing.T) {
	h := newHarness(t, nil, func(d *Deps) {
		d.UserStatus = func(ctx context.Context, userID string) (string, error) {
			return "disabled", nil
		}
	})
	ctx := context.Background()
	owner := seedOwner(t, h.store, "gate-owner")
	a, _ := h.store.Create(ctx, validAction(owner))

	runID, err := h.runner.RunNow(ctx, a.ID, owner, false)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	run := waitRun(t, h.store, a.ID, runID)
	if run.Status != RunStatusSkippedOwner {
		t.Fatalf("status = %s, want skipped_owner", run.Status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.invoked != 0 {
		t.Error("pipeline invoked for a disabled owner")
	}
	// Skips never feed the breaker.
	got, _ := h.store.Get(ctx, a.ID, owner, false)
	if got.ConsecutiveFailures != 0 {
		t.Errorf("skip incremented failures: %d", got.ConsecutiveFailures)
	}
}

func TestRunner_BreakerTripsAndDisables(t *testing.T) {
	failing := func(ctx context.Context, sess *session.Session, prompt string, ov *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
		return "", nil, fmt.Errorf("model exploded")
	}
	h := newHarness(t, failing)
	ctx := context.Background()
	owner := seedOwner(t, h.store, "brk-owner")
	act := validAction(owner)
	act.MaxConsecutiveFailures = 2
	a, _ := h.store.Create(ctx, act)

	for i := 0; i < 2; i++ {
		runID, err := h.runner.RunNow(ctx, a.ID, owner, false)
		if err != nil {
			t.Fatalf("RunNow %d: %v", i, err)
		}
		run := waitRun(t, h.store, a.ID, runID)
		if run.Status != RunStatusError {
			t.Fatalf("run %d status = %s", i, run.Status)
		}
	}

	got, _ := h.store.Get(ctx, a.ID, owner, false)
	if got.Enabled {
		t.Fatal("breaker did not disable the action")
	}
	if got.ConsecutiveFailures < 2 {
		t.Errorf("failures = %d", got.ConsecutiveFailures)
	}
	// The trip notice went out through the deliverer.
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.delivered) == 0 {
		t.Error("breaker trip was silent — expected a notice delivery")
	}
}

func TestRunner_OverlapSkips(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	slow := func(ctx context.Context, sess *session.Session, prompt string, ov *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
		once.Do(func() { close(started) })
		<-release
		return "slow done", nil, nil
	}
	h := newHarness(t, slow)
	ctx := context.Background()
	owner := seedOwner(t, h.store, "ovl-owner")
	a, _ := h.store.Create(ctx, validAction(owner))

	first, err := h.runner.RunNow(ctx, a.ID, owner, false)
	if err != nil {
		t.Fatalf("first RunNow: %v", err)
	}
	// Wait until the first run is INSIDE the pipeline (and therefore
	// holds the in-flight slot) — a sleep races under load.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("first run never reached the pipeline")
	}
	second, err := h.runner.RunNow(ctx, a.ID, owner, false)
	if err != nil {
		t.Fatalf("second RunNow: %v", err)
	}
	skipped := waitRun(t, h.store, a.ID, second)
	if skipped.Status != RunStatusSkippedOverlap {
		t.Fatalf("second run status = %s, want skipped_overlap", skipped.Status)
	}
	close(release)
	done := waitRun(t, h.store, a.ID, first)
	if done.Status != RunStatusOK {
		t.Fatalf("first run status = %s", done.Status)
	}
}

func TestRunner_ShardEnvelopeReachesInvoke(t *testing.T) {
	sh := &shards.Shard{
		ID: "env-shard", OwnerID: "env-owner", Name: "Envelope",
		Persistence: shards.PersistencePersistent, Visibility: shards.VisibilityIsolated,
		ScopeTag: "shard:env-shard", SystemPrompt: "you are bounded",
		ToolAllowlist: []string{"read_page"}, MaxTokens: 512,
	}
	h := newHarness(t, nil, func(d *Deps) {
		d.GetShard = func(ctx context.Context, id string) (*shards.Shard, error) {
			if id != sh.ID {
				return nil, fmt.Errorf("unknown shard")
			}
			return sh, nil
		}
	})
	ctx := context.Background()
	owner := seedOwner(t, h.store, "env-owner")
	act := validAction(owner)
	act.ShardID = sh.ID
	// FK: the shard row must exist for the action insert.
	if _, err := h.store.pool.ExecContext(ctx, `
		INSERT INTO shards (id, owner_id, name, persistence, visibility, scope_tag, system_prompt)
		VALUES ($1, $2, 'Envelope', 'persistent', 'isolated', 'shard:env-shard', 'x')
		ON CONFLICT (id) DO NOTHING`, sh.ID, owner); err != nil {
		t.Fatalf("seed shard row: %v", err)
	}
	a, err := h.store.Create(ctx, act)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	runID, err := h.runner.RunNow(ctx, a.ID, owner, false)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	run := waitRun(t, h.store, a.ID, runID)
	if run.Status != RunStatusOK {
		t.Fatalf("status = %s (%s)", run.Status, run.Error)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.overrides) != 1 || h.overrides[0] == nil {
		t.Fatalf("invoke did not receive overrides: %v", h.overrides)
	}
	ov := h.overrides[0]
	if ov.ShardID != sh.ID || ov.ScopeTag != "shard:env-shard" ||
		len(ov.ToolAllowlist) != 1 || ov.ToolAllowlist[0] != "read_page" ||
		!ov.ExcludeFromHot {
		t.Errorf("overrides = %+v", ov)
	}
}

// ── Phase 2: quiet sentinel + budget ─────────────────────────────

func TestIsQuietReply(t *testing.T) {
	quiet := []string{
		"NOTHING_TO_REPORT",
		"nothing_to_report",
		"  NOTHING_TO_REPORT.\n",
		"`NOTHING_TO_REPORT`",
		"**NOTHING_TO_REPORT**",
		"\"NOTHING_TO_REPORT!\"",
	}
	for _, s := range quiet {
		if !isQuietReply(s) {
			t.Errorf("isQuietReply(%q) = false, want true", s)
		}
	}
	loud := []string{
		"",
		"NOTHING_TO_REPORT but actually...",
		"There is NOTHING_TO_REPORT today.",
		"All systems nominal.",
	}
	for _, s := range loud {
		if isQuietReply(s) {
			t.Errorf("isQuietReply(%q) = true, want false", s)
		}
	}
}

func TestRunner_OnContentSuppressesDelivery(t *testing.T) {
	var sawPrompt string
	quiet := func(ctx context.Context, sess *session.Session, prompt string, ov *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
		sawPrompt = prompt
		return "NOTHING_TO_REPORT.", &pipeline.RouteInfo{ModelID: "test/fake"}, nil
	}
	h := newHarness(t, quiet)
	ctx := context.Background()
	owner := seedOwner(t, h.store, "quiet-owner")
	act := validAction(owner)
	act.DeliveryPolicy = "on_content"
	// Pre-load a failure so the quiet run's counter-reset is visible.
	a, _ := h.store.Create(ctx, act)
	preRun, _ := h.store.StartRun(ctx, a.ID, "manual")
	if _, err := h.store.FinishRun(ctx, preRun, a.ID, RunResult{Status: RunStatusError, Error: "warmup"}); err != nil {
		t.Fatalf("seed failure: %v", err)
	}

	runID, err := h.runner.RunNow(ctx, a.ID, owner, false)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	run := waitRun(t, h.store, a.ID, runID)
	if run.Status != RunStatusSkippedQuiet {
		t.Fatalf("status = %s, want skipped_quiet (%s)", run.Status, run.Error)
	}
	// The sentinel contract was appended to the prompt; the reply is
	// ledgered; NOTHING was delivered; failures reset.
	if !strings.Contains(sawPrompt, QuietSentinel) {
		t.Error("on_content did not append the sentinel contract to the prompt")
	}
	if run.Output == "" {
		t.Error("quiet run should still ledger the reply")
	}
	h.mu.Lock()
	delivered := len(h.delivered)
	h.mu.Unlock()
	if delivered != 0 {
		t.Errorf("quiet run delivered %d message(s)", delivered)
	}
	got, _ := h.store.Get(ctx, a.ID, owner, false)
	if got.ConsecutiveFailures != 0 {
		t.Errorf("quiet run left failures = %d, want reset", got.ConsecutiveFailures)
	}
}

func TestRunner_OnContentDeliversRealContent(t *testing.T) {
	h := newHarness(t, nil) // default invoke returns "report text"
	ctx := context.Background()
	owner := seedOwner(t, h.store, "loud-owner")
	act := validAction(owner)
	act.DeliveryPolicy = "on_content"
	a, _ := h.store.Create(ctx, act)

	runID, _ := h.runner.RunNow(ctx, a.ID, owner, false)
	run := waitRun(t, h.store, a.ID, runID)
	if run.Status != RunStatusOK {
		t.Fatalf("status = %s", run.Status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.delivered) != 1 {
		t.Errorf("real content should deliver, got %d", len(h.delivered))
	}
}

func TestRunner_DailyBudgetGatesScheduledNotManual(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	owner := seedOwner(t, h.store, "budget-owner")
	act := validAction(owner)
	act.MaxRunsPerDay = 1
	a, _ := h.store.Create(ctx, act)

	// First scheduled fire executes; second hits the budget.
	h.runner.fire(a.ID, "cron")
	h.runner.fire(a.ID, "cron")
	runs, err := h.store.ListRuns(ctx, a.ID, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	// Newest first: the second fire was budget-skipped.
	if runs[0].Status != RunStatusSkippedBudget || runs[1].Status != RunStatusOK {
		t.Fatalf("statuses = %s, %s — want skipped_budget, ok", runs[0].Status, runs[1].Status)
	}

	// Manual run-now bypasses the budget entirely.
	manualID, err := h.runner.RunNow(ctx, a.ID, owner, false)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	manual := waitRun(t, h.store, a.ID, manualID)
	if manual.Status != RunStatusOK {
		t.Fatalf("manual status = %s, want ok despite budget", manual.Status)
	}
}

func TestStore_DisableByShard(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	owner := seedOwner(t, s, "dbs-owner")
	shardID := fmt.Sprintf("dbs-shard-%d", time.Now().UnixNano())
	if _, err := s.pool.ExecContext(ctx, `
		INSERT INTO shards (id, owner_id, name, persistence, visibility, scope_tag, system_prompt)
		VALUES ($1, $2, 'DBS', 'persistent', 'isolated', $1, 'x')`, shardID, owner); err != nil {
		t.Fatalf("seed shard: %v", err)
	}

	bound := validAction(owner)
	bound.ShardID = shardID
	boundA, err := s.Create(ctx, bound)
	if err != nil {
		t.Fatalf("create bound: %v", err)
	}
	trusted, err := s.Create(ctx, validAction(owner))
	if err != nil {
		t.Fatalf("create trusted: %v", err)
	}

	n, err := s.DisableByShard(ctx, shardID)
	if err != nil {
		t.Fatalf("DisableByShard: %v", err)
	}
	if n != 1 {
		t.Fatalf("disabled %d action(s), want 1", n)
	}
	gotBound, _ := s.Get(ctx, boundA.ID, owner, false)
	if gotBound.Enabled || gotBound.LastStatus != "shard_deleted" {
		t.Errorf("bound action after = enabled:%v last_status:%q", gotBound.Enabled, gotBound.LastStatus)
	}
	gotTrusted, _ := s.Get(ctx, trusted.ID, owner, false)
	if !gotTrusted.Enabled {
		t.Error("unrelated trusted action was disabled")
	}

	// Idempotent: nothing left to disable.
	if n, _ := s.DisableByShard(ctx, shardID); n != 0 {
		t.Errorf("second DisableByShard touched %d rows", n)
	}
}

// ── Phase 3: event triggers ───────────────────────────────────────

// pageSavedAction creates a page_saved action watching bookID. The
// watch fields are set directly (the handler normally resolves the
// slug); validation accepts them as-is.
func pageSavedAction(owner, bookID string) *Action {
	a := validAction(owner)
	a.TriggerKind = TriggerPageSaved
	a.Cron = ""
	a.WatchBookID = bookID
	a.WatchBookSlug = "watched-book"
	a.MinIntervalSeconds = 1
	return a
}

// latestRun polls until the action has a finished run and returns it.
func latestRun(t *testing.T, s *Store, actionID string) *Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := s.ListRuns(context.Background(), actionID, 5)
		if len(runs) > 0 && runs[0].Status != RunStatusRunning {
			return runs[0]
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no finished run")
	return nil
}

func TestRunner_PageSavedFiresWithEventContext(t *testing.T) {
	var sawPrompt string
	var mu sync.Mutex
	invoke := func(ctx context.Context, sess *session.Session, prompt string, ov *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
		mu.Lock()
		sawPrompt = prompt
		mu.Unlock()
		return "event report", nil, nil
	}
	bus := pageevents.NewBus()
	h := newHarness(t, invoke, func(d *Deps) { d.PageEvents = bus })
	ctx := context.Background()
	owner := seedOwner(t, h.store, "psv-owner")
	a, err := h.store.Create(ctx, pageSavedAction(owner, "11111111-1111-1111-1111-111111111111"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.runner.Stop()

	bus.Publish(pageevents.KindPageSaved, "11111111-1111-1111-1111-111111111111", "some-page",
		pageevents.PageSavedPayload{BookSlug: "watched-book", PageSlug: "diary", Title: "Diary"})

	run := latestRun(t, h.store, a.ID)
	if run.Status != RunStatusOK || run.Trigger != "page_saved" {
		t.Fatalf("run = %s/%s (%s)", run.Status, run.Trigger, run.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(sawPrompt, "[Trigger event]") || !strings.Contains(sawPrompt, "diary") {
		t.Errorf("event context missing from prompt:\n%s", sawPrompt)
	}
}

func TestRunner_PageSavedIgnoresOwnTargetAndOtherBooks(t *testing.T) {
	bus := pageevents.NewBus()
	h := newHarness(t, nil, func(d *Deps) {
		d.PageEvents = bus
		d.Deliverers["page"] = func(ctx context.Context, ownerID, actionID string, tg Target, name, text string) error {
			return nil
		}
	})
	ctx := context.Background()
	owner := seedOwner(t, h.store, "loop-owner")
	act := pageSavedAction(owner, "22222222-2222-2222-2222-222222222222")
	// The action's own report target lives in the watched book.
	act.ReportTargets = []Target{{Kind: "page", BookSlug: "watched-book", PageID: "target-page-id"}}
	a, _ := h.store.Create(ctx, act)
	if err := h.runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.runner.Stop()

	// Event for the action's OWN target page → must not fire (the
	// self-trigger loop). Event for a different book → not watched.
	bus.Publish(pageevents.KindPageSaved, "22222222-2222-2222-2222-222222222222", "target-page-id", nil)
	bus.Publish(pageevents.KindPageSaved, "33333333-3333-3333-3333-333333333333", "other-page", nil)
	time.Sleep(300 * time.Millisecond)
	runs, _ := h.store.ListRuns(ctx, a.ID, 5)
	if len(runs) != 0 {
		t.Fatalf("expected no runs, got %d (first: %s/%s)", len(runs), runs[0].Status, runs[0].Trigger)
	}

	// A different page in the watched book DOES fire.
	bus.Publish(pageevents.KindPageSaved, "22222222-2222-2222-2222-222222222222", "another-page", nil)
	run := latestRun(t, h.store, a.ID)
	if run.Status != RunStatusOK {
		t.Fatalf("run = %s (%s)", run.Status, run.Error)
	}
}

func TestRunner_PageSavedThrottlesBursts(t *testing.T) {
	bus := pageevents.NewBus()
	h := newHarness(t, nil, func(d *Deps) { d.PageEvents = bus })
	ctx := context.Background()
	owner := seedOwner(t, h.store, "burst-owner")
	act := pageSavedAction(owner, "44444444-4444-4444-4444-444444444444")
	act.MinIntervalSeconds = 3600 // one fire per hour, i.e. once in this test
	a, _ := h.store.Create(ctx, act)
	if err := h.runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.runner.Stop()

	// An autosave burst: five events in quick succession.
	for i := 0; i < 5; i++ {
		bus.Publish(pageevents.KindPageSaved, "44444444-4444-4444-4444-444444444444", fmt.Sprintf("p%d", i), nil)
	}
	run := latestRun(t, h.store, a.ID)
	if run.Status != RunStatusOK {
		t.Fatalf("run = %s", run.Status)
	}
	// Give any stragglers a beat, then assert the burst collapsed to
	// ONE ledger row — throttled fires are dropped silently.
	time.Sleep(300 * time.Millisecond)
	runs, _ := h.store.ListRuns(ctx, a.ID, 10)
	if len(runs) != 1 {
		t.Fatalf("burst produced %d runs, want 1", len(runs))
	}
}

func TestRunner_WebhookFiresAndGuards(t *testing.T) {
	var sawPrompt string
	var mu sync.Mutex
	invoke := func(ctx context.Context, sess *session.Session, prompt string, ov *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
		mu.Lock()
		sawPrompt = prompt
		mu.Unlock()
		return "hook report", nil, nil
	}
	h := newHarness(t, invoke)
	ctx := context.Background()
	owner := seedOwner(t, h.store, "hook-owner")
	act := validAction(owner)
	act.TriggerKind = TriggerWebhook
	act.Cron = ""
	// Unique per run — the actions_test schema persists across `go
	// test` invocations and webhook_token is unique-indexed.
	act.WebhookToken = fmt.Sprintf("whk_test_%d", time.Now().UnixNano())
	act.MinIntervalSeconds = 3600
	a, err := h.store.Create(ctx, act)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Happy path: payload reaches the prompt, ledger says webhook.
	runID, err := h.runner.FireWebhook(ctx, act.WebhookToken, []byte(`{"alert":"disk full"}`))
	if err != nil {
		t.Fatalf("FireWebhook: %v", err)
	}
	run := waitRun(t, h.store, a.ID, runID)
	if run.Status != RunStatusOK || run.Trigger != "webhook" {
		t.Fatalf("run = %s/%s", run.Status, run.Trigger)
	}
	mu.Lock()
	if !strings.Contains(sawPrompt, "disk full") {
		t.Errorf("payload missing from prompt:\n%s", sawPrompt)
	}
	mu.Unlock()

	// Second fire inside the interval → throttled, no ledger row.
	if _, err := h.runner.FireWebhook(ctx, act.WebhookToken, nil); !errors.Is(err, ErrThrottled) {
		t.Fatalf("second fire err = %v, want ErrThrottled", err)
	}

	// Bad token and disabled action read identically as not-found.
	if _, err := h.runner.FireWebhook(ctx, "whk_wrong", nil); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("bad token err = %v, want ErrActionNotFound", err)
	}
	if err := h.store.SetEnabled(ctx, a.ID, owner, false, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := h.runner.FireWebhook(ctx, act.WebhookToken, nil); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("disabled err = %v, want ErrActionNotFound", err)
	}
}

func TestRunner_NoDeliveriesSucceedingIsAnError(t *testing.T) {
	h := newHarness(t, nil, func(d *Deps) {
		d.Deliverers = map[string]DeliverFunc{
			"log": func(ctx context.Context, ownerID, actionID string, tg Target, name, text string) error {
				return fmt.Errorf("target down")
			},
		}
	})
	ctx := context.Background()
	owner := seedOwner(t, h.store, "del-owner")
	a, _ := h.store.Create(ctx, validAction(owner))

	runID, _ := h.runner.RunNow(ctx, a.ID, owner, false)
	run := waitRun(t, h.store, a.ID, runID)
	if run.Status != RunStatusError {
		t.Fatalf("status = %s, want error when every delivery failed", run.Status)
	}
}

// ── Envelopes: user / ephemeral / shard ──────────────────────────

func TestValidateEnvelope(t *testing.T) {
	// Derivation for legacy callers: no envelope + no shard → user;
	// no envelope + shard_id → shard.
	a := validAction("env-v")
	if err := Validate(a); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if a.Envelope != EnvelopeUser {
		t.Errorf("derived envelope = %q, want user", a.Envelope)
	}
	b := validAction("env-v")
	b.ShardID = "some-shard"
	if err := Validate(b); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if b.Envelope != EnvelopeShard {
		t.Errorf("derived envelope = %q, want shard", b.Envelope)
	}

	bad := []struct {
		name   string
		mutate func(*Action)
	}{
		{"user with shard_id", func(a *Action) { a.Envelope = EnvelopeUser; a.ShardID = "s" }},
		{"ephemeral with shard_id", func(a *Action) { a.Envelope = EnvelopeEphemeral; a.ShardID = "s" }},
		{"shard without shard_id", func(a *Action) { a.Envelope = EnvelopeShard }},
		{"unknown envelope", func(a *Action) { a.Envelope = "cosmic" }},
	}
	for _, c := range bad {
		a := validAction("env-v")
		c.mutate(a)
		if err := Validate(a); err == nil {
			t.Errorf("%s: Validate accepted a bad envelope shape", c.name)
		}
	}

	// slack_dm needs no fields — the owner's identity resolves at
	// delivery time.
	dm := validAction("env-v")
	dm.ReportTargets = []Target{{Kind: "slack_dm"}}
	if err := Validate(dm); err != nil {
		t.Errorf("slack_dm target rejected: %v", err)
	}
}

func TestRunner_EphemeralEnvelopeReachesInvoke(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	owner := seedOwner(t, h.store, "eph-owner")
	act := validAction(owner)
	act.Envelope = EnvelopeEphemeral
	a, err := h.store.Create(ctx, act)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Envelope != EnvelopeEphemeral {
		t.Fatalf("persisted envelope = %q", a.Envelope)
	}

	runID, err := h.runner.RunNow(ctx, a.ID, owner, false)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	run := waitRun(t, h.store, a.ID, runID)
	if run.Status != RunStatusOK {
		t.Fatalf("status = %s (%s)", run.Status, run.Error)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.overrides) != 1 || h.overrides[0] == nil {
		t.Fatalf("invoke did not receive overrides: %v", h.overrides)
	}
	ov := h.overrides[0]
	// The prompt-only contract: no identity, no prompt, no tools
	// (empty non-nil allowlist), nothing read or written.
	if ov.ShardID != "" || ov.SystemPrompt != "" || ov.ScopeTag != "" {
		t.Errorf("ephemeral overrides carry scope: %+v", ov)
	}
	if ov.ToolAllowlist == nil || len(ov.ToolAllowlist) != 0 {
		t.Errorf("ephemeral allowlist = %v, want empty non-nil", ov.ToolAllowlist)
	}
	if !ov.SkipMemoryRetrieval || !ov.SkipSessionHydration || !ov.SkipCommit {
		t.Errorf("ephemeral skips not all set: %+v", ov)
	}
}
