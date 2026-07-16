// Package research implements the Phase 2 deep-research fan-out
// (RESEARCH-SKILL-SPEC §6): spawn_research_workers dispatches parallel
// single-purpose pipeline turns — "virtual shards" — each
// investigating one sub-question inside a locked-down ShardOverrides
// envelope and appending findings to a shared evidence page in the
// caller's "research" book. When a writer model is configured,
// compose_research_note (§6.5) additionally runs the note-writing
// pass as a no-tools completion pinned to that model.
//
// The tool returns immediately (§6.2). Workers run for minutes; the
// per-tool-call cap is 30 seconds — so the goroutines run on a
// context detached from the tool call's, and the orchestrating turn
// reads the evidence page later to synthesize. Containment is the
// envelope (§6.3, §9): four tools, one book, no memory retrieval or
// commit, no session trace. A prompt-injected worker can at worst
// graffiti the evidence page.
//
// The invoke pattern (session per worker + HandleShard behind an
// InvokeFunc closure) is copied from the scheduled-actions runner
// (internal/actions/runner.go:51, :455-516).
package research

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/skillpkg"
	"github.com/familiar/gateway/internal/skills"
)

const (
	toolName        = "spawn_research_workers"
	composeToolName = "compose_research_note"

	// Worker-pool defaults (RESEARCH-SKILL-SPEC §7). MaxWorkers stays
	// well under the inference slot count so the orchestrator and
	// interactive chat always have slots; the search budget is the
	// per-worker web_search grant stamped on the envelope.
	defaultMaxWorkers   = 3
	defaultSearchBudget = 4
	defaultWorkerTier   = "technical"

	// maxTasks matches the schema's maxItems. More than 8 sub-questions
	// per spawn means the decomposition is too fine — the orchestrator
	// should re-spawn with the leftovers after reading round one.
	maxTasks = 8

	// defaultMaxRounds is the gap-fill cap (§6.7): the initial batch
	// plus one automatic retry of failed sub-questions.
	defaultMaxRounds = 2

	// workerTimeout mirrors the pipeline's own 300s turn wall clock; a
	// worker that outlives the turn deadline inside the pipeline would
	// be cut off there anyway. runDeadline bounds the whole fan-out —
	// queued tasks that can't start within it fail with a clear line on
	// the page instead of running forever (§6.4).
	workerTimeout = 600 * time.Second
	runDeadline   = 20 * time.Minute

	// statusAppendTimeout bounds the best-effort failure/completion
	// appends, which run on fresh contexts because the run context is
	// typically already done when they fire.
	statusAppendTimeout = 30 * time.Second

	// workerMaxTokens is deliberately small (§6.3): findings land on
	// the evidence page via append_to_page, not in the chat reply — the
	// reply is a one-line status the orchestrator never even reads.
	workerMaxTokens = 2048

	// writerMaxTokens is the compose pass's answer budget (§6.5): the
	// writer's completion IS the note, so it gets the full default
	// reply allowance rather than the workers' stub.
	writerMaxTokens = 4096

	// writerTimeout bounds the single compose completion; long-form
	// generation on a big local model runs minutes, and the pipeline's
	// own 300s turn cap is the true ceiling anyway.
	writerTimeout = 600 * time.Second

	// maxEvidenceChars caps the evidence handed to the writer (~15K
	// tokens at len/4) so prompt + evidence + note fit a 64K slot. The
	// evidence page is read server-side, so this is the only cap — the
	// writer sees far more evidence than a 2,000-token tool result
	// could ever carry back to the orchestrator.
	maxEvidenceChars = 60000

	// truncated question length in page failure lines.
	questionPreviewRunes = 80
)

// The worker and writer role prompts ship INSIDE the builtin research
// package (references/worker-prompt.md, references/writer-prompt.md)
// and are read from the binary embed at construction — prompt content
// stays in the markdown layer; Go only substitutes {{SEARCH_BUDGET}}.
// The consts below are last-resort fallbacks so a packaging mistake
// degrades to working defaults instead of empty system prompts.
const (
	searchBudgetPlaceholder = "{{SEARCH_BUDGET}}"

	// fallbackWorkerPrompt mirrors references/worker-prompt.md
	// (RESEARCH-SKILL-SPEC §6.3). The page — not the chat reply — is
	// the worker's real output: MaxTokens is 2048 and the orchestrator
	// only ever reads the evidence page.
	fallbackWorkerPrompt = `You are a research worker investigating ONE sub-question.

Search the web — you have {{SEARCH_BUDGET}} searches, so batch your
queries; Brave snippets usually answer detail questions without a
fetch. Fetch at most 2-3 pages, and prefer primary/official sources
over SEO farms. Never re-fetch a URL that failed.

APPEND your findings to the evidence page with append_to_page (the
book_slug and page_slug are given in your task). Append a
'### <your sub-question>' heading first, then 2-6 compact bullets,
each formatted '- finding — [Source](url)'.

Your final chat reply must be a single status line (for example:
'DONE — 4 findings appended'). The page is your real output.`

	// fallbackWriterPrompt mirrors references/writer-prompt.md (§6.5).
	fallbackWriterPrompt = `You are the research writer. You receive a topic and an evidence log.

Write the research note in markdown, and NOTHING else. Structure:
Summary / Key findings / Details / Sources, with an inline [Title](URL)
citation at every claim, drawn only from the evidence. Time-sensitive
claims carry "as of <month year>". End on Sources — no "Open questions"
or "Further research" section.`

	// fallbackSynthesisPrompt mirrors references/synthesis-prompt.md
	// (§6.7). {{...}} placeholders are filled by SynthesisPrompt.
	fallbackSynthesisPrompt = `The research workers for "{{TOPIC}}" have finished. Their findings are on the evidence page: book_slug="{{EVIDENCE_BOOK}}", page_slug="{{EVIDENCE_PAGE}}".

1. read_page that evidence page — it is your only source; do not search.
2. update_page the note at book_slug="{{NOTE_BOOK}}", page_slug="{{NOTE_PAGE}}" with the full write-up (Summary / Key findings / Details / Sources — end on Sources, NO "Open questions" section), inline [Title](URL) citations from the evidence only.
3. save_fact 10-20 key facts, scope "user", tags ["research","{{TOPIC_SLUG}}"].
4. Reply with a <=200-word summary ending "Full write-up in your notes: Research: {{TOPIC}}."

Do not spawn more workers.`
)

// maxRolePromptBytes is a sanity cap on embedded role prompts — a
// prompt this size is a packaging bug, and unlike skill bodies these
// bypass capSkillBody (they never transit a tool result).
const maxRolePromptBytes = 64 * 1024

