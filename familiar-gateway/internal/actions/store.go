// Package actions implements SCHEDULED-ACTIONS-SPEC Phase 1: DB-backed
// scheduled actions that run a prompt through the pipeline — inside a
// shard envelope when one is bound — and deliver the result to report
// targets (Slack, a wiki/note page, the log). The Store owns rows and
// the run ledger; the Runner (runner.go) owns the clock.
package actions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/familiar/gateway/internal/db"
)

// Target is one delivery destination. Kind selects which other
// fields apply:
//
//	{"kind":"slack","channel_id":"C0..."}
//	{"kind":"page","book_slug":"personal","page_id":"<uuid>"}
//	{"kind":"conversation","conversation_id":"<uuid>"}
//	{"kind":"log"}
//
// book_slug accepts the same "personal" alias the console wiki
// routes use. A conversation target submitted WITHOUT an id is
// auto-filled by the create/patch handlers with a dedicated
// "Scheduled: <name>" thread; by the time a row reaches the store
// the id is always present.
type Target struct {
	Kind           string `json:"kind"`
	ChannelID      string `json:"channel_id,omitempty"`
	BookSlug       string `json:"book_slug,omitempty"`
	PageID         string `json:"page_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// Trigger kinds (SCHEDULED-ACTIONS-SPEC Phase 3).
const (
	TriggerCron      = "cron"
	TriggerOneShot   = "one_shot"
	TriggerPageSaved = "page_saved"
	TriggerWebhook   = "webhook"
)

// Envelope modes — what context the run executes inside:
//
//	user      — trusted path, full scope: the owner's memory,
//	            system prompt, and full toolbox ("run as you").
//	ephemeral — nothing but the prompt: no system prompt, no memory
//	            retrieval, no tools, no session hydration, no commits.
//	shard     — a user shard's envelope (requires shard_id).
const (
	EnvelopeUser      = "user"
	EnvelopeEphemeral = "ephemeral"
	EnvelopeShard     = "shard"
)

// Action is one scheduled-actions row.
type Action struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	ShardID string `json:"shard_id,omitempty"` // set iff envelope == "shard"
	// Envelope selects the run context (user / ephemeral / shard).
	// Empty input is derived in Validate: shard_id set → shard,
	// otherwise user — which is also the pre-envelope legacy shape.
	Envelope string `json:"envelope"`
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	// TriggerKind selects the firing mechanism; empty input is
	// derived in Validate (cron set → cron, run_at set → one_shot).
	TriggerKind string     `json:"trigger_kind"`
	Cron        string     `json:"cron,omitempty"`
	RunAt       *time.Time `json:"run_at,omitempty"`
	// WatchBookID/Slug apply to page_saved: the book whose saves
	// fire this action. ID is resolved by the handler at write time
	// (events carry book ids); slug is kept for display.
	WatchBookID   string `json:"watch_book_id,omitempty"`
	WatchBookSlug string `json:"watch_book_slug,omitempty"`
	// MinIntervalSeconds throttles event triggers (autosave bursts,
	// webhook floods): fires inside the window are dropped silently —
	// no ledger spam. Cron/one-shot ignore it.
	MinIntervalSeconds int `json:"min_interval_seconds"`
	// WebhookToken is the bearer secret for webhook actions,
	// generated server-side; exposed only through owner-scoped reads.
	WebhookToken           string   `json:"webhook_token,omitempty"`
	Timezone               string   `json:"timezone"`
	Enabled                bool     `json:"enabled"`
	ReportTargets          []Target `json:"report_targets"`
	DeliveryPolicy         string   `json:"delivery_policy"`
	TimeoutSeconds         int      `json:"timeout_seconds"`
	MaxConsecutiveFailures int      `json:"max_consecutive_failures"`
	ConsecutiveFailures    int      `json:"consecutive_failures"`
	// MaxRunsPerDay caps scheduled executions in a rolling 24h
	// window; 0 = unlimited. Manual run-now is exempt — a human at
	// the panel is not a runaway cron.
	MaxRunsPerDay int        `json:"max_runs_per_day"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastStatus    string     `json:"last_status,omitempty"`
}

