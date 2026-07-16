package actions

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/familiar/gateway/internal/pageevents"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/shards"
)

// ErrThrottled is returned by FireWebhook when the action's
// min_interval window hasn't elapsed — the endpoint maps it to 429.
var ErrThrottled = fmt.Errorf("actions: throttled")

// QuietSentinel is the delivery_policy=on_content contract: when the
// model's whole reply is this token (formatting and trailing
// punctuation tolerated), the run records skipped_quiet and nothing
// is delivered. The runner appends the contract to the prompt itself
// so action authors don't have to remember the magic word.
const QuietSentinel = "NOTHING_TO_REPORT"

// quietInstruction is what on_content appends to the action prompt.
const quietInstruction = "\n\nIf there is nothing new or worth reporting, reply with exactly " +
	QuietSentinel + " and nothing else."

// isQuietReply reports whether a model reply is the sentinel — after
// stripping whitespace, markdown emphasis/code wrappers, and trailing
// punctuation, case-insensitively. Models love decorating one-word
// answers; the contract shouldn't break on "`NOTHING_TO_REPORT`."
func isQuietReply(text string) bool {
	t := strings.TrimSpace(text)
	t = strings.Trim(t, "*_`\"' \t\n")
	t = strings.TrimRight(t, ".!")
	return strings.EqualFold(t, QuietSentinel)
}

// InvokeFunc runs one prompt through the pipeline. overrides nil =
// trusted path (pipeline.Handle); non-nil = shard envelope
// (pipeline.HandleShard). Wired as a closure in main.go and faked in
// tests — the runner never imports the pipeline's internals beyond
// the two types in the signature.
type InvokeFunc func(ctx context.Context, sess *session.Session, prompt string, overrides *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error)

// DeliverFunc sends a finished run's text to one target. Keyed by
// Target.Kind in Runner deps; each is a narrow closure built in
// main.go (Slack sender, wiki-store append, log) so this package
// stays free of adapter/store imports.
type DeliverFunc func(ctx context.Context, ownerID, actionID string, t Target, actionName, text string) error

// Deps wires the runner. Every field is required except Deliverers
// entries — a target kind with no deliverer fails that delivery
// (recorded in the ledger) without failing the run.
type Deps struct {
	Store    *Store
	Sessions *session.Manager
	Invoke   InvokeFunc
	// GetShard loads the envelope for shard-bound actions. Owner
	// scoping is NOT applied here — the action row's shard_id was
	// owner-validated at write time; the runner re-checks ownership
	// to catch a shard that changed hands since.
	GetShard func(ctx context.Context, id string) (*shards.Shard, error)
	// UserStatus returns the owner's current status ("approved",
	// "disabled", ...). Checked at every fire so disabling a user
	// silences their actions the same instant it kills sessions.
	UserStatus func(ctx context.Context, userID string) (string, error)
	Deliverers map[string]DeliverFunc
	// PageEvents, when set, powers page_saved triggers: the runner
	// subscribes for the process lifetime and fires watching actions
	// on matching book ids (Phase 3). Optional — without it,
	// page_saved actions simply never fire.
	PageEvents *pageevents.Bus
	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time
}

// Runner owns the schedule. Entries are rebuilt from the DB on
// Start and whenever the store reports a change (Reload); one-shot
// run_at actions use timers outside the cron driver.
type Runner struct {
	deps Deps
	cron *cron.Cron

	mu       sync.Mutex
	entries  []cron.EntryID
	timers   []*time.Timer
	inFlight map[string]bool // actionID → a run is executing now
	// watchers maps book_id → the page_saved actions watching it
	// (rebuilt by Reload). lastEventFire backs the per-action
	// min_interval throttle for event triggers; throttled fires are
	// dropped silently — autosave bursts must not spam the ledger.
	watchers      map[string][]*Action
	lastEventFire map[string]time.Time

	eventsCancel context.CancelFunc

	active  sync.WaitGroup
	started bool
}