// promptFromPackage loads a role prompt from the embedded builtin
// package, falling back to the compiled default when the file is
// missing, empty, or implausibly large (a packaging regression, not a
// runtime state worth failing a spawn over).
func promptFromPackage(relPath, fallback string) string {
	b, err := skillpkg.BuiltinFile("research", relPath)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 || len(b) > maxRolePromptBytes {
		log.Printf("[research] %s not usable from the builtin package (err=%v, %d bytes) — using compiled fallback", relPath, err, len(b))
		return fallback
	}
	return strings.TrimSpace(string(b))
}

// workerAllowlist is the four-tool toolbox (§6.3, §9): search, fetch,
// and the two evidence-page verbs. No memory, no profile, no other
// tools — injection containment for workers that read hostile web
// content.
var workerAllowlist = []string{"web_search", "fetch_page", "read_page", "append_to_page"}

// InvokeFunc runs one prompt through the pipeline's shard path. The
// signature is copied from actions.InvokeFunc (runner.go:51): wired in
// main.go as a closure over pipeline.HandleShard, faked in tests so
// the skill's fan-out is testable without a pipeline.
type InvokeFunc func(ctx context.Context, sess *session.Session, prompt string, overrides *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error)

// Backend is the narrow slice of admin.WikiStore the skill uses.
// Defining the interface here keeps the test surface small (the wiki
// skill's WikiBackend is the model) and documents exactly which store
// capabilities the fan-out depends on.
type Backend interface {
	// EnsureResearchBook resolves the caller's PER-USER research
	// evidence book (slug research:{userID}), creating it idempotently.
	// Per-user by construction: no shared-slug collision, no membership
	// check, no fork race, caller always owner. Workers are confined to
	// it via BookAccess (§6.2, §9); it's hidden from every book listing.
	EnsureResearchBook(ctx context.Context, userID string) (*admin.Book, error)
	GetPage(ctx context.Context, bookID, pageSlug string) (*admin.WikiPage, error)
	CreatePage(ctx context.Context, bookID, userID, title, content, requestedSlug string) (*admin.WikiPage, error)
	// AppendPage is atomic in-DB concatenation — concurrent worker
	// appends can't clobber each other (§2, WIKI-SYNC).
	AppendPage(ctx context.Context, bookID, pageID, userID, text string) (*admin.WikiPage, error)
	// EnsurePersonalBook + UpdatePage back the compose pass (§6.5): the
	// note deliverable lives in the caller's personal book, created as
	// a stub and filled in by the writer envelope when it completes.
	EnsurePersonalBook(ctx context.Context, userID string) (*admin.Book, error)
	UpdatePage(ctx context.Context, bookID, pageSlug, userID string, p admin.PagePatch) (*admin.WikiPage, error)
}

// Options wires the skill. Invoke, Sessions, and Backend are required
// (main.go gates registration on all three existing); the worker knobs
// default when zero so tests and minimal configs stay short.
type Options struct {
	Invoke   InvokeFunc
	Sessions *session.Manager
	Backend  Backend

	// MaxWorkers caps concurrently running workers (default 3). Keep
	// ≤ inference slots − 2 so the orchestrator and interactive chat
	// keep their slots (RESEARCH-SKILL-SPEC §7).
	MaxWorkers int
	// WorkerSearchBudget is each worker's web_search grant, stamped as
	// ShardOverrides.SearchBudget (default 4).
	WorkerSearchBudget int
	// WorkerTier is the envelope TierHint (default "technical" =
	// chat model; "tier2" pins the sidecar model for cheap workers).
	WorkerTier string

	// WorkerModel pins workers to an explicit registry model ID via
	// ModelOverride, overriding WorkerTier's alias routing (the tier
	// still shapes the thinking budget). Empty = tier routing. main.go
	// validates the ID exists and speaks tools before wiring it.
	WorkerModel string

	// WriterModel enables the compose_research_note tool (§6.5): the
	// note-writing pass runs as a no-tools completion pinned to this
	// model. Empty = tool not offered; the orchestrating chat model
	// writes the note in-turn, exactly as before.
	WriterModel string

	// MaxRounds caps deep-run gap-fill rounds (§6.7). Default 2: the
	// initial batch plus one retry of any failed sub-questions.
	MaxRounds int

	// Runs + Synthesize wire autonomous background runs (§6.7). Both
	// nil ⇒ the skill runs synchronously (no run tracking; the user
	// drives synthesis with "continue"). They're attached late via
	// SetOrchestrator because the synthesize closure needs collaborators
	// (conversation store, push sender) built after the skill.
	Runs       RunStore
	Synthesize SynthesizeFunc
}

// RunStore persists autonomous-run state (§6.7). admin.ResearchRunStore
// satisfies it; a mock backs the tests.
type RunStore interface {
	Create(ctx context.Context, userID, convID, topic string, questions []string, evBookSlug, evPageSlug string) (*admin.ResearchRun, error)
	ActiveForConversation(ctx context.Context, userID, convID string) (*admin.ResearchRun, error)
	Get(ctx context.Context, id string) (*admin.ResearchRun, error)
	Update(ctx context.Context, id string, p admin.RunPatch) error
	// UpdateIfActive applies the patch only while the run is non-terminal
	// (compare-and-set) so a lifecycle transition never reverts a
	// terminal status a concurrent cancel wrote. Returns whether applied.
	UpdateIfActive(ctx context.Context, id string, p admin.RunPatch) (bool, error)
	// IncrementWorkerDone bumps live progress (§6.7): one more area done
	// plus its token + page tally, as each worker finishes.
	IncrementWorkerDone(ctx context.Context, id string, tokens int64, pages int) error
	// SetWorkerState transitions one area's state in the roster the card
	// renders (queued|active|done|failed), keyed by stable task index.
	SetWorkerState(ctx context.Context, id string, idx int, state string) error
}

// SynthesizeFunc runs the owner-path synthesis turn for a completed run
// (§6.7): it creates the note stub, drives one trusted pl.Handle turn
// that reads the evidence and fills the note + memory pass, delivers a
// summary to the run's conversation (+ mobile push), and marks the run
// done or failed itself. Built in main.go; blocking (the supervisor
// goroutine calls it and returns).
type SynthesizeFunc func(ctx context.Context, runID string)

// runSessionPrefix labels the session a synthesis turn runs under, so
// Execute can tell a synthesis turn from a user's kickoff turn and
// refuse spawn_research_workers inside a run (the loop is skill-driven,
// §6.7).
const runSessionPrefix = "research:run:"