// Run is one ledger row.
type Run struct {
	ID         string          `json:"id"`
	ActionID   string          `json:"action_id"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Status     string          `json:"status"`
	Trigger    string          `json:"trigger"`
	ModelID    string          `json:"model_id,omitempty"`
	Output     string          `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	InTokens   int             `json:"input_tokens,omitempty"`
	OutTokens  int             `json:"output_tokens,omitempty"`
	DurationMs int             `json:"duration_ms,omitempty"`
	Deliveries json.RawMessage `json:"deliveries,omitempty"`
}

// Run statuses. "running" rows left behind by a crash are flipped to
// error at Runner start.
const (
	RunStatusRunning        = "running"
	RunStatusOK             = "ok"
	RunStatusError          = "error"
	RunStatusTimeout        = "timeout"
	RunStatusSkippedOverlap = "skipped_overlap"
	RunStatusSkippedOwner   = "skipped_owner"
	// skipped_quiet: the model ran and answered the NOTHING_TO_REPORT
	// sentinel under delivery_policy=on_content — a successful run
	// with deliberately no delivery.
	RunStatusSkippedQuiet = "skipped_quiet"
	// skipped_budget: the action hit max_runs_per_day; nothing ran.
	RunStatusSkippedBudget = "skipped_budget"
)

// maxStoredOutput caps the ledger's copy of a run's output. The
// ledger is for accountability, not a second copy of every artifact.
const maxStoredOutput = 16 * 1024

var ErrActionNotFound = errors.New("actions: not found")

// CronParser is the schedule grammar — standard 5-field crontab, the
// same parser the config-file scheduler uses. Shared so validation
// here and entry registration in the runner can't diverge.
var CronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// Store persists scheduled_actions + scheduled_action_runs.
// OnChange, when set, fires after every successful mutation — the
// runner hooks it to rebuild its schedule entries.
type Store struct {
	pool     *db.Pool
	OnChange func()
}

func NewStore(pool *db.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("actions: nil pool")
	}
	return &Store{pool: pool}, nil
}

func (s *Store) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