func NewRunner(deps Deps) (*Runner, error) {
	if deps.Store == nil || deps.Sessions == nil || deps.Invoke == nil {
		return nil, fmt.Errorf("actions: runner missing store/sessions/invoke")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Runner{
		deps:          deps,
		cron:          cron.New(cron.WithParser(CronParser)),
		inFlight:      make(map[string]bool),
		watchers:      make(map[string][]*Action),
		lastEventFire: make(map[string]time.Time),
	}, nil
}

// Start loads enabled actions, registers schedules, and starts the
// cron driver. Also reaps ledger rows stranded by a previous crash.
func (r *Runner) Start(ctx context.Context) error {
	if n, err := r.deps.Store.MarkInterruptedRuns(ctx); err != nil {
		log.Printf("[actions] warning: mark interrupted runs: %v", err)
	} else if n > 0 {
		log.Printf("[actions] marked %d interrupted run(s) from previous boot", n)
	}
	if err := r.Reload(ctx); err != nil {
		return err
	}
	if r.deps.PageEvents != nil {
		evCtx, cancel := context.WithCancel(context.Background())
		r.eventsCancel = cancel
		ch := r.deps.PageEvents.Subscribe(evCtx, 64)
		go func() {
			for e := range ch {
				if e.Kind == pageevents.KindPageSaved {
					r.onPageSaved(e)
				}
			}
		}()
	}
	r.cron.Start()
	r.started = true
	return nil
}

// onPageSaved fires every enabled page_saved action watching the
// event's book, after two gates evaluated synchronously (the bus
// consumer is single-goroutine, so the throttle map needs no extra
// locking discipline beyond r.mu):
//
//  1. Self-trigger loop prevention: an event for a page that is one
//     of the action's own page targets is ignored — otherwise an
//     action that appends to a page in its watched book re-triggers
//     itself forever.
//  2. min_interval throttle: fires inside the window are dropped
//     WITHOUT a ledger row. The notes editor autosaves on a 500ms
//     debounce; a typing session is one event burst, not thirty
//     scheduled runs.
func (r *Runner) onPageSaved(e pageevents.Event) {
	r.mu.Lock()
	watching := append([]*Action(nil), r.watchers[e.BookID]...)
	r.mu.Unlock()

	for _, a := range watching {
		selfTarget := false
		for _, t := range a.ReportTargets {
			if t.Kind == "page" && t.PageID == e.PageID {
				selfTarget = true
				break
			}
		}
		if selfTarget {
			continue
		}

		now := r.deps.Now()
		r.mu.Lock()
		last := r.lastEventFire[a.ID]
		throttled := a.MinIntervalSeconds > 0 && now.Sub(last) < time.Duration(a.MinIntervalSeconds)*time.Second
		if !throttled {
			r.lastEventFire[a.ID] = now
		}
		r.mu.Unlock()
		if throttled {
			continue
		}

		note := fmt.Sprintf("A page was just saved in a book you watch.\nbook: %s\npage: %s\npayload: %s",
			e.BookID, e.PageID, string(e.Payload))
		actionID := a.ID
		r.active.Add(1)
		go func() {
			defer r.active.Done()
			r.fireEvent(actionID, "page_saved", note)
		}()
	}
}

// FireWebhook fires a webhook action by bearer token. The payload
// (truncated) rides into the prompt as event context. Returns the
// run id; "not found" covers bad tokens, non-webhook kinds, and
// disabled actions identically so the public endpoint can't be used
// to probe which of those failed.
func (r *Runner) FireWebhook(ctx context.Context, token string, payload []byte) (string, error) {
	a, err := r.deps.Store.GetByWebhookToken(ctx, token)
	if err != nil {
		return "", err
	}
	// Constant-time recheck — the unique-index lookup already
	// matched, but the comparison cost should not depend on it.
	if subtle.ConstantTimeCompare([]byte(a.WebhookToken), []byte(token)) != 1 || !a.Enabled {
		return "", ErrActionNotFound
	}

	now := r.deps.Now()
	r.mu.Lock()
	last := r.lastEventFire[a.ID]
	throttled := a.MinIntervalSeconds > 0 && now.Sub(last) < time.Duration(a.MinIntervalSeconds)*time.Second
	if !throttled {
		r.lastEventFire[a.ID] = now
	}
	r.mu.Unlock()
	if throttled {
		return "", fmt.Errorf("%w (min interval %ds)", ErrThrottled, a.MinIntervalSeconds)
	}

	const maxPayload = 8 * 1024
	body := string(payload)
	if len(body) > maxPayload {
		body = body[:maxPayload] + "\n…[truncated]"
	}
	note := "Webhook fired."
	if strings.TrimSpace(body) != "" {
		note += "\npayload:\n" + body
	}

	runID, err := r.deps.Store.StartRun(ctx, a.ID, "webhook")
	if err != nil {
		return "", err
	}
	r.active.Add(1)
	go func() {
		defer r.active.Done()
		r.execute(a.ID, runID, "webhook", note)
	}()
	return runID, nil
}

// Reload rebuilds every schedule entry from the DB. Cheap enough to
// run on every CRUD (the store's OnChange hook) — actions number in
// the dozens, not thousands.
func (r *Runner) Reload(ctx context.Context) error {
	acts, err := r.deps.Store.ListEnabled(ctx)
	if err != nil {
		return fmt.Errorf("actions: reload: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.entries {
		r.cron.Remove(id)
	}
	r.entries = r.entries[:0]
	for _, t := range r.timers {
		t.Stop()
	}
	r.timers = r.timers[:0]

	r.watchers = make(map[string][]*Action)
	now := r.deps.Now()
	for _, a := range acts {
		a := a
		switch a.TriggerKind {
		case TriggerCron:
			sched, err := CronParser.Parse(a.Cron)
			if err != nil {
				// Validate gates writes, so this is a corrupt row —
				// log loudly, skip, never kill the reload.
				log.Printf("[actions] %s (%s): unparseable cron %q: %v", a.Name, a.ID, a.Cron, err)
				continue
			}
			loc, locErr := time.LoadLocation(a.Timezone)
			if locErr != nil {
				// Should not happen — Validate gates writes and the binary
				// embeds tzdata — but if a zone ever fails to load, fall
				// back to UTC loudly rather than silently mis-scheduling.
				log.Printf("[actions] %s (%s): timezone %q failed to load, running in UTC: %v", a.Name, a.ID, a.Timezone, locErr)
			} else if a.Timezone != "UTC" {
				sched = wrapTZ(sched, loc)
			}
			id := r.cron.Schedule(sched, cron.FuncJob(func() { r.fire(a.ID, "cron") }))
			r.entries = append(r.entries, id)
		case TriggerOneShot:
			delay := a.RunAt.Sub(now)
			if delay < 0 {
				// Missed one-shots don't fire late (spec: no
				// catch-up); disable so it stops reloading forever.
				go r.disableOneShot(a.ID, "missed")
				continue
			}
			timer := time.AfterFunc(delay, func() { r.fire(a.ID, "run_at") })
			r.timers = append(r.timers, timer)
		case TriggerPageSaved:
			r.watchers[a.WatchBookID] = append(r.watchers[a.WatchBookID], a)
		case TriggerWebhook:
			// Fired via FireWebhook — nothing to register.
		}
	}
	log.Printf("[actions] schedule rebuilt: %d entr(ies), %d book watch(es)", len(acts), len(r.watchers))
	return nil
}

// Stop halts the driver and waits for in-flight runs.
func (r *Runner) Stop() {
	if r.started {
		<-r.cron.Stop().Done()
	}
	if r.eventsCancel != nil {
		r.eventsCancel()
	}
	r.mu.Lock()
	for _, t := range r.timers {
		t.Stop()
	}
	r.mu.Unlock()
	r.active.Wait()
}

// RunNow fires an action immediately (trigger="manual"), regardless
// of its enabled bit — disabled actions stay testable from the panel.
// Returns the ledger run id; the execution itself is asynchronous.
func (r *Runner) RunNow(ctx context.Context, actionID, ownerID string, isAdmin bool) (string, error) {
	a, err := r.deps.Store.Get(ctx, actionID, ownerID, isAdmin)
	if err != nil {
		return "", err
	}
	runID, err := r.deps.Store.StartRun(ctx, a.ID, "manual")
	if err != nil {
		return "", err
	}
	r.active.Add(1)
	go func() {
		defer r.active.Done()
		r.execute(a.ID, runID, "manual", "")
	}()
	return runID, nil
}

// fire is the cron/timer entry point: open a ledger row, execute.
func (r *Runner) fire(actionID, trigger string) {
	r.fireEvent(actionID, trigger, "")
}

// fireEvent is fire with event context — the note rides into the
// prompt so the model knows WHAT triggered it (which page saved,
// what the webhook carried).
func (r *Runner) fireEvent(actionID, trigger, eventNote string) {
	r.active.Add(1)
	defer r.active.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	runID, err := r.deps.Store.StartRun(ctx, actionID, trigger)
	cancel()
	if err != nil {
		log.Printf("[actions] %s: start run: %v", actionID, err)
		return
	}
	r.execute(actionID, runID, trigger, eventNote)
	if trigger == "run_at" {
		// One-shots fire exactly once; the disable also drops the
		// timer on the reload the store change triggers. Empty
		// status keeps the run's own outcome in last_status.
		r.disableOneShot(actionID, "")
	}
}

// execute owns one run end to end: gates → invoke → deliver → ledger.
func (r *Runner) execute(actionID, runID, trigger, eventNote string) {
	start := r.deps.Now()
	finish := func(res RunResult) {
		res.DurationMs = int(r.deps.Now().Sub(start) / time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		failures, err := r.deps.Store.FinishRun(ctx, runID, actionID, res)
		if err != nil {
			log.Printf("[actions] %s: finish run: %v", actionID, err)
			return
		}
		r.maybeTripBreaker(ctx, actionID, res.Status, failures)
	}

	// Overlap skip BEFORE any heavy work. The in-flight map is
	// per-process — fine for the single-gateway topology.
	r.mu.Lock()
	if r.inFlight[actionID] {
		r.mu.Unlock()
		finish(RunResult{Status: RunStatusSkippedOverlap})
		return
	}
	r.inFlight[actionID] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inFlight, actionID)
		r.mu.Unlock()
	}()

	loadCtx, cancelLoad := context.WithTimeout(context.Background(), 10*time.Second)
	a, err := r.deps.Store.Get(loadCtx, actionID, "", true)
	cancelLoad()
	if err != nil {
		finish(RunResult{Status: RunStatusError, Error: "load action: " + err.Error()})
		return
	}

	// Owner gate: a non-approved owner's actions are silenced, not
	// errored — re-approval resumes them without touching the breaker.
	if r.deps.UserStatus != nil {
		statusCtx, cancelStatus := context.WithTimeout(context.Background(), 5*time.Second)
		status, err := r.deps.UserStatus(statusCtx, a.OwnerID)
		cancelStatus()
		if err != nil {
			finish(RunResult{Status: RunStatusError, Error: "owner lookup: " + err.Error()})
			return
		}
		if status != "approved" {
			finish(RunResult{Status: RunStatusSkippedOwner})
			return
		}
	}

	// Budget gate: scheduled triggers stop at max_runs_per_day in a
	// rolling 24h window. Manual runs bypass — a human pressing the
	// button is not the runaway this protects against.
	if trigger != "manual" && a.MaxRunsPerDay > 0 {
		budgetCtx, cancelBudget := context.WithTimeout(context.Background(), 5*time.Second)
		n, err := r.deps.Store.CountScheduledRunsSince(budgetCtx, a.ID, runID, r.deps.Now().Add(-24*time.Hour))
		cancelBudget()
		if err != nil {
			finish(RunResult{Status: RunStatusError, Error: "budget check: " + err.Error()})
			return
		}
		if n >= a.MaxRunsPerDay {
			finish(RunResult{Status: RunStatusSkippedBudget})
			return
		}
	}

	// Envelope: user (nil overrides = trusted "run as you"),
	// ephemeral (prompt-only), or a shard's envelope. Legacy rows
	// have envelope='' in memory only via old test fixtures — the
	// shard_id check keeps them behaving as before.
	var overrides *pipeline.ShardOverrides
	switch {
	case a.Envelope == EnvelopeEphemeral:
		overrides = pipeline.EphemeralOverrides()
	case a.Envelope == EnvelopeShard || a.ShardID != "":
		if r.deps.GetShard == nil {
			finish(RunResult{Status: RunStatusError, Error: "shard-bound action but no shard store wired"})
			return
		}
		if a.ShardID == "" {
			// envelope=shard with the shard deleted out from under it
			// (ON DELETE SET NULL) — the breaker normally disables
			// these, but a manual run-now can still reach here.
			finish(RunResult{Status: RunStatusError, Error: "shard envelope but no shard bound (shard deleted?)"})
			return
		}
		shardCtx, cancelShard := context.WithTimeout(context.Background(), 5*time.Second)
		sh, err := r.deps.GetShard(shardCtx, a.ShardID)
		cancelShard()
		if err != nil {
			finish(RunResult{Status: RunStatusError, Error: "load shard: " + err.Error()})
			return
		}
		if !sh.Active() {
			finish(RunResult{Status: RunStatusError, Error: "shard " + sh.ID + " is disabled"})
			return
		}
		if sh.OwnerID != a.OwnerID {
			// The shard changed hands since the action was written.
			finish(RunResult{Status: RunStatusError, Error: "shard owner no longer matches action owner"})
			return
		}
		overrides = pipeline.OverridesForShard(sh)
	}

	// Stable session per action — history + committed facts
	// accumulate across runs like the config scheduler's tasks do
	// (scheduler.go:164), under the shard's scope when enveloped.
	sess := r.deps.Sessions.GetOrCreate("action:"+a.ID, "action:"+a.ID)
	sess.SetIdentity("actions", a.OwnerID)

	// on_content appends the quiet-sentinel contract so authors
	// don't have to remember the magic word themselves; event
	// triggers append their context so the model knows what fired.
	prompt := a.Prompt
	if eventNote != "" {
		prompt += "\n\n[Trigger event]\n" + eventNote
	}
	if a.DeliveryPolicy == "on_content" {
		prompt += quietInstruction
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), time.Duration(a.TimeoutSeconds)*time.Second)
	text, info, err := r.deps.Invoke(runCtx, sess, prompt, overrides)
	timedOut := runCtx.Err() == context.DeadlineExceeded
	cancelRun()

	res := RunResult{Status: RunStatusOK, Output: text}
	if info != nil {
		res.ModelID = info.ModelID
		res.InTokens = info.InputTokens
		res.OutTokens = info.OutputTokens
	}
	if err != nil {
		status := RunStatusError
		if timedOut {
			status = RunStatusTimeout
		}
		finish(RunResult{Status: status, Error: err.Error(), ModelID: res.ModelID})
		return
	}

	// Quiet suppression: under on_content, a sentinel reply is a
	// successful run that deliberately delivers nothing. The reply
	// is still ledgered for the panel's history view.
	if a.DeliveryPolicy == "on_content" && isQuietReply(text) {
		res.Status = RunStatusSkippedQuiet
		log.Printf("[actions] %s (%s): quiet — nothing to report", a.Name, a.ID)
		finish(res)
		return
	}

	// Delivery fan-out: every target gets attempted; one failure
	// doesn't starve the rest. A run with NO successful delivery is
	// an error (the report never reached anyone).
	delivered := 0
	for _, t := range a.ReportTargets {
		dr := DeliveryResult{Kind: t.Kind}
		fn, ok := r.deps.Deliverers[t.Kind]
		if !ok {
			dr.Error = "no deliverer for kind " + t.Kind
		} else {
			dCtx, dCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := fn(dCtx, a.OwnerID, a.ID, t, a.Name, text); err != nil {
				dr.Error = err.Error()
			} else {
				dr.OK = true
				delivered++
			}
			dCancel()
		}
		res.Deliveries = append(res.Deliveries, dr)
	}
	if delivered == 0 && len(a.ReportTargets) > 0 {
		res.Status = RunStatusError
		res.Error = "no delivery target succeeded"
	}

	log.Printf("[actions] %s (%s): %s in %s, %d/%d deliveries",
		a.Name, a.ID, res.Status, r.deps.Now().Sub(start).Round(time.Millisecond),
		delivered, len(a.ReportTargets))
	finish(res)
}