// Skill implements skills.Skill: the spawn tool always, plus the
// compose tool when a writer model is configured.
type Skill struct {
	opts Options

	// Role prompts, resolved once at construction from the embedded
	// builtin package (references/*.md) with compiled fallbacks. The
	// worker prompt already has {{SEARCH_BUDGET}} substituted; the
	// synthesis prompt is a template rendered per run by SynthesisPrompt.
	workerPrompt  string
	writerPrompt  string
	synthesisTmpl string

	// sem is the GATEWAY-WIDE worker cap. It lives on the skill, not
	// in dispatch(): overlapping runs (a re-spawn while run one still
	// executes, two users researching at once) share the one pool, so
	// total in-flight workers never exceed MaxWorkers — the
	// "≤ inference slots − 2" invariant is global, not per-spawn.
	sem chan struct{}

	// rootCtx parents every run so Close() can cut detached workers on
	// gateway shutdown instead of leaving up to runDeadline of turns
	// racing the teardown.
	rootCtx context.Context
	cancel  context.CancelFunc

	// runCancels maps a run's DB id → the CancelFunc for its current
	// round's worker context, so a user "stop" (CancelRun) can cut the
	// in-flight workers. Replaced each round; removed on cancel or when
	// the run hands off to synthesis.
	runCancels sync.Map // string → context.CancelFunc

	// cancelledRuns records run ids a user stopped. It's the authority
	// (not the DB status, which a racing round-transition could briefly
	// overwrite): dispatch and advanceRun refuse to start/advance a
	// cancelled run and re-assert its failed status.
	cancelledRuns sync.Map // string → true
}

// CancelRun stops an active run — the backing for a user "stop". It flags
// the run cancelled (so no further round or synthesis proceeds) and cuts
// the current round's in-flight workers. The HTTP handler also marks the
// run failed in the store. Always returns true — the flag is set whether
// or not a worker context was currently registered.
func (s *Skill) CancelRun(runDBID string) bool {
	s.cancelledRuns.Store(runDBID, true)
	if v, ok := s.runCancels.LoadAndDelete(runDBID); ok {
		if cancel, ok := v.(context.CancelFunc); ok {
			cancel()
		}
	}
	return true
}

func (s *Skill) isCancelled(runDBID string) bool {
	_, ok := s.cancelledRuns.Load(runDBID)
	return ok
}

// finishCancelled re-asserts the terminal (failed) status of a cancelled
// run and drops its worker-cancel entry. Idempotent — the HTTP handler
// already marked it failed; this closes the window where a racing
// round-transition overwrote the status back to researching.
func (s *Skill) finishCancelled(ctx context.Context, runDBID string) {
	s.runCancels.Delete(runDBID)
	st := admin.RunStatusFailed
	reason := "stopped by user"
	_ = s.opts.Runs.Update(ctx, runDBID, admin.RunPatch{Status: &st, Error: &reason})
}

// validWorkerTiers mirrors shardModelOverride's TierHint aliases
// (pipeline/shards.go). shardModelOverride silently returns ok=false
// for anything else — the envelope would fall through to the trusted
// routing path, running the sidecar classifier per worker — so New
// validates instead of letting a typo reroute every worker silently.
var validWorkerTiers = map[string]bool{
	"tier1": true, "trivial": true,
	"tier2": true, "knowledge": true,
	"tier3": true, "technical": true,
	"tier4": true, "deep": true, "deep_reasoning": true,
}

// New constructs the skill, defaulting the zero-valued worker knobs.
func New(opts Options) *Skill {
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = defaultMaxWorkers
	}
	if opts.WorkerSearchBudget <= 0 {
		opts.WorkerSearchBudget = defaultSearchBudget
	}
	if opts.WorkerTier == "" {
		opts.WorkerTier = defaultWorkerTier
	}
	if tier := strings.ToLower(opts.WorkerTier); !validWorkerTiers[tier] {
		log.Printf("[research] worker_tier %q is not a tier alias (tier1-4/trivial/knowledge/technical/deep/deep_reasoning) — using %q", opts.WorkerTier, defaultWorkerTier)
		opts.WorkerTier = defaultWorkerTier
	}
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = defaultMaxRounds
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := promptFromPackage("references/worker-prompt.md", fallbackWorkerPrompt)
	worker = strings.ReplaceAll(worker, searchBudgetPlaceholder, strconv.Itoa(opts.WorkerSearchBudget))
	return &Skill{
		opts:          opts,
		workerPrompt:  worker,
		writerPrompt:  promptFromPackage("references/writer-prompt.md", fallbackWriterPrompt),
		synthesisTmpl: promptFromPackage("references/synthesis-prompt.md", fallbackSynthesisPrompt),
		sem:           make(chan struct{}, opts.MaxWorkers),
		rootCtx:       ctx,
		cancel:        cancel,
	}
}

// SynthesisPrompt renders the §6.7 synthesis instruction for a run: the
// owner turn reads the evidence page and writes the note into the stub.
// Kept here (not in main.go) so the prompt lives in the markdown
// package with the rest of the skill's methodology.
func (s *Skill) SynthesisPrompt(topic, topicSlug, evidenceBookSlug, evidencePageSlug, noteBookSlug, notePageSlug string) string {
	r := strings.NewReplacer(
		"{{TOPIC}}", topic,
		"{{TOPIC_SLUG}}", topicSlug,
		"{{EVIDENCE_BOOK}}", evidenceBookSlug,
		"{{EVIDENCE_PAGE}}", evidencePageSlug,
		"{{NOTE_BOOK}}", noteBookSlug,
		"{{NOTE_PAGE}}", notePageSlug,
	)
	return r.Replace(s.synthesisTmpl)
}

// SetOrchestrator late-wires autonomous background runs (§6.7): the run
// store + the synthesize closure, built in main.go after the skill is
// already registered. Both nil-tolerant — passing nils leaves the skill
// synchronous.
func (s *Skill) SetOrchestrator(runs RunStore, synth SynthesizeFunc) {
	s.opts.Runs = runs
	s.opts.Synthesize = synth
}

// autonomous reports whether background-run orchestration is wired.
func (s *Skill) autonomous() bool {
	return s.opts.Runs != nil && s.opts.Synthesize != nil
}