// Validate checks an action's shape before it touches the DB. The
// store owns validation (not the HTTP handler) so the runner's
// assumptions — parseable cron, known target kinds, sane bounds —
// hold for every row regardless of which caller wrote it.
func Validate(a *Action) error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("actions: name required")
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return fmt.Errorf("actions: prompt required")
	}
	if a.OwnerID == "" {
		return fmt.Errorf("actions: owner_id required")
	}
	// Envelope: derive for legacy callers, then enforce coherence —
	// shard_id travels with (and only with) envelope=shard. A shard
	// deleted out from under an action leaves envelope=shard with no
	// shard_id; re-enabling that row fails here until the owner picks
	// a new envelope (the existing shard_deleted contract).
	if a.Envelope == "" {
		if a.ShardID != "" {
			a.Envelope = EnvelopeShard
		} else {
			a.Envelope = EnvelopeUser
		}
	}
	switch a.Envelope {
	case EnvelopeUser, EnvelopeEphemeral:
		if a.ShardID != "" {
			return fmt.Errorf("actions: envelope %q cannot carry shard_id", a.Envelope)
		}
	case EnvelopeShard:
		if a.ShardID == "" {
			return fmt.Errorf("actions: envelope shard requires shard_id")
		}
	default:
		return fmt.Errorf("actions: unknown envelope %q", a.Envelope)
	}
	// Derive the kind for legacy callers that only set a schedule.
	if a.TriggerKind == "" {
		switch {
		case a.Cron != "":
			a.TriggerKind = TriggerCron
		case a.RunAt != nil:
			a.TriggerKind = TriggerOneShot
		default:
			return fmt.Errorf("actions: trigger_kind (or cron / run_at) required")
		}
	}
	switch a.TriggerKind {
	case TriggerCron:
		if a.Cron == "" || a.RunAt != nil {
			return fmt.Errorf("actions: cron trigger requires cron and no run_at")
		}
		if _, err := CronParser.Parse(a.Cron); err != nil {
			return fmt.Errorf("actions: invalid cron %q: %w", a.Cron, err)
		}
	case TriggerOneShot:
		if a.RunAt == nil || a.Cron != "" {
			return fmt.Errorf("actions: one_shot trigger requires run_at and no cron")
		}
	case TriggerPageSaved:
		if a.Cron != "" || a.RunAt != nil {
			return fmt.Errorf("actions: page_saved trigger takes no schedule")
		}
		if a.WatchBookID == "" {
			return fmt.Errorf("actions: page_saved requires watch_book_id (handlers resolve it from watch_book_slug)")
		}
	case TriggerWebhook:
		if a.Cron != "" || a.RunAt != nil {
			return fmt.Errorf("actions: webhook trigger takes no schedule")
		}
		if a.WebhookToken == "" {
			return fmt.Errorf("actions: webhook requires webhook_token (handlers generate it)")
		}
	default:
		return fmt.Errorf("actions: unknown trigger_kind %q", a.TriggerKind)
	}
	if a.MinIntervalSeconds == 0 {
		a.MinIntervalSeconds = 60
	}
	if a.MinIntervalSeconds < 0 || a.MinIntervalSeconds > 86400 {
		return fmt.Errorf("actions: min_interval_seconds out of range [0, 86400]")
	}
	if a.Timezone != "" {
		if _, err := time.LoadLocation(a.Timezone); err != nil {
			return fmt.Errorf("actions: invalid timezone %q", a.Timezone)
		}
	}
	if len(a.ReportTargets) == 0 {
		return fmt.Errorf("actions: at least one report target required")
	}
	for i, t := range a.ReportTargets {
		switch t.Kind {
		case "slack":
			if t.ChannelID == "" {
				return fmt.Errorf("actions: target %d: slack requires channel_id", i)
			}
		case "slack_dm":
			// DMs the action OWNER's linked Slack identity; resolved
			// at delivery time, so no fields here.
		case "page":
			if t.BookSlug == "" || t.PageID == "" {
				return fmt.Errorf("actions: target %d: page requires book_slug and page_id", i)
			}
		case "conversation":
			if t.ConversationID == "" {
				return fmt.Errorf("actions: target %d: conversation requires conversation_id (handlers auto-create when omitted)", i)
			}
		case "push":
			// Like conversation: the output lands in a per-action thread
			// (handlers auto-create when omitted) AND a Web Push notifies
			// the owner, deep-linking to that thread.
			if t.ConversationID == "" {
				return fmt.Errorf("actions: target %d: push requires conversation_id (handlers auto-create when omitted)", i)
			}
		case "log":
		case "notify":
			// Push-only ping (no fields). Pairs with a real destination
			// target: the output goes to that destination (or just the
			// ledger), and this fires a PWA push linking to the run.
		case "none":
			// "Nowhere" — the run is still recorded in the ledger (visible
			// in the Actions pane), but the output isn't delivered anywhere.
			// For tool-driven actions whose only effect is the edit itself.
		default:
			return fmt.Errorf("actions: target %d: unknown kind %q", i, t.Kind)
		}
	}
	switch a.DeliveryPolicy {
	case "", "always":
		a.DeliveryPolicy = "always"
	case "on_content":
		// Enforced by the runner: a reply matching the
		// NOTHING_TO_REPORT sentinel records skipped_quiet and
		// delivers nothing.
	default:
		return fmt.Errorf("actions: invalid delivery_policy %q", a.DeliveryPolicy)
	}
	if a.TimeoutSeconds == 0 {
		a.TimeoutSeconds = 600
	}
	if a.TimeoutSeconds < 30 || a.TimeoutSeconds > 3600 {
		return fmt.Errorf("actions: timeout_seconds out of range [30, 3600]")
	}
	if a.MaxConsecutiveFailures == 0 {
		a.MaxConsecutiveFailures = 5
	}
	if a.MaxConsecutiveFailures < 1 || a.MaxConsecutiveFailures > 100 {
		return fmt.Errorf("actions: max_consecutive_failures out of range [1, 100]")
	}
	if a.MaxRunsPerDay < 0 || a.MaxRunsPerDay > 1000 {
		return fmt.Errorf("actions: max_runs_per_day out of range [0, 1000]")
	}
	if a.Timezone == "" {
		a.Timezone = "UTC"
	}
	return nil
}