// maybeTripBreaker disables an action whose consecutive-failure
// count reached its cap and reports the trip through the first
// working deliverer — an unattended action must not fail silently
// forever, and must not burn tokens forever either.
func (r *Runner) maybeTripBreaker(ctx context.Context, actionID, status string, failures int) {
	if status != RunStatusError && status != RunStatusTimeout {
		return
	}
	a, err := r.deps.Store.Get(ctx, actionID, "", true)
	if err != nil || failures < a.MaxConsecutiveFailures {
		return
	}
	if err := r.deps.Store.SetEnabled(ctx, actionID, "", true, false); err != nil {
		log.Printf("[actions] %s: breaker disable failed: %v", actionID, err)
		return
	}
	log.Printf("[actions] %s (%s): circuit breaker tripped after %d consecutive failures — disabled",
		a.Name, a.ID, failures)
	notice := fmt.Sprintf("Scheduled action %q disabled itself after %d consecutive failures. "+
		"Re-enable it from the Scheduled panel once the cause is fixed.", a.Name, failures)
	for _, t := range a.ReportTargets {
		if fn, ok := r.deps.Deliverers[t.Kind]; ok {
			if fn(ctx, a.OwnerID, a.ID, t, a.Name, notice) == nil {
				return
			}
		}
	}
}

// disableOneShot flips a run_at action off after it fired or was
// missed. A non-empty status stamps last_status (the "missed" case);
// empty leaves the run's own outcome in place.
func (r *Runner) disableOneShot(actionID, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.deps.Store.SetEnabled(ctx, actionID, "", true, false); err != nil {
		log.Printf("[actions] one-shot %s: disable: %v", actionID, err)
	}
	if status != "" {
		_, _ = r.deps.Store.pool.ExecContext(ctx,
			`UPDATE scheduled_actions SET last_status = $2 WHERE id = $1::uuid`,
			actionID, status)
	}
}

// wrapTZ evaluates a cron schedule in the action's timezone. robfig
// schedules are wall-clock-relative to the time passed in; shifting
// in and out keeps "0 7 * * *" meaning 7 a.m. local.
type tzSchedule struct {
	inner cron.Schedule
	loc   *time.Location
}

func wrapTZ(s cron.Schedule, loc *time.Location) cron.Schedule {
	return tzSchedule{inner: s, loc: loc}
}

func (t tzSchedule) Next(from time.Time) time.Time {
	return t.inner.Next(from.In(t.loc))
}