func (s *Skill) Name() string { return "research-workers" }
func (s *Skill) Description() string {
	return "Fan out parallel research workers that investigate sub-questions and append findings to an evidence page"
}
func (s *Skill) Version() string { return "1.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }

// Close cancels rootCtx, cutting every in-flight worker and queued
// dispatch: on gateway shutdown the workers' Invoke calls fail fast
// with context.Canceled instead of driving pipeline turns against a
// tearing-down process for up to runDeadline.
func (s *Skill) Close() error {
	s.cancel()
	return nil
}

var spawnParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "topic": {
      "type": "string",
      "description": "Research topic — used to title the evidence page."
    },
    "tasks": {
      "type": "array",
      "minItems": 1,
      "maxItems": 8,
      "description": "A JSON array of task objects — NOT a string. Example: [{\"question\": \"What is X?\"}, {\"question\": \"How does Y work?\", \"hints\": \"prefer primary sources\"}]. Do not wrap the array in quotes.",
      "items": {
        "type": "object",
        "properties": {
          "question": {
            "type": "string",
            "description": "The sub-question this worker answers."
          },
          "hints": {
            "type": "string",
            "description": "Optional angles, source preferences, or constraints."
          }
        },
        "required": ["question"]
      }
    },
    "page_slug": {
      "type": "string",
      "description": "Existing evidence page to reuse (from a prior spawn). Omit to create one."
    }
  },
  "required": ["topic", "tasks"]
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	tools := []skills.ToolDefinition{
		{
			Name: toolName,
			Description: "Dispatch parallel research workers for a deep-research run. Each worker " +
				"investigates one sub-question with its own web search budget and appends findings " +
				"to the evidence page. Returns immediately; results accumulate on the page.",
			Parameters: spawnParams,
		},
	}
	// The compose tool is offered only when a writer model is
	// configured — tool presence is the capability signal the SKILL.md
	// branches on, the same trick deep-protocol.md uses for spawn.
	if s.opts.WriterModel != "" {
		tools = append(tools, skills.ToolDefinition{
			Name: composeToolName,
			Description: "Compose the research note on the dedicated writer model. Pass the evidence " +
				"page slug (deep runs) or the findings as evidence text. Creates the note in the " +
				"personal book and fills it in shortly; returns its location immediately — write the " +
				"chat summary and memory pass yourself from the evidence.",
			Parameters: composeParams,
		})
	}
	return tools
}

var composeParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "topic": {
      "type": "string",
      "description": "Research topic — used to title the note."
    },
    "evidence_page_slug": {
      "type": "string",
      "description": "Evidence page in the research book to write from (deep runs). The full page is read server-side."
    },
    "evidence": {
      "type": "string",
      "description": "Findings with source links, passed inline (quick/standard runs without an evidence page)."
    },
    "note_title": {
      "type": "string",
      "description": "Optional note title; defaults to 'Research: <topic>'."
    }
  },
  "required": ["topic"]
}`)

type taskSpec struct {
	Question string `json:"question"`
	Hints    string `json:"hints,omitempty"`
	// idx is the stable 0-based position in the run's roster, set after
	// unmarshal. It survives gap-fill (a failed task carries its idx into
	// the retry round) so per-worker state updates always hit the right
	// roster entry even as the round-local worker numbering resets.
	idx int
}

// UnmarshalJSON tolerates a bare-string task element ("just the
// question") in addition to the schema's {question, hints} object —
// another shape local models emit when they flatten the array.
func (t *taskSpec) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if len(s) > 0 && s[0] == '"' {
		var q string
		if err := json.Unmarshal([]byte(s), &q); err != nil {
			return err
		}
		t.Question = strings.TrimSpace(q)
		t.Hints = ""
		return nil
	}
	// Object form. Alias to avoid recursing into this method.
	type alias taskSpec
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*t = taskSpec(a)
	return nil
}

// taskList tolerates the ways local models mis-encode the tasks array.
// The schema wants a JSON array of {question, hints}; qwen routinely
// emits it double-encoded as a JSON *string* ("[{...}]"), or as a single
// object instead of a one-element array. Any of these now unmarshals to
// []taskSpec rather than failing the whole spawn — the exact bug that
// silently dropped deep runs back to inline searching.
type taskList []taskSpec

func (tl *taskList) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*tl = nil
		return nil
	}
	// Double-encoded: the whole array arrived wrapped in a JSON string.
	if s[0] == '"' {
		var inner string
		if err := json.Unmarshal([]byte(s), &inner); err != nil {
			return err
		}
		s = strings.TrimSpace(inner)
		if s == "" || s == "null" {
			*tl = nil
			return nil
		}
	}
	// A single task object instead of an array → wrap it.
	if s[0] == '{' {
		s = "[" + s + "]"
	}
	// []taskSpec (not taskList) so this method doesn't recurse; element
	// decoding still runs taskSpec.UnmarshalJSON for string/object items.
	var specs []taskSpec
	if err := json.Unmarshal([]byte(s), &specs); err != nil {
		return err
	}
	*tl = specs
	return nil
}

type spawnArgs struct {
	Topic    string   `json:"topic"`
	Tasks    taskList `json:"tasks"`
	PageSlug string   `json:"page_slug,omitempty"`
}

type composeArgs struct {
	Topic            string `json:"topic"`
	EvidencePageSlug string `json:"evidence_page_slug,omitempty"`
	Evidence         string `json:"evidence,omitempty"`
	NoteTitle        string `json:"note_title,omitempty"`
}

// Execute validates, ensures the research book + evidence page, then
// dispatches the workers and returns without waiting for them. The
// compose tool follows the same shape: stub note now, writer fills it
// in asynchronously (§6.5).
func (s *Skill) Execute(ctx context.Context, tool string, params json.RawMessage) (skills.ToolResult, error) {
	switch tool {
	case toolName:
		// always available
	case composeToolName:
		if s.opts.WriterModel == "" {
			// Not advertised without a writer model; refuse the
			// hallucinated dispatch too.
			return skills.ToolResult{}, fmt.Errorf("research: unknown tool %q", tool)
		}
	default:
		return skills.ToolResult{}, fmt.Errorf("research: unknown tool %q", tool)
	}
	if s.opts.Invoke == nil || s.opts.Sessions == nil || s.opts.Backend == nil {
		return skills.ToolResult{Error: "research workers are not fully wired on this gateway"}, nil
	}

	// Identity guard, mirroring the memory skill's posture: a blank
	// UserID means the caller never resolved identity, and everything
	// below (book membership, page writes, worker sessions) is keyed
	// to the calling user.
	sc, _ := skills.ContextFrom(ctx)
	userID := sc.UserID
	if userID == "" {
		return skills.ToolResult{Error: "research: no user identity on this turn — cannot dispatch workers"}, nil
	}

	if tool == composeToolName {
		return s.composeNote(ctx, userID, params)
	}

	// The gap-fill loop is skill-driven (§6.7): a synthesis turn runs
	// under a research:run:{id} session and must NOT spawn more workers
	// — it synthesizes from the evidence the skill already gathered.
	if strings.HasPrefix(sc.SessionID, runSessionPrefix) {
		return skills.ToolResult{Error: "you're synthesizing a research run — write the note from the evidence page, don't spawn more workers"}, nil
	}

	var args spawnArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	if strings.TrimSpace(args.Topic) == "" {
		return skills.ToolResult{Error: "topic is required"}, nil
	}
	if len(args.Tasks) == 0 {
		return skills.ToolResult{Error: "tasks must contain at least 1 sub-question"}, nil
	}
	if len(args.Tasks) > maxTasks {
		return skills.ToolResult{Error: fmt.Sprintf("tasks is capped at %d sub-questions per spawn — merge or defer the rest", maxTasks)}, nil
	}
	for i := range args.Tasks {
		if strings.TrimSpace(args.Tasks[i].Question) == "" {
			return skills.ToolResult{Error: fmt.Sprintf("tasks[%d].question is required", i)}, nil
		}
		// Stamp the stable roster index (survives gap-fill renumbering).
		args.Tasks[i].idx = i
	}

	book, err := s.ensureBook(ctx, userID)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("research book: %w", err)
	}

	page, userErr, err := s.ensurePage(ctx, userID, book, args)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("evidence page: %w", err)
	}
	if userErr != "" {
		return skills.ToolResult{Error: userErr}, nil
	}

	// Autonomous background run (§6.7): when orchestration is wired and
	// this is a real conversation turn, track a run so the skill can
	// drive gap-fill → synthesis → delivery itself and a progress card
	// can restore across tab-away. conversationID = the session id
	// (workspace chats key the session by conversation UUID).
	// convID never carries runSessionPrefix here — a run session was
	// already rejected above.
	convID := sc.SessionID
	if s.autonomous() && convID != "" {
		// Fast-path friendly error; the DB partial-unique index (via
		// Create → ErrActiveRunExists) is the atomic guarantee against
		// the check-then-create race.
		if _, aErr := s.opts.Runs.ActiveForConversation(ctx, userID, convID); aErr == nil {
			return skills.ToolResult{Error: "a research run is already underway in this conversation — wait for it to finish before starting another"}, nil
		} else if !errors.Is(aErr, admin.ErrRunNotFound) {
			return skills.ToolResult{}, fmt.Errorf("research run lookup: %w", aErr)
		}
		questions := make([]string, len(args.Tasks))
		for i, t := range args.Tasks {
			questions[i] = t.Question
		}
		run, cErr := s.opts.Runs.Create(ctx, userID, convID, args.Topic, questions, book.Slug, page.Slug)
		if errors.Is(cErr, admin.ErrActiveRunExists) {
			return skills.ToolResult{Error: "a research run is already underway in this conversation — wait for it to finish before starting another"}, nil
		}
		if cErr != nil {
			return skills.ToolResult{}, fmt.Errorf("create research run: %w", cErr)
		}
		s.dispatch(userID, book, page, newRunID(), run.ID, 1, args.Tasks)
		content := fmt.Sprintf(
			"Started an autonomous background research run on %q across %d areas. The system now runs the "+
				"whole thing itself — gap-fill, synthesis, the write-up note, the chat summary, and the memory "+
				"pass — and posts the results in this conversation when done.\n\n"+
				"YOUR TURN IS DONE: tell the user, briefly and warmly, that you've started researching and will "+
				"report back here with the findings when it's ready. Do NOT ask them to continue, say continue, "+
				"check back, or read the page — there is nothing for them to do and nothing more for you to do "+
				"this turn. Do not call read_page or write a note.",
			args.Topic, len(args.Tasks))
		return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
	}

	// Synchronous fallback (no orchestration wired): the caller drives
	// synthesis with "continue".
	s.dispatch(userID, book, page, newRunID(), "", 1, args.Tasks)
	content := fmt.Sprintf(
		"%d workers dispatched for %q → %s/%s. Findings accumulate on that page; "+
			"a completion line will appear when all workers finish. Read the page next turn to synthesize.",
		len(args.Tasks), args.Topic, book.Slug, page.Slug)
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}

// ensureBook resolves the caller's PER-USER research evidence book
// (slug research:{userID}), creating it idempotently. Each user gets
// their own — the store's deterministic slug means no shared-book
// collision, no membership question, no fork race, and the caller is
// always the owner (so writes always succeed). This replaced an
// earlier shared fixed-slug "research" book that only the first user
// could ever use.
func (s *Skill) ensureBook(ctx context.Context, userID string) (*admin.Book, error) {
	return s.opts.Backend.EnsureResearchBook(ctx, userID)
}

// ensurePage resolves or creates the evidence page. A given page_slug
// must exist (the orchestrator got it from a prior spawn's result);
// otherwise a fresh page is created with the sub-question checklist as
// its Plan section. CreatePage handles slug collisions itself
// (slugify + suffix), so re-researching the same topic just makes
// "research-topic-a1b2" style slugs.
func (s *Skill) ensurePage(ctx context.Context, userID string, book *admin.Book, args spawnArgs) (*admin.WikiPage, string, error) {
	if args.PageSlug != "" {
		page, err := s.opts.Backend.GetPage(ctx, book.ID, args.PageSlug)
		if errors.Is(err, admin.ErrPageNotFound) {
			return nil, fmt.Sprintf("evidence page %q not found in your research book — omit page_slug to create a fresh one", args.PageSlug), nil
		}
		if err != nil {
			return nil, "", err
		}
		return page, "", nil
	}
	page, err := s.opts.Backend.CreatePage(ctx, book.ID, userID, "Research: "+args.Topic, initialPageContent(args.Topic, args.Tasks), "")
	if err != nil {
		return nil, "", err
	}
	return page, "", nil
}

// composeNote runs the §6.5 writer pass: resolve the evidence, create
// the note as a stub in the caller's personal book, and dispatch a
// single no-tools completion on the configured writer model to fill it
// in. Same async shape as spawn — the tool returns immediately because
// a long-form completion on a big local model blows the 30s tool-call
// cap many times over.
func (s *Skill) composeNote(ctx context.Context, userID string, params json.RawMessage) (skills.ToolResult, error) {
	var args composeArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	if strings.TrimSpace(args.Topic) == "" {
		return skills.ToolResult{Error: "topic is required"}, nil
	}
	hasPage := strings.TrimSpace(args.EvidencePageSlug) != ""
	hasInline := strings.TrimSpace(args.Evidence) != ""
	if hasPage == hasInline {
		return skills.ToolResult{Error: "pass exactly one of evidence_page_slug (deep runs) or evidence (inline findings)"}, nil
	}

	// Resolve the evidence server-side. Reading the page here (not via
	// a read_page tool round-trip) is the point: the writer sees the
	// FULL evidence log, not a 2,000-token truncation of it.
	evidence := args.Evidence
	if hasPage {
		book, err := s.opts.Backend.EnsureResearchBook(ctx, userID)
		if err != nil {
			return skills.ToolResult{}, fmt.Errorf("research book: %w", err)
		}
		page, err := s.opts.Backend.GetPage(ctx, book.ID, args.EvidencePageSlug)
		if errors.Is(err, admin.ErrPageNotFound) {
			return skills.ToolResult{Error: fmt.Sprintf("evidence page %q not found in your research book — pass the findings inline as evidence instead", args.EvidencePageSlug)}, nil
		}
		if err != nil {
			return skills.ToolResult{}, fmt.Errorf("evidence page: %w", err)
		}
		evidence = page.Content
	}
	if len(evidence) > maxEvidenceChars {
		// Cut on a line boundary when one is reasonably close (keeps
		// citations whole), else back up to a rune boundary — a raw
		// byte slice could split a multi-byte rune and hand the writer
		// invalid UTF-8.
		cut := maxEvidenceChars
		if nl := strings.LastIndexByte(evidence[:cut], '\n'); nl > maxEvidenceChars/2 {
			cut = nl
		} else {
			for cut > 0 && !utf8.RuneStart(evidence[cut]) {
				cut--
			}
		}
		evidence = evidence[:cut] + "\n\n[evidence truncated at the writer's context cap]"
	}

	// The note stub goes in the personal book (§4 item 1) so the user
	// sees it appear immediately; the writer's completion replaces the
	// placeholder when it lands.
	personal, err := s.opts.Backend.EnsurePersonalBook(ctx, userID)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("personal book: %w", err)
	}
	title := strings.TrimSpace(args.NoteTitle)
	if title == "" {
		title = "Research: " + args.Topic
	}
	stub, err := s.opts.Backend.CreatePage(ctx, personal.ID, userID, title,
		"_Composing the research note on the writer model — this fills in shortly._", "")
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("note stub: %w", err)
	}

	runID := newRunID()
	s.dispatchWriter(userID, personal, stub, runID, args.Topic, evidence)

	content := fmt.Sprintf(
		"Writer dispatched (run %s) → note %q at %s/%s. It fills in shortly — write the chat "+
			"summary and run the memory pass from the evidence yourself now.",
		runID, title, personal.Slug, stub.Slug)
	// Stash the note location so the pipeline surfaces it in the "done"
	// event (auto-open + in-chat link), same as the create_page fallback
	// path — otherwise the compose path would orphan the note.
	loc, _ := json.Marshal(map[string]string{
		"book_slug": personal.Slug, "page_slug": stub.Slug, "title": title,
	})
	return skills.ToolResult{Content: content, Data: loc, Tokens: len(content) / 4}, nil
}

// dispatchWriter runs the single writer completion on a detached
// context, sharing the gateway-wide worker semaphore — the writer
// occupies an inference slot like any worker, and Close() cuts it on
// shutdown the same way. Acquisition is bounded by runDeadline so a
// compose queued behind a full worker fan-out fails visibly on the
// stub instead of sitting silent forever; the slot is released BEFORE
// the page write — the DB doesn't need an inference slot.
func (s *Skill) dispatchWriter(userID string, book *admin.Book, page *admin.WikiPage, runID, topic, evidence string) {
	log.Printf("[research] run %s: dispatching writer (model=%s, note=%s/%s, user=%s)",
		runID, s.opts.WriterModel, book.Slug, page.Slug, userID)
	// The IfMatch precondition is the stub's timestamp AS DISPATCHED —
	// the last state this code observed. Anything newer on the page at
	// delivery time is someone else's edit and must survive.
	ifMatch := page.UpdatedAt
	go func() {
		select {
		case s.sem <- struct{}{}:
		case <-s.rootCtx.Done():
			s.deliverToStub(userID, book, page, ifMatch, composeFailureText(s.rootCtx.Err()))
			return
		case <-time.After(runDeadline):
			s.deliverToStub(userID, book, page, ifMatch, composeFailureText(errors.New("queued behind research workers past the run deadline — retry when the run finishes")))
			return
		}

		text, err := func() (string, error) {
			defer func() { <-s.sem }()
			wctx, cancel := context.WithTimeout(s.rootCtx, writerTimeout)
			defer cancel()

			key := "research:" + runID + ":writer"
			sess := s.opts.Sessions.GetOrCreate(key, key)
			sess.SetIdentity("research", userID)

			overrides := &pipeline.ShardOverrides{
				ShardID:              "research-writer",
				SystemPrompt:         s.writerPrompt,
				SkipMemoryRetrieval:  true,
				SkipSessionHydration: true,
				SkipCommit:           true,
				ExcludeFromHot:       true,
				// No tools at all: the writer is a pure completion —
				// evidence in, note markdown out. An empty allowlist
				// advertises nothing and dispatches nothing.
				ToolAllowlist: []string{},
				ModelOverride: s.opts.WriterModel,
				MaxTokens:     writerMaxTokens,
			}
			prompt := "Topic: " + topic + "\n\nEvidence log:\n\n" + evidence
			out, _, err := s.opts.Invoke(wctx, sess, prompt, overrides)
			return out, err
		}()
		if err != nil {
			log.Printf("[research] run %s: writer failed: %v", runID, err)
			s.deliverToStub(userID, book, page, ifMatch, composeFailureText(err))
			return
		}
		note := strings.TrimSpace(text)
		if note == "" {
			s.deliverToStub(userID, book, page, ifMatch, composeFailureText(errors.New("writer returned an empty note")))
			return
		}
		s.deliverToStub(userID, book, page, ifMatch, note)
		log.Printf("[research] run %s: note composed (%d chars)", runID, len(note))
		// Evidence cleanup is left to the periodic sweep (§6.6): reaping
		// the page here raced two ways — it could delete the note's only
		// source when the write didn't actually land, or delete evidence
		// a concurrent re-spawn appended after this writer's snapshot.
		// The retention sweep bounds growth safely; the future
		// run-lifecycle owner will do race-free end-of-run cleanup.
	}()
}

func composeFailureText(cause error) string {
	return fmt.Sprintf("_Compose failed: %v. Ask me to write the note directly from the evidence._", cause)
}

// deliverToStub lands text on the note page without ever silently
// clobbering the user. Content-only + IfMatch is the merge-eligible
// case (WIKI-SYNC Phase 2): an untouched stub is replaced cleanly and
// concurrent user edits auto-merge server-side. A merge conflict
// (ErrPageStale) or a renamed page (slug changed → ErrPageNotFound)
// falls back to the ID-addressed atomic append, so the composed note
// is never dropped — the exact clobber-and-drop paths the wiki sync
// work closed stay closed here.
func (s *Skill) deliverToStub(userID string, book *admin.Book, page *admin.WikiPage, ifMatch time.Time, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), statusAppendTimeout)
	defer cancel()
	patch := admin.PagePatch{Content: &text, IfMatch: &ifMatch}
	_, err := s.opts.Backend.UpdatePage(ctx, book.ID, page.Slug, userID, patch)
	if err == nil {
		return
	}
	if !errors.Is(err, admin.ErrPageStale) && !errors.Is(err, admin.ErrPageNotFound) {
		log.Printf("[research] note update failed (book=%s page=%s): %v — falling back to append", book.ID, page.Slug, err)
	}
	if _, aErr := s.opts.Backend.AppendPage(ctx, book.ID, page.ID, userID, "\n---\n\n"+text); aErr != nil {
		log.Printf("[research] note append fallback failed (book=%s page=%s): %v", book.ID, page.ID, aErr)
	}
}

// initialPageContent is the evidence page skeleton (§4 item 4): the
// sub-question checklist under Plan, an empty Findings section the
// workers append under.
func initialPageContent(topic string, tasks []taskSpec) string {
	var b strings.Builder
	b.WriteString("# Research: ")
	b.WriteString(topic)
	b.WriteString("\n\n## Plan\n")
	for _, t := range tasks {
		b.WriteString("- [ ] ")
		b.WriteString(t.Question)
		b.WriteString("\n")
	}
	b.WriteString("\n## Findings\n")
	return b.String()
}

// dispatch fans a batch of tasks out to workers (§6.7): one goroutine
// per task through the gateway-wide MaxWorkers semaphore, each on a
// detached context (workerTimeout per worker, runDeadline for the batch
// — never the tool call's ctx, which dies ~30s after Execute returns).
// A supervisor waits for the pool, stamps the completion line, and —
// when autonomous — advances the run. runDBID is the research_runs id
// ("" in the synchronous fallback); round is the current gap-fill
// round; runLabel is the short per-batch hex for worker session keys
// and the evidence page's status lines.
func (s *Skill) dispatch(userID string, book *admin.Book, page *admin.WikiPage, runLabel, runDBID string, round int, tasks []taskSpec) {
	total := len(tasks)
	// Derived from rootCtx (not Background) so Close() cuts the run on
	// shutdown; s.sem (not a local) so overlapping runs share the one
	// gateway-wide MaxWorkers pool.
	// Cancelled between rounds → don't start another fan-out; re-assert
	// terminal in case a racing round-transition overwrote it.
	if runDBID != "" && s.isCancelled(runDBID) {
		fctx, fcancel := context.WithTimeout(context.Background(), statusAppendTimeout)
		s.finishCancelled(fctx, runDBID)
		fcancel()
		return
	}
	runCtx, cancelRun := context.WithTimeout(s.rootCtx, runDeadline)
	// Register this round's canceller so a user "stop" can cut the
	// workers mid-round (replaces the prior round's entry).
	if runDBID != "" {
		s.runCancels.Store(runDBID, cancelRun)
	}
	sem := s.sem
	systemPrompt := s.workerPrompt
	runID := runLabel

	var wg sync.WaitGroup
	var succeeded atomic.Int32
	// failed collects the tasks whose worker errored — the deterministic
	// gap signal the supervisor retries (§6.7).
	var failedMu sync.Mutex
	var failed []taskSpec
	recordFailure := func(t taskSpec) {
		failedMu.Lock()
		failed = append(failed, t)
		failedMu.Unlock()
	}

	log.Printf("[research] run %s (db=%s round=%d): dispatching %d worker(s) (max_workers=%d, budget=%d, tier=%s, page=%s/%s, user=%s)",
		runID, runDBID, round, total, s.opts.MaxWorkers, s.opts.WorkerSearchBudget, s.opts.WorkerTier, book.Slug, page.Slug, userID)

	for i, t := range tasks {
		wg.Add(1)
		// Workers are numbered 1..total — the index lands on the
		// user-visible evidence page ("worker 2 ... failed") and in the
		// per-worker session key, and 1-based matches the completion
		// line's "n/total" framing.
		go func(n int, t taskSpec) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				// Queued behind the semaphore until the run deadline —
				// surface it on the page like any other worker failure
				// so gap-fill retries only the missing tasks (§6.7).
				s.reportWorkerFailure(userID, book, page, n, t.Question,
					errors.New("run deadline reached before the worker could start"))
				recordFailure(t)
				s.setWorkerState(runDBID, t.idx, admin.WorkerFailed)
				s.reportWorkerProgress(runDBID, nil)
				return
			}
			defer func() { <-sem }()
			// Slot acquired → this area is now actively searching.
			s.setWorkerState(runDBID, t.idx, admin.WorkerActive)

			wctx, cancelWorker := context.WithTimeout(runCtx, workerTimeout)
			defer cancelWorker()

			// Fresh session per worker (§6.3): no hydration, no commit
			// (the envelope skips both), keyed uniquely so parallel
			// workers never share turn state.
			key := "research:" + runID + ":" + strconv.Itoa(n)
			sess := s.opts.Sessions.GetOrCreate(key, key)
			sess.SetIdentity("research", userID)

			overrides := &pipeline.ShardOverrides{
				ShardID:              "research-worker",
				SystemPrompt:         systemPrompt,
				SkipMemoryRetrieval:  true,
				SkipSessionHydration: true,
				SkipCommit:           true,
				// Belt over the SkipCommit suspenders (§6.3): even if a
				// future allowlist grows a memory-writing tool, worker
				// writes bypass the hot tier.
				ExcludeFromHot: true,
				ToolAllowlist:  append([]string(nil), workerAllowlist...),
				BookAccess:     []string{book.ID},
				// ModelOverride (when configured) pins workers to an
				// explicit registry model; TierHint still shapes the
				// thinking budget either way (shardModelOverride
				// returns both).
				ModelOverride: s.opts.WorkerModel,
				TierHint:      s.opts.WorkerTier,
				SearchBudget:  s.opts.WorkerSearchBudget,
				MaxTokens:     workerMaxTokens,
			}

			_, info, err := s.opts.Invoke(wctx, sess, taskPrompt(t, book.Slug, page.Slug), overrides)
			s.reportWorkerProgress(runDBID, info)
			if err != nil {
				log.Printf("[research] run %s: worker %d failed: %v", runID, n, err)
				s.reportWorkerFailure(userID, book, page, n, t.Question, err)
				recordFailure(t)
				s.setWorkerState(runDBID, t.idx, admin.WorkerFailed)
				return
			}
			s.setWorkerState(runDBID, t.idx, admin.WorkerDone)
			succeeded.Add(1)
		}(i+1, t)
	}

	// Supervisor: wait for the pool, stamp the completion line, then —
	// when autonomous — drive the next step (gap-fill or synthesis,
	// §6.7). The batch's own goroutine owns this so each round chains to
	// the next without a user turn.
	go func() {
		wg.Wait()
		cancelRun()
		n := int(succeeded.Load())
		line := fmt.Sprintf("\n---\nrun %s complete: %d/%d workers succeeded (%s UTC)\n",
			runID, n, total, time.Now().UTC().Format("15:04"))
		s.appendStatus(userID, book, page, line)
		log.Printf("[research] run %s complete: %d/%d workers succeeded", runID, n, total)

		if runDBID == "" || !s.autonomous() {
			return // synchronous fallback: the user drives synthesis
		}
		failedMu.Lock()
		gaps := append([]taskSpec(nil), failed...)
		failedMu.Unlock()
		s.advanceRun(userID, book, page, runDBID, round, gaps)
	}()
}

// advanceRun is the §6.7 loop step run after a batch completes: retry
// failed sub-questions (a new round) while under the cap, otherwise
// hand off to synthesis. Runs in the supervisor goroutine, on a fresh
// context (the run context is spent). workers_done is owned by the
// per-worker IncrementWorkerDone bumps; this only resets it for a new
// round.
func (s *Skill) advanceRun(userID string, book *admin.Book, page *admin.WikiPage, runDBID string, round int, gaps []taskSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), statusAppendTimeout)
	defer cancel()

	// Cancelled (user hit stop) → no new round, no synthesis; re-assert
	// terminal in case a race let the status flap back to active.
	if s.isCancelled(runDBID) {
		s.finishCancelled(ctx, runDBID)
		return
	}

	if len(gaps) > 0 && round < s.opts.MaxRounds {
		next := round + 1
		st := admin.RunStatusResearching
		wt := len(gaps)
		zero := 0
		// Compare-and-set: if a cancel marked the run failed, this no-ops
		// (applied=false) and we bail instead of resurrecting the run.
		applied, err := s.opts.Runs.UpdateIfActive(ctx, runDBID, admin.RunPatch{
			Status: &st, Round: &next, WorkersTotal: &wt, WorkersDone: &zero,
		})
		if err != nil {
			log.Printf("[research] run db=%s: round-advance update failed: %v", runDBID, err)
		}
		if !applied {
			s.runCancels.Delete(runDBID)
			return
		}
		log.Printf("[research] run db=%s: gap-fill round %d for %d failed task(s)", runDBID, next, len(gaps))
		// dispatch re-checks the cancel flag at its start and bails +
		// re-asserts terminal if a stop landed during this transition.
		s.dispatch(userID, book, page, newRunID(), runDBID, next, gaps)
		return
	}

	// Terminal batch → synthesize. Workers are done, so drop the
	// canceller (synthesis isn't cut by the worker context).
	s.runCancels.Delete(runDBID)
	// Compare-and-set to synthesizing: a stop that marked the run failed
	// wins — applied=false means don't synthesize.
	st := admin.RunStatusSynthesizing
	applied, err := s.opts.Runs.UpdateIfActive(ctx, runDBID, admin.RunPatch{Status: &st})
	if err != nil {
		log.Printf("[research] run db=%s: synthesizing-status update failed: %v", runDBID, err)
	}
	if !applied {
		return
	}
	// Synthesis drives an owner pl.Handle turn (minutes) + delivery, so
	// give it its own timeout rather than the short append one.
	sctx, scancel := context.WithTimeout(s.rootCtx, runDeadline)
	defer scancel()
	s.opts.Synthesize(sctx, runDBID)
}

// taskPrompt is the worker's user message: the sub-question, optional
// hints, and the exact append target. The system prompt explains the
// method; this carries the specifics.
func taskPrompt(t taskSpec, bookSlug, pageSlug string) string {
	var b strings.Builder
	b.WriteString("Sub-question: ")
	b.WriteString(t.Question)
	b.WriteString("\n")
	if strings.TrimSpace(t.Hints) != "" {
		b.WriteString("Hints: ")
		b.WriteString(t.Hints)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nEvidence page: book_slug=%q page_slug=%q — append your findings there with append_to_page.", bookSlug, pageSlug)
	return b.String()
}

// reportWorkerProgress bumps the run's live counters as a worker
// finishes (§6.7) — one more area done, plus its token + page tally.
// No-op in the synchronous fallback (runDBID == ""). Best-effort on a
// fresh context; info is nil when the worker never ran (queue timeout).
func (s *Skill) reportWorkerProgress(runDBID string, info *pipeline.RouteInfo) {
	if runDBID == "" || !s.autonomous() {
		return
	}
	var tokens int64
	var pages int
	if info != nil {
		tokens = int64(info.InputTokens + info.OutputTokens)
		pages = info.PagesFetched
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusAppendTimeout)
	defer cancel()
	if err := s.opts.Runs.IncrementWorkerDone(ctx, runDBID, tokens, pages); err != nil {
		log.Printf("[research] run db=%s: progress bump failed: %v", runDBID, err)
	}
}

// setWorkerState transitions one area's state in the roster the card
// renders (§research UI). Keyed by the task's stable roster index so a
// gap-fill retry lands on the same entry. Best-effort telemetry — a
// failure only logs, never affects the run. No-op in the synchronous
// fallback (runDBID == "").
func (s *Skill) setWorkerState(runDBID string, idx int, state string) {
	if runDBID == "" || !s.autonomous() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusAppendTimeout)
	defer cancel()
	if err := s.opts.Runs.SetWorkerState(ctx, runDBID, idx, state); err != nil {
		log.Printf("[research] run db=%s: worker %d → %s failed: %v", runDBID, idx, state, err)
	}
}

// reportWorkerFailure appends the §6.4 failure line. Best-effort on a
// fresh context (the run context may already be done); an append
// failure only logs — the run must not fail because its status
// couldn't be recorded.
func (s *Skill) reportWorkerFailure(userID string, book *admin.Book, page *admin.WikiPage, n int, question string, err error) {
	line := fmt.Sprintf("\nworker %d (%s) failed: %v\n", n, truncateRunes(question, questionPreviewRunes), err)
	s.appendStatus(userID, book, page, line)
}

// appendStatus writes a status line onto the evidence page, detached
// from any worker/run context.
func (s *Skill) appendStatus(userID string, book *admin.Book, page *admin.WikiPage, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), statusAppendTimeout)
	defer cancel()
	if _, err := s.opts.Backend.AppendPage(ctx, book.ID, page.ID, userID, text); err != nil {
		log.Printf("[research] status append failed (book=%s page=%s): %v", book.ID, page.ID, err)
	}
}

// newRunID mints a short random run identifier (crypto/rand, per the
// codebase's no-math/rand convention). It labels the completion line,
// worker sessions, and log lines — not a secret, just unambiguous.
func newRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the system is badly broken, but the
		// ID is only a label — a clock-derived fallback keeps the run
		// alive rather than aborting a dispatch over it.
		return strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
	}
	return hex.EncodeToString(b[:])
}

// truncateRunes caps s at n runes, appending an ellipsis when cut.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