const actionCols = `
	id::text, owner_id, COALESCE(shard_id, ''), envelope, name, prompt,
	trigger_kind, COALESCE(cron, ''), run_at,
	COALESCE(watch_book_id::text, ''), watch_book_slug,
	min_interval_seconds, COALESCE(webhook_token, ''),
	timezone, enabled, report_targets,
	delivery_policy, timeout_seconds, max_consecutive_failures,
	consecutive_failures, max_runs_per_day, created_at, updated_at,
	last_run_at, COALESCE(last_status, '')`

func scanAction(sc interface{ Scan(...any) error }) (*Action, error) {
	var a Action
	var targets []byte
	err := sc.Scan(
		&a.ID, &a.OwnerID, &a.ShardID, &a.Envelope, &a.Name, &a.Prompt,
		&a.TriggerKind, &a.Cron, &a.RunAt,
		&a.WatchBookID, &a.WatchBookSlug,
		&a.MinIntervalSeconds, &a.WebhookToken,
		&a.Timezone, &a.Enabled, &targets,
		&a.DeliveryPolicy, &a.TimeoutSeconds, &a.MaxConsecutiveFailures,
		&a.ConsecutiveFailures, &a.MaxRunsPerDay, &a.CreatedAt,
		&a.UpdatedAt, &a.LastRunAt, &a.LastStatus,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(targets, &a.ReportTargets); err != nil {
		return nil, fmt.Errorf("actions: corrupt report_targets on %s: %w", a.ID, err)
	}
	return &a, nil
}

func (s *Store) Create(ctx context.Context, a *Action) (*Action, error) {
	if err := Validate(a); err != nil {
		return nil, err
	}
	targets, err := json.Marshal(a.ReportTargets)
	if err != nil {
		return nil, fmt.Errorf("actions: marshal targets: %w", err)
	}
	var shardID any
	if a.ShardID != "" {
		shardID = a.ShardID
	}
	var cronExpr any
	if a.Cron != "" {
		cronExpr = a.Cron
	}
	// New actions are always armed — creating a schedule means
	// wanting it to run; disabling is an explicit act afterward.
	row := s.pool.QueryRowContext(ctx, `
		INSERT INTO scheduled_actions (
		    owner_id, shard_id, envelope, name, prompt, trigger_kind, cron,
		    run_at, watch_book_id, watch_book_slug,
		    min_interval_seconds, webhook_token, timezone,
		    enabled, report_targets, delivery_policy, timeout_seconds,
		    max_consecutive_failures, max_runs_per_day
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,true,$14,$15,$16,$17,$18)
		RETURNING `+actionCols,
		a.OwnerID, shardID, a.Envelope, a.Name, a.Prompt, a.TriggerKind, cronExpr,
		a.RunAt, nullIfEmpty(a.WatchBookID), a.WatchBookSlug,
		a.MinIntervalSeconds, nullIfEmpty(a.WebhookToken), a.Timezone,
		targets, a.DeliveryPolicy,
		a.TimeoutSeconds, a.MaxConsecutiveFailures, a.MaxRunsPerDay)
	created, err := scanAction(row)
	if err != nil {
		return nil, fmt.Errorf("actions: create: %w", err)
	}
	s.changed()
	return created, nil
}

// Get loads one action. ownerID scoping mirrors the shards store:
// non-admin callers only see their own rows (a non-owned id reads as
// not-found, never 403, so ids can't be probed).
func (s *Store) Get(ctx context.Context, id, ownerID string, isAdmin bool) (*Action, error) {
	q := `SELECT ` + actionCols + ` FROM scheduled_actions WHERE id = $1::uuid`
	args := []any{id}
	if !isAdmin {
		q += ` AND owner_id = $2`
		args = append(args, ownerID)
	}
	a, err := scanAction(s.pool.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrActionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("actions: get: %w", err)
	}
	return a, nil
}

// List returns actions newest-first. Empty ownerID with isAdmin lists
// every action on the instance.
func (s *Store) List(ctx context.Context, ownerID string, isAdmin bool) ([]*Action, error) {
	q := `SELECT ` + actionCols + ` FROM scheduled_actions`
	var args []any
	if !isAdmin || ownerID != "" {
		q += ` WHERE owner_id = $1`
		args = append(args, ownerID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.pool.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("actions: list: %w", err)
	}
	defer rows.Close()
	out := []*Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListEnabled returns every enabled action — the runner's reload set.
func (s *Store) ListEnabled(ctx context.Context) ([]*Action, error) {
	rows, err := s.pool.QueryContext(ctx,
		`SELECT `+actionCols+` FROM scheduled_actions WHERE enabled ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("actions: list enabled: %w", err)
	}
	defer rows.Close()
	out := []*Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Update replaces the mutable fields of an action (full-row PUT
// semantics at the store; the handler merges PATCH bodies before
// calling). Owner scoping as in Get.
func (s *Store) Update(ctx context.Context, a *Action, isAdmin bool) (*Action, error) {
	if err := Validate(a); err != nil {
		return nil, err
	}
	targets, err := json.Marshal(a.ReportTargets)
	if err != nil {
		return nil, fmt.Errorf("actions: marshal targets: %w", err)
	}
	var shardID any
	if a.ShardID != "" {
		shardID = a.ShardID
	}
	var cronExpr any
	if a.Cron != "" {
		cronExpr = a.Cron
	}
	q := `
		UPDATE scheduled_actions SET
		    shard_id = $2, envelope = $3, name = $4, prompt = $5,
		    trigger_kind = $6, cron = $7, run_at = $8, watch_book_id = $9,
		    watch_book_slug = $10, min_interval_seconds = $11,
		    webhook_token = $12, timezone = $13, report_targets = $14,
		    delivery_policy = $15, timeout_seconds = $16,
		    max_consecutive_failures = $17, max_runs_per_day = $18,
		    updated_at = NOW()
		WHERE id = $1::uuid`
	args := []any{a.ID, shardID, a.Envelope, a.Name, a.Prompt, a.TriggerKind,
		cronExpr, a.RunAt, nullIfEmpty(a.WatchBookID), a.WatchBookSlug,
		a.MinIntervalSeconds, nullIfEmpty(a.WebhookToken),
		a.Timezone, targets, a.DeliveryPolicy, a.TimeoutSeconds,
		a.MaxConsecutiveFailures, a.MaxRunsPerDay}
	if !isAdmin {
		q += ` AND owner_id = $19`
		args = append(args, a.OwnerID)
	}
	q += ` RETURNING ` + actionCols
	updated, err := scanAction(s.pool.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrActionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("actions: update: %w", err)
	}
	s.changed()
	return updated, nil
}

// SetEnabled flips the enabled bit. Re-enabling resets the failure
// counter — the breaker tripped on the OLD streak; a human turning
// the action back on is declaring a fresh start.
func (s *Store) SetEnabled(ctx context.Context, id, ownerID string, isAdmin, enabled bool) error {
	q := `UPDATE scheduled_actions
	         SET enabled = $2,
	             consecutive_failures = CASE WHEN $2 THEN 0 ELSE consecutive_failures END,
	             updated_at = NOW()
	       WHERE id = $1::uuid`
	args := []any{id, enabled}
	if !isAdmin {
		q += ` AND owner_id = $3`
		args = append(args, ownerID)
	}
	res, err := s.pool.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("actions: set enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrActionNotFound
	}
	s.changed()
	return nil
}

func (s *Store) Delete(ctx context.Context, id, ownerID string, isAdmin bool) error {
	q := `DELETE FROM scheduled_actions WHERE id = $1::uuid`
	args := []any{id}
	if !isAdmin {
		q += ` AND owner_id = $2`
		args = append(args, ownerID)
	}
	res, err := s.pool.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("actions: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrActionNotFound
	}
	s.changed()
	return nil
}

// ── Run ledger ────────────────────────────────────────────────────

// StartRun inserts a ledger row in the running state and returns its id.
func (s *Store) StartRun(ctx context.Context, actionID, trigger string) (string, error) {
	var id string
	err := s.pool.QueryRowContext(ctx, `
		INSERT INTO scheduled_action_runs (action_id, trigger)
		VALUES ($1::uuid, $2) RETURNING id::text`,
		actionID, trigger).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("actions: start run: %w", err)
	}
	return id, nil
}

// RunResult carries everything FinishRun stamps onto the ledger row
// and mirrors onto the action (last_run_at / last_status + the
// failure counter).
type RunResult struct {
	Status     string
	ModelID    string
	Output     string
	Error      string
	InTokens   int
	OutTokens  int
	DurationMs int
	Deliveries []DeliveryResult
}

// DeliveryResult records one target's outcome inside the run row.
type DeliveryResult struct {
	Kind  string `json:"kind"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// FinishRun completes a ledger row and updates the parent action's
// last_run/last_status + consecutive_failures. Returns the action's
// new consecutive-failure count so the runner can trip the breaker.
func (s *Store) FinishRun(ctx context.Context, runID, actionID string, r RunResult) (int, error) {
	output := r.Output
	if len(output) > maxStoredOutput {
		output = output[:maxStoredOutput] + "\n…[truncated]"
	}
	deliveries, _ := json.Marshal(r.Deliveries)
	if _, err := s.pool.ExecContext(ctx, `
		UPDATE scheduled_action_runs SET
		    finished_at = NOW(), status = $2, model_id = $3, output = $4,
		    error = $5, input_tokens = $6, output_tokens = $7,
		    duration_ms = $8, deliveries = $9
		WHERE id = $1::uuid`,
		runID, r.Status, nullIfEmpty(r.ModelID), nullIfEmpty(output),
		nullIfEmpty(r.Error), r.InTokens, r.OutTokens, r.DurationMs,
		deliveries); err != nil {
		return 0, fmt.Errorf("actions: finish run: %w", err)
	}

	// Failure accounting: error/timeout count toward the breaker;
	// ok — and skipped_quiet, which is a successful run that simply
	// had nothing to say — reset it; the other skips leave it alone.
	delta := ""
	switch r.Status {
	case RunStatusOK, RunStatusSkippedQuiet:
		delta = `consecutive_failures = 0,`
	case RunStatusError, RunStatusTimeout:
		delta = `consecutive_failures = consecutive_failures + 1,`
	}
	var failures int
	err := s.pool.QueryRowContext(ctx, `
		UPDATE scheduled_actions SET `+delta+`
		    last_run_at = NOW(), last_status = $2, updated_at = updated_at
		WHERE id = $1::uuid
		RETURNING consecutive_failures`,
		actionID, r.Status).Scan(&failures)
	if err != nil {
		return 0, fmt.Errorf("actions: stamp action after run: %w", err)
	}
	return failures, nil
}

// ListRuns returns the ledger newest-first, capped.
func (s *Store) ListRuns(ctx context.Context, actionID string, limit int) ([]*Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.QueryContext(ctx, `
		SELECT id::text, action_id::text, started_at, finished_at, status,
		       trigger, COALESCE(model_id,''), COALESCE(output,''),
		       COALESCE(error,''), COALESCE(input_tokens,0),
		       COALESCE(output_tokens,0), COALESCE(duration_ms,0),
		       COALESCE(deliveries,'null')
		FROM scheduled_action_runs WHERE action_id = $1::uuid
		ORDER BY started_at DESC LIMIT $2`, actionID, limit)
	if err != nil {
		return nil, fmt.Errorf("actions: list runs: %w", err)
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.ActionID, &r.StartedAt, &r.FinishedAt,
			&r.Status, &r.Trigger, &r.ModelID, &r.Output, &r.Error,
			&r.InTokens, &r.OutTokens, &r.DurationMs, &r.Deliveries); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DisableByShard disables every enabled action bound to the given
// shard and stamps last_status so the panel says WHY. Called by the
// admin shard-delete handler BEFORE the row is deleted — the FK is
// ON DELETE SET NULL, so without this hook deletion would silently
// promote the shard's actions from a bounded tool envelope to
// full-capability trusted runs. Demotion-by-deletion must be loud
// and inert, never an escalation.
func (s *Store) DisableByShard(ctx context.Context, shardID string) (int64, error) {
	if shardID == "" {
		return 0, nil
	}
	res, err := s.pool.ExecContext(ctx, `
		UPDATE scheduled_actions
		   SET enabled = false, last_status = 'shard_deleted', updated_at = NOW()
		 WHERE shard_id = $1 AND enabled`, shardID)
	if err != nil {
		return 0, fmt.Errorf("actions: disable by shard: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.changed()
	}
	return n, nil
}

// GetByWebhookToken resolves a webhook bearer token to its action.
// Token comparison happens IN the query (unique index lookup); the
// caller still must check enabled + trigger_kind. Not-found and
// wrong-kind read identically to callers (ErrActionNotFound) so the
// hook endpoint can't be used to probe token validity separately
// from action shape.
func (s *Store) GetByWebhookToken(ctx context.Context, token string) (*Action, error) {
	if token == "" {
		return nil, ErrActionNotFound
	}
	a, err := scanAction(s.pool.QueryRowContext(ctx,
		`SELECT `+actionCols+` FROM scheduled_actions
		  WHERE webhook_token = $1 AND trigger_kind = 'webhook'`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrActionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("actions: get by token: %w", err)
	}
	return a, nil
}

// CountScheduledRunsSince counts ledger rows that consumed (or
// attempted to consume) an execution in the window — the budget
// denominator. Manual runs are exempt by design; pure skips
// (overlap / owner / budget) never reached the pipeline and don't
// count either. excludeRunID drops the caller's own in-flight
// ledger row — a budgeted fire must not count itself.
func (s *Store) CountScheduledRunsSince(ctx context.Context, actionID, excludeRunID string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM scheduled_action_runs
		 WHERE action_id = $1::uuid
		   AND id <> $2::uuid
		   AND started_at > $3
		   AND trigger <> 'manual'
		   AND status NOT IN ('skipped_overlap', 'skipped_owner', 'skipped_budget')`,
		actionID, excludeRunID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("actions: count runs: %w", err)
	}
	return n, nil
}

// MarkInterruptedRuns flips runs stranded in "running" (a crash or
// restart mid-run) to error. Called once at Runner start so the
// ledger never shows a phantom in-flight run.
func (s *Store) MarkInterruptedRuns(ctx context.Context) (int64, error) {
	res, err := s.pool.ExecContext(ctx, `
		UPDATE scheduled_action_runs
		   SET status = 'error', error = 'interrupted by gateway restart',
		       finished_at = NOW()
		 WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
