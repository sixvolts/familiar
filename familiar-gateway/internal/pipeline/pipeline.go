// Package pipeline glues routing, context assembly, LLM dispatch,
// and fact commit into one request path.
//
// # Resilience hierarchy
//
// The pipeline degrades gracefully as optional subsystems fail. The
// ordering here is intentional: each downstream step assumes upstream
// failures have already been absorbed.
//
//  1. Sidecar router down → fall back to rule-based routing in router.Router.
//  2. Embedder down → pgvector and engine-side semantic search are skipped;
//     keyword-only retrieval proceeds.
//  3. Memory store down → assembleMessages logs and continues; the zone
//     stays empty and the LLM sees no retrieved memories for this turn.
//  4. Profile store down → working-context zone stays empty; the
//     system prompt still loads.
//  5. Skill execution fails → the failure text is returned to the LLM
//     as the tool result so the model can handle it gracefully.
//  6. Session store down → rolling summary stays in memory only.
//  7. Engine down → hard failure; the gateway cannot start without it.
//
// The pattern throughout: log the degradation once at the failure
// site, never abort the turn for an optional component, and never
// block the user on a slow optional path (bounded timeouts via
// context.WithTimeout on every optional call).
package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/ctxbuild"
	"github.com/familiar/gateway/internal/engine"
	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/llm"
	"github.com/familiar/gateway/internal/memevents"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/prefetch"
	"github.com/familiar/gateway/internal/rerank"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/familiar/gateway/internal/skills"
	"github.com/familiar/gateway/internal/userprofile"
	pb "github.com/familiar/gateway/proto/engine"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EmbedFunc computes a dense vector for a text string.
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

// MemoryVersioner records a snapshot in the memory_versions audit
// table. The pipeline calls this when it supersedes a memory so the
// version history reflects extraction-driven changes.
type MemoryVersioner interface {
	RecordVersion(ctx context.Context, memoryID, content, scope, sourceType, changedBy, changeType string) error
}

// RouteInfo carries metadata about how a message was handled.
//
// RetrievedRelationships captures the rels that were available to
// the assistant when it composed its response — populated in
// assembleMessages and consumed by the post-turn extract pipeline
// per CHAT-REARCH §"Memory Write Pipeline" inputs. Empty for shard
// invocations and the trusted-path zero-memory branch.
type RouteInfo struct {
	ModelID                string
	MemHits                int
	Tier                   ctxbuild.PromptTier
	RetrievedRelationships []memory.Relationship

	// InputTokens / OutputTokens are accumulated across tool-loop
	// iterations so the frontend's tok/s calculation reflects total
	// work, not just the final iteration.
	InputTokens  int
	OutputTokens int
	DecodeMs     float64 // pure LLM generation time, summed across iterations

	// PagesFetched counts fetch_page tool calls this turn — surfaced so
	// the research progress card can show "N pages read" (§6.7).
	PagesFetched int

	// ResearchNote records the personal-book research note written this
	// turn (create_page/update_page with a "Research:" title), so the
	// streaming "done" event can tell the workspace to auto-open it and
	// link it in chat. Zero-value (empty PageSlug) means "none this
	// turn". Value type so &info.ResearchNote mirrors &info.PagesFetched.
	ResearchNote ResearchNoteRef

	// ReasoningContent holds post-hoc extracted reasoning (from
	// formatters like cohere2 that split untagged chain-of-thought).
	// Sent in the done event so the UI can populate the thinking bubble.
	ReasoningContent string

	// Stopped is set when the user pressed Stop and the turn's generation
	// was cut server-side mid-stream (StopTurn). The committed answer is
	// the partial produced up to that point; the done event reports
	// finish="stopped" so the UI can label it.
	Stopped bool
}

// ResearchNoteRef locates a research note the turn wrote to the user's
// personal book. Surfaced in the streaming "done" payload so the
// workspace can auto-open the note in a side pane and add a clickable
// link in chat (the inline quick/standard research path; the deep path
// has its own poll-driven note swap).
type ResearchNoteRef struct {
	BookSlug string
	PageSlug string
	Title    string
}

// researchNoteFrom decides whether a successful wiki write-tool result
// is a research note. The wiki create_page/update_page tools stash the
// page location in ToolResult.Data; we treat it as a research note when
// it lands in the user's personal book with the "Research:" title
// convention (the same convention makeResearchSynthesizer uses). Parses
// defensively — a non-write tool, or malformed/absent Data, is a clean
// (false), never an error that could abort the turn.
func researchNoteFrom(toolName string, data json.RawMessage) (ResearchNoteRef, bool) {
	// create_page/update_page are the generic wiki write tools (the
	// no-writer-model fallback); compose_research_note is the research
	// skill's own writer path — both stash the note location in Data.
	if toolName != "create_page" && toolName != "update_page" && toolName != "compose_research_note" {
		return ResearchNoteRef{}, false
	}
	if len(data) == 0 {
		return ResearchNoteRef{}, false
	}
	var loc struct {
		BookSlug string `json:"book_slug"`
		PageSlug string `json:"page_slug"`
		Title    string `json:"title"`
	}
	if json.Unmarshal(data, &loc) != nil {
		return ResearchNoteRef{}, false
	}
	if !strings.HasPrefix(loc.BookSlug, "personal:") ||
		!strings.HasPrefix(strings.TrimSpace(loc.Title), "Research:") ||
		loc.PageSlug == "" {
		return ResearchNoteRef{}, false
	}
	return ResearchNoteRef{BookSlug: loc.BookSlug, PageSlug: loc.PageSlug, Title: loc.Title}, true
}

// Pipeline wires together the engine, router, and session manager.
type Pipeline struct {
	engine            engine.Service
	router            *router.Router
	sessions          *session.Manager
	embedder          EmbedFunc
	memStore          memory.MemoryStore
	relStore          memory.RelationshipStore
	entityVocab       *memory.EntityVocab
	versioner         MemoryVersioner
	memoryCfg         config.MemoryConfig
	rerankCfg         config.RerankConfig
	reranker          *rerank.Client
	pipelineCfg       config.PipelineConfig
	ctxCfg            ctxbuild.Config
	profiles          *userprofile.Store
	agentID           string
	apiKeyFn          func(string) string
	systemPrompt      string
	promptStore       *ctxbuild.PromptStore
	sidecarEndpoint   string
	sidecarClient     *sidecar.Client
	toolOrchestrator  *prefetch.Orchestrator
	skillRegistry     *skills.Registry
	maxToolIters      int
	shardAugment      func(ctx context.Context, ov *ShardOverrides) error
	shardOnlyTools    map[string]bool
	userSkillsAugment func(ctx context.Context, userID string) string
	sessionStore      *session.Store
	conversations     ConversationStore
	identityResolver  *identity.Resolver
	// effort resolves classifier ordinal levels into concrete
	// budgets (token caps, top-k, max-searches). Built from
	// cfg.Effort by classifier.ResolverFromConfig at startup.
	// CHAT-REARCH S2.3b.
	effort *classifier.EffortResolver
	// events fans typed memory-write events out to logs and the SSE
	// endpoint. Nil-safe — emission no-ops when unwired.
	// CHAT-REARCH S5.
	events *memevents.Bus
	// embedderStats tracks per-call embedder success/failure so a
	// silently degraded embedder is observable in the logs.
	// CHAT-REARCH §"Smaller Hardening".
	embedderStats callStats
	// maintenance, when active, reroutes the trusted chat path to a
	// fallback model (the big model is down / drained). Nil-safe.
	maintenance maintenanceSwitch
	// lifetime is the gateway's root (shutdown) context, set once at
	// startup via SetLifetime. A turn's generation is detached from the
	// per-request context so a client disconnect doesn't truncate it,
	// but it must still yield to gateway shutdown — that's what
	// lifetime provides. Nil falls back to a cap-only bound.
	lifetime context.Context
	// stopReg maps a live turn's session ID to the cancel handle that
	// cuts its generation. turnContext registers an entry for the
	// duration of a turn; StopTurn consults it to end an in-flight turn
	// server-side when the user presses Stop. This is distinct from a
	// client disconnect, which detached turns deliberately ignore.
	stopReg sync.Map // sessID string -> *turnStopper
}

// turnStopper is the registry value for one in-flight turn. The pointer
// identity lets teardown remove exactly this turn's entry (via
// CompareAndDelete) without racing a newer turn that reused the same
// session ID. cancel carries a cause so the LLM stream can tell a
// user-Stop (salvage the partial) apart from a hard-cap/shutdown.
type turnStopper struct {
	cancel context.CancelCauseFunc
}

// errUserStopped is the cancellation cause set by StopTurn. The
// streaming providers treat a context cancelled with this cause (or any
// cause, once tokens exist) as "return the partial", so the turn commits
// what the user already saw instead of discarding it.
var errUserStopped = errors.New("turn stopped by user")

// StopTurn cuts the in-flight turn for sessID (workspace passes the
// conversation_id, which is the session key). It cancels the turn's
// detached generation context so the model stops decoding immediately
// and the partial produced so far is committed — keeping the persisted
// history in sync with what the user saw. Returns true when a live turn
// was found and signalled, false when there was nothing to stop.
func (p *Pipeline) StopTurn(sessID string) bool {
	if sessID == "" {
		return false
	}
	v, ok := p.stopReg.Load(sessID)
	if !ok {
		return false
	}
	st, ok := v.(*turnStopper)
	if !ok || st == nil {
		return false
	}
	st.cancel(errUserStopped)
	return true
}

// turnHardCap bounds one turn's total generation + tool work once it's
// detached from the request context. Matches the per-LLM-call ceiling;
// a turn that blows it is a stuck model, not a slow one.
const turnHardCap = 600 * time.Second

// SetLifetime wires the gateway's root (shutdown) context. Call once at
// startup, before serving. It's the cancellation source for detached
// turns: they ignore client disconnect but still stop on shutdown.
func (p *Pipeline) SetLifetime(ctx context.Context) { p.lifetime = ctx }

// turnContext returns the context used to PRODUCE one turn's output
// (preamble, tools, LLM generation, commit). It carries the request
// context's values — auth, resolved identity, session scope — but is
// deliberately NOT cancelled when the request context is (i.e. when the
// SSE client disconnects). Instead it's bounded by turnHardCap and
// cancelled by gateway shutdown (p.lifetime). This is what lets an
// abandoned stream finish generating and persist the whole turn rather
// than being cut off mid-token. The caller MUST defer the returned
// cancel to release the timer + AfterFunc registration.
func (p *Pipeline) turnContext(reqCtx context.Context, sessID string) (context.Context, context.CancelFunc) {
	// WithCancelCause (not WithTimeout) so a user Stop can cancel with a
	// distinguishable cause; the hard cap and shutdown are layered on as
	// AfterFunc callbacks that cancel with their own causes.
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(reqCtx))
	timer := time.AfterFunc(turnHardCap, func() { cancel(context.DeadlineExceeded) })
	var stopShutdown func() bool
	if p.lifetime != nil {
		stopShutdown = context.AfterFunc(p.lifetime, func() { cancel(context.Canceled) })
	}
	// Register this turn so StopTurn can cut its generation. Keyed by
	// session id (== workspace conversation_id). Pointer identity guards
	// teardown against a newer turn that reused the same session id.
	var st *turnStopper
	if sessID != "" {
		st = &turnStopper{cancel: cancel}
		p.stopReg.Store(sessID, st)
	}
	return ctx, func() {
		if st != nil {
			p.stopReg.CompareAndDelete(sessID, st)
		}
		if stopShutdown != nil {
			stopShutdown()
		}
		timer.Stop()
		cancel(context.Canceled)
	}
}

// maintenanceSwitch is the slice of the maintenance controller the
// pipeline needs: "are we in maintenance, and if so which model?".
// An interface keeps the pipeline decoupled from the controller's
// package (and trivially mockable in tests).
type maintenanceSwitch interface {
	Active() (bool, string)
}

// resolveIdentity maps the session's platform-specific SenderID to a
// canonical user ID via the resolver, caching it on the session so
// subsequent turns (and downstream lookups within this turn) don't
// re-resolve. A no-op when no resolver is wired or Platform/SenderID
// are missing.
//
// OWNER-MIGRATION: an unmapped platform identity used to silently
// resolve to "owner". The resolver now returns ok=false in that case
// and we leave CanonicalID empty so downstream stages (memory scoping,
// profile lookup, fact attribution) see a missing identity and refuse
// to operate, rather than impersonating the legacy "owner" user.
func (p *Pipeline) resolveIdentity(sess *session.Session) {
	if p.identityResolver == nil || sess == nil {
		return
	}
	if sess.CanonicalID() != "" {
		return
	}
	platform := sess.Platform()
	if platform == "" || sess.SenderID == "" {
		return
	}
	if canonical, ok := p.identityResolver.Resolve(platform, sess.SenderID); ok {
		sess.SetCanonicalID(canonical)
	}
}

// hydrateSession loads the persisted running summary AND the recent
// verbatim turns for sess on the first call per process. Subsequent
// calls are no-ops so retrieval stays on the hot path without extra
// round-trips.
//
// Turns are loaded from the conversation store using sess.ID as the
// conversation UUID — the workspace adapter passes conversation_id
// as the session id (see SESSION-HYDRATION.md). Adapters whose
// session ids aren't UUIDs get a harmless no-op from the store.
func (p *Pipeline) hydrateSession(ctx context.Context, sess *session.Session) {
	// These two skip paths run on every turn after the first (or on
	// every turn of a store-less deploy), so they stay silent — the
	// once-per-session "hydrating"/"hydrated" logs below carry the
	// signal without scaling log volume with traffic.
	if sess == nil || sess.IsHydrated() {
		return
	}
	if p.sessionStore == nil && p.conversations == nil {
		return
	}
	log.Printf("[pipeline] hydrating session %s (turns=%d, conversations=%v)", sess.ID, sess.TurnCount(), p.conversations != nil)
	sess.MarkHydrated() // mark first to avoid thundering herd on errors
	loadCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var summary string
	var count int
	if p.sessionStore != nil {
		var err error
		summary, count, err = p.sessionStore.Load(loadCtx, sess.SummaryKey())
		if err != nil {
			log.Printf("[pipeline] session store load %s: %v", sess.ID, err)
		} else if summary != "" || count > 0 {
			sess.SetSummary(summary, count)
		}
	}

	// Verbatim turns. Only hydrate when the in-memory session is
	// empty — a session that already has turns is a live, in-process
	// session (not post-restart) and refilling it would duplicate
	// history. Capped at MaxSessionTurns to match what AddTurn would
	// retain anyway.
	turnsLoaded := 0
	if p.conversations != nil && sess.TurnCount() == 0 {
		err := p.conversations.LoadRecentTurns(loadCtx, sess.ID, session.MaxSessionTurns, func(role, content string, toolCalls []byte, toolCallID string) {
			sess.AddMessage(session.Turn{
				Role:       role,
				Content:    content,
				ToolCalls:  toolCalls,
				ToolCallID: toolCallID,
			})
			turnsLoaded++
		})
		if err != nil {
			log.Printf("[pipeline] conversation hydrate %s: %v", sess.ID, err)
		}
	}

	if summary == "" && count == 0 && turnsLoaded == 0 {
		return
	}
	log.Printf("[pipeline] hydrated session %s (summary=%d chars, dropped=%d, turns=%d)",
		sess.ID, len(summary), count, turnsLoaded)
}

// ConversationStore is the persistent-conversation surface the
// pipeline uses on two paths:
//
//   - Read (LoadRecentTurns): rehydrate verbatim session turns —
//     including the tool_calls + tool_call_id shape — after a
//     gateway restart.
//   - Write (AppendIntermediateMessages): persist the agentic
//     loop's mid-turn rows (assistant w/ tool_calls, tool
//     results) so the messages table mirrors what the LLM sees
//     and hydration can replay it later.
//
// *admin.ConversationStore satisfies both methods; tests can drop
// in a no-op. See SESSION-HYDRATION.md.
//
// Implementations must call visit once per kept message in
// chronological order (oldest first). A non-UUID conversationID
// (or any other lookup miss) should resolve to "no messages, no
// error" on read and a silent no-op on write — the pipeline asks
// speculatively on every session regardless of adapter, and not
// every adapter's session id is a workspace conversation UUID.
type ConversationStore interface {
	LoadRecentTurns(ctx context.Context, conversationID string, limit int, visit func(role, content string, toolCalls []byte, toolCallID string)) error
	AppendIntermediateMessages(ctx context.Context, conversationID string, msgs []IntermediateMessage) error
}

// IntermediateMessage mirrors admin.IntermediateMessage in the
// shape the pipeline produces — kept in this package so callers
// (and tests) don't have to import internal/admin.
type IntermediateMessage struct {
	Role       string // "assistant" | "tool"
	Content    string
	ToolCalls  []byte // JSON-encoded llm.ToolCall slice
	ToolCallID string
}

// Deps bundles every dependency the Pipeline needs at construction.
// Required fields: Engine, Router, Sessions, AgentID. Optional fields
// may be left zero; a nil field disables the corresponding feature
// (e.g. no MemoryStore → no pgvector retrieval, no ProfileStore → no
// working-context zone). MaxToolIters defaults to 5 when zero.
type Deps struct {
	Engine       engine.Service
	Router       *router.Router
	Sessions     *session.Manager
	AgentID      string
	SystemPrompt string

	// ShardAugment, when set, runs against every shard envelope just
	// before the turn begins (SKILL-PACKAGES-SPEC Phase 2): it
	// appends the bound-skills prompt block and extends the
	// allowlist with the skillpacks tools. Failures degrade (logged,
	// turn continues without skills) — the pipeline-resilience rule.
	ShardAugment func(ctx context.Context, ov *ShardOverrides) error

	// ShardOnlyTools never appear in trusted-path tool
	// advertisement and are refused at trusted-path dispatch even if
	// the model hallucinates the name. Imported-skill access tools
	// live here. A turn where UserSkillsAugment grants skills is the
	// one exception — the grant unlocks these tools for that turn.
	ShardOnlyTools []string

	// UserSkillsAugment, when set, returns the "## Skills" prompt
	// block for the user's chat-enabled personal skills, or "" when
	// they have none (USER-SKILLS-SPEC Phase B). A non-empty block is
	// appended to the trusted-path system prompt AND unlocks the
	// ShardOnlyTools (use_skill / read_skill_file) for that turn.
	// Failures inside the closure must degrade to "" — the turn
	// proceeds without personal skills.
	UserSkillsAugment func(ctx context.Context, userID string) string

	Embedder          EmbedFunc
	APIKeyFn          func(string) string
	MemoryStore       memory.MemoryStore
	RelationshipStore memory.RelationshipStore
	EntityVocab       *memory.EntityVocab
	Versioner         MemoryVersioner
	MemoryConfig      config.MemoryConfig
	RerankConfig      config.RerankConfig
	// Reranker is the cross-encoder client used to trim the hybrid-
	// search candidate pool to the top few memories. Optional — nil
	// (or a disabled RerankConfig) falls back to hybrid-search top-k.
	Reranker         *rerank.Client
	PipelineConfig   config.PipelineConfig
	ContextConfig    ctxbuild.Config
	ProfileStore     *userprofile.Store
	PromptStore      *ctxbuild.PromptStore
	SidecarEndpoint  string
	SidecarClient    *sidecar.Client
	ToolOrchestrator *prefetch.Orchestrator
	SkillRegistry    *skills.Registry
	MaxToolIters     int
	SessionStore     *session.Store
	// Conversations optionally hydrates verbatim turns when a
	// session is created cold (e.g. after a gateway restart). Nil
	// disables turn hydration; the session starts empty.
	Conversations    ConversationStore
	IdentityResolver *identity.Resolver
	// EffortResolver maps classifier ordinal levels to concrete
	// budgets. Optional — nil falls back to classifier
	// .DefaultResolver() (spec defaults).
	EffortResolver *classifier.EffortResolver
	// Events is the typed event bus the pipeline publishes
	// memory-write activity onto. Optional — nil emission is a
	// no-op. Wire one in main.go and mount the SSE handler on the
	// adapter to deliver the same stream to a browser.
	Events *memevents.Bus

	// Maintenance, when set and active, reroutes the trusted chat
	// path to a fallback model. Optional — nil disables the feature.
	Maintenance maintenanceSwitch
}

// New constructs a Pipeline from its dependency bundle. All wiring
// happens here; there are no post-construction setters.
func New(d Deps) *Pipeline {
	maxIters := d.MaxToolIters
	if maxIters <= 0 {
		maxIters = 10
	}
	effort := d.EffortResolver
	if effort == nil {
		effort = classifier.DefaultResolver()
	}
	p := &Pipeline{
		engine:            d.Engine,
		router:            d.Router,
		sessions:          d.Sessions,
		embedder:          d.Embedder,
		memStore:          d.MemoryStore,
		relStore:          d.RelationshipStore,
		entityVocab:       d.EntityVocab,
		versioner:         d.Versioner,
		memoryCfg:         d.MemoryConfig,
		rerankCfg:         d.RerankConfig,
		reranker:          d.Reranker,
		pipelineCfg:       d.PipelineConfig,
		ctxCfg:            d.ContextConfig,
		profiles:          d.ProfileStore,
		agentID:           d.AgentID,
		apiKeyFn:          d.APIKeyFn,
		systemPrompt:      d.SystemPrompt,
		promptStore:       d.PromptStore,
		sidecarEndpoint:   d.SidecarEndpoint,
		sidecarClient:     d.SidecarClient,
		toolOrchestrator:  d.ToolOrchestrator,
		skillRegistry:     d.SkillRegistry,
		maxToolIters:      maxIters,
		shardAugment:      d.ShardAugment,
		shardOnlyTools:    toolAllowlistSet(d.ShardOnlyTools),
		userSkillsAugment: d.UserSkillsAugment,
		sessionStore:      d.SessionStore,
		conversations:     d.Conversations,
		identityResolver:  d.IdentityResolver,
		effort:            effort,
		events:            d.Events,
		maintenance:       d.Maintenance,
	}
	p.embedderStats.label = "embedder"
	return p
}

// embedText produces an embedding for text, or nil on failure /
// missing embedder. CHAT-REARCH §"Smaller Hardening" — failures
// here are tracked via embedderStats so a degraded embedder shows
// up in the logs as a rolling failure-rate summary, not just one
// log line per call. Downstream callers (assembleMessages,
// runPostTurnExtract) treat nil as "no vector available" and skip
// pgvector retrieval gracefully — the pipeline never 500s on an
// embedder outage; the worst case is reduced memory recall for the
// duration of the outage.
func (p *Pipeline) embedText(ctx context.Context, text string) []float32 {
	if p.embedder == nil || text == "" {
		return nil
	}
	embedCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	vec, err := p.embedder(embedCtx, text)
	if err != nil {
		log.Printf("[pipeline] embed error: %v", err)
		p.embedderStats.recordAttempt(true)
		return nil
	}
	p.embedderStats.recordAttempt(false)
	return vec
}

// GenerateTitle asks the sidecar for a short title for a new chat
// from its opening exchange. Best-effort: returns an error when no
// sidecar is wired or the classify task is unavailable, and the
// caller keeps whatever title it already had.
func (p *Pipeline) GenerateTitle(ctx context.Context, userMsg, assistantMsg string) (string, error) {
	if p.sidecarClient == nil {
		return "", fmt.Errorf("pipeline: no sidecar configured for title generation")
	}
	return p.sidecarClient.GenerateTitle(ctx, userMsg, assistantMsg)
}

// sidecarModelLabel is the "model" field on direct sidecar chat calls
// (the preamble generator). llama-server ignores it — the endpoint
// selects the loaded model — so it's a label, not a selector. Kept
// generic instead of pinning a specific model name that goes stale
// on a swap. See EXTERNAL-READINESS-REVIEW.md for the broader note on
// threading real per-endpoint model ids through the sidecar client.
const sidecarModelLabel = "sidecar"

// generatePreamble calls the sidecar to produce a brief acknowledgment
// before handing off to the heavy model. It streams chunks via onChunk
// AND returns the full streamed text (preamble + the "---" separator)
// so the caller can fold it into the committed assistant turn — the
// user saw it inline, so the stored conversation must include it or a
// reload would show a different message than the live view. Returns ""
// when no sidecar is wired or the call fails.
func (p *Pipeline) generatePreamble(ctx context.Context, userMsg string, complexity string, onChunk func(string)) string {
	if p.sidecarEndpoint == "" {
		return ""
	}

	preamblePrompt := "You are a helpful assistant about to work on a user request. " +
		"Generate a brief 1-2 sentence acknowledgment that: " +
		"(1) confirms you understand what they are asking, " +
		"(2) briefly describes what you will provide, " +
		"(3) sets expectations naturally. " +
		"Keep it casual and direct. No filler. Do NOT answer the question. " +
		"Only the brief preamble.\n\nUser request: " + userMsg +
		"\nComplexity: " + complexity + "\n\nYour brief acknowledgment:"

	type sidecarMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type sidecarReq struct {
		Model              string         `json:"model"`
		Messages           []sidecarMsg   `json:"messages"`
		MaxTokens          int            `json:"max_tokens,omitempty"`
		Stream             bool           `json:"stream"`
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	}

	body := sidecarReq{
		// The model name is a label only: llama-server serves whatever
		// model is loaded at the endpoint, so the field is ignored. We
		// borrow the sidecar (classify-slot) endpoint, which the
		// operator points at their fast preamble model. (A multi-model
		// backend like vLLM that routes by name would need a real
		// per-endpoint model id threaded through the sidecar client —
		// tracked separately; see EXTERNAL-READINESS-REVIEW.md.)
		Model:              sidecarModelLabel,
		Messages:           []sidecarMsg{{Role: "user", Content: preamblePrompt}},
		MaxTokens:          150,
		Stream:             true,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		log.Printf("[pipeline] preamble marshal error: %v", err)
		return ""
	}

	preambleCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(preambleCtx, http.MethodPost,
		p.sidecarEndpoint+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		log.Printf("[pipeline] preamble request error: %v", err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[pipeline] preamble call error: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[pipeline] preamble HTTP %d", resp.StatusCode)
		return ""
	}

	// Parse SSE stream from sidecar, accumulating the text we stream so
	// the caller can commit it as part of the assistant turn.
	var preamble strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			preamble.WriteString(chunk.Choices[0].Delta.Content)
			onChunk(chunk.Choices[0].Delta.Content)
		}
	}

	// Nothing came back (model returned empty) → no separator, no
	// committed lead-in. Return "" so the caller commits only the
	// main response.
	if preamble.Len() == 0 {
		return ""
	}

	// Separator between preamble and main response.
	const separator = "\n\n---\n\n"
	onChunk(separator)
	preamble.WriteString(separator)
	log.Printf("[pipeline] preamble delivered for %s complexity request", complexity)
	return preamble.String()
}

// assembleMessages runs memory retrieval, packs everything through the
// ctxbuild Builder under a per-model token budget, and returns the
// provider-ready message slice. It is shared by Handle and HandleStream.
//
// toolResultCtx is the pre-formatted blob the tool orchestrator produces, or
// empty if no tools ran. onStatus may be nil for non-streaming callers.
// info.MemHits is populated with the number of memories that survived the
// budget (not the number retrieved).
func (p *Pipeline) assembleMessages(
	ctx context.Context,
	sess *session.Session,
	userMsg string,
	modelID string,
	complexity string,
	memDepth classifier.MemoryDepth,
	toolResultCtx string,
	info *RouteInfo,
	onStatus func(string),
) []llm.Message {
	// Resolve the prompt tier up front so memory retrieval can use the
	// tier-specific threshold / max_results / expansion policy. The tier
	// lookup is pure and cheap.
	tier := ctxbuild.TierFor(complexity)
	info.Tier = tier

	memBudget := p.effort.MemoryFor(memDepth)

	var memoryContext []*pb.MemoryResultProto
	var convHistory []*pb.ConversationTurn
	var queryVec []float32

	// retrievalQuery is what we search memory with — userMsg condensed
	// against recent dialogue. Defaults to the raw message; rewritten
	// below when a sidecar is available.
	retrievalQuery := userMsg

	if !memBudget.Skip {
		if onStatus != nil {
			onStatus("Searching memories...\n")
		}
		// Query condensation: a follow-up turn like "what about the
		// timeout?" embeds terribly on its own. Rewrite it into a
		// self-contained query against recent dialogue before
		// embedding. Best-effort — any sidecar failure falls back to
		// the raw message. The condensed query is used ONLY for
		// retrieval; userMsg still drives generation.
		if p.sidecarClient != nil {
			recent := sess.RecentTurns(6)
			if len(recent) > 0 {
				history := make([]sidecar.Turn, 0, len(recent))
				for _, t := range recent {
					history = append(history, sidecar.Turn{Role: t.Role, Content: t.Content})
				}
				condCtx, condCancel := context.WithTimeout(ctx, 3*time.Second)
				condensed, condErr := p.sidecarClient.CondenseQuery(condCtx, history, userMsg)
				condCancel()
				if condErr != nil {
					log.Printf("[pipeline] query condensation failed (using raw message): %v", condErr)
				} else if condensed != "" && condensed != userMsg {
					log.Printf("[pipeline] query condensed: %q -> %q", userMsg, condensed)
					retrievalQuery = condensed
				}
			}
		}
		queryVec = p.embedText(ctx, retrievalQuery)
		assembleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		vis := &pb.VisibilityContext{
			ChannelId: sess.ChannelID,
			AgentId:   p.agentID,
			// UserID() (canonical, falling back to SenderID) so the read
			// axis matches the write axis used in commitAndExtract.
			UserId: sess.UserID(),
		}
		// Tier-aware budgets — earlier we hardcoded 2000 / 4000
		// which clipped Qwen-122B's 262K window down to ~3K words
		// of history regardless of the request's complexity. Pull
		// the budgets from tier so deep_reasoning gets 64K of
		// history while trivial keeps a tight 2K. Zero values fall
		// back to the legacy literals.
		memBudget := uint32(2000)
		if tier.MemBudget > 0 {
			memBudget = uint32(tier.MemBudget)
		}
		convBudget := uint32(4000)
		if tier.ConvBudget > 0 {
			convBudget = uint32(tier.ConvBudget)
		}
		ctxResp, err := p.engine.AssembleContext(assembleCtx, sess.ID, retrievalQuery, vis, memBudget, convBudget, queryVec)
		if err != nil {
			log.Printf("[pipeline] AssembleContext error (continuing): %v", err)
		} else {
			if ctxResp.Error != "" {
				log.Printf("[pipeline] AssembleContext engine error: %s", ctxResp.Error)
			}
			memoryContext = ctxResp.MemoryContext
			convHistory = ctxResp.ConversationHistory
			if onStatus != nil {
				onStatus(fmt.Sprintf("Memory: %d hits | History: %d turns\n", len(memoryContext), len(convHistory)))
			}
		}
	} else {
		log.Printf("[pipeline] trivial complexity (no memory requested) — skipping embedder/pgvector/engine context")
	}

	// Engine hot memories → ctxbuild.Memory, preserving the existing
	// "- content (staleness)" format so the LLM sees the same thing it did
	// before the refactor.
	var mems []ctxbuild.Memory
	for _, m := range memoryContext {
		if m.Fact == nil {
			continue
		}
		staleness := m.Staleness
		if staleness == "" {
			staleness = "unknown"
		}
		mems = append(mems, ctxbuild.Memory{
			Content: fmt.Sprintf("- %s (%s)", m.Fact.Content, staleness),
		})
	}

	// pgvector persistent tier, with optional tier-driven query expansion.
	// Tier overrides on threshold/max_results fall through zero values to
	// the global memoryCfg defaults.
	if p.memStore != nil && queryVec != nil {
		// Effort-resolved MemoryBudget wins; fall through to tier-driven and
		// then global config defaults. The classifier is the new authority,
		// but tier-based deployments without effort overrides keep working.
		limit := p.memoryCfg.MaxInjected
		if tier.MemoryConfig.MaxResults > 0 {
			limit = tier.MemoryConfig.MaxResults
		}
		if memBudget.TopK > 0 {
			limit = memBudget.TopK
		}
		threshold := p.memoryCfg.RelevanceThreshold
		if tier.MemoryConfig.Threshold > 0 {
			threshold = tier.MemoryConfig.Threshold
		}
		if memBudget.SimilarityThreshold > 0 {
			threshold = memBudget.SimilarityThreshold
		}

		pgResults := p.searchPgVector(ctx, sess.UserID(), retrievalQuery, queryVec, tier, limit, threshold, onStatus)
		for _, r := range pgResults {
			mems = append(mems, ctxbuild.Memory{
				Content:    fmt.Sprintf("- %s (similarity: %.2f)", r.Content, r.Similarity),
				Scope:      r.Scope,
				Similarity: r.Similarity,
			})
		}
		if len(pgResults) > 0 {
			log.Printf("[pipeline] pgvector returned %d memories (tier=%s limit=%d thr=%.2f)",
				len(pgResults), tier.Name, limit, threshold)
		}

		// Promote-on-access was the RAM-tier mirror bridge that used to
		// run here — it copied high-scoring pgvector hits into the
		// engine's hot cache so the next retrieval would hit RAM
		// instead of Postgres. The engine migration deleted the RAM
		// tier and switched to synchronous pgvector writes, so the
		// bridge collapsed to "INSERT the same row back into the
		// table". Removed entirely.
	}

	// Prefer engine-provided conversation history, fall back to session.
	// On the fallback path we hand ctxbuild the full session buffer
	// (capped by MaxSessionTurns) and let it evict by token budget —
	// previously we capped at 10 here, which silently truncated to a
	// fraction of the model's actual context window. ctxbuild's
	// conversation zone is the single authoritative truncation point;
	// trivial complexity still gets a tiny window (2 turns) since the
	// router skipped engine + memory entirely for that tier.
	var turns []session.Turn
	if len(convHistory) > 0 {
		turns = make([]session.Turn, 0, len(convHistory))
		for _, t := range convHistory {
			turns = append(turns, session.Turn{Role: t.Role, Content: t.Content})
		}
	} else if complexity == "trivial" {
		turns = sess.RecentTurns(2)
	} else {
		turns = sess.RecentTurns(0) // 0 = all (capped by MaxSessionTurns)
	}

	summary, _ := sess.Snapshot()

	// Layer 1 user prompt: the per-user "assistant personality"
	// prompt, authored by the user in their profile. Keyed by the
	// canonical user identity (falls back to SenderID when no
	// identity resolver is configured). A missing row is not an
	// error — the prompt simply stays empty.
	var userPrompt string
	if p.profiles != nil && sess.UserID() != "" {
		profCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		up, perr := p.profiles.Get(profCtx, sess.UserID())
		cancel()
		if perr != nil {
			log.Printf("[pipeline] user prompt load error (continuing): %v", perr)
		} else {
			userPrompt = up
		}
	}

	var toolResults []ctxbuild.ToolResult
	if toolResultCtx != "" {
		toolResults = []ctxbuild.ToolResult{{Name: "tools", Content: toolResultCtx}}
	}

	// Graph augmentation has two layers stacked on top of each other.
	// One-hop: for every retrieved memory, pull any relationship
	// whose subject appears in the memory content (existing behaviour).
	// Multi-hop: for every entity name from the vocab that shows up
	// in the retrieved memories, run a depth-2 CTE traversal outward
	// so the LLM sees "operator → owns → gpu-host → has_gpu → gpu-x" in
	// a single context pass instead of only the edges that textually
	// match a memory. Both layers dedupe into one relLines slice.
	var relLines []string
	if p.relStore != nil && len(mems) > 0 {
		contents := make([]string, 0, len(mems))
		for _, m := range mems {
			contents = append(contents, m.Content)
		}

		relCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		rels, rerr := p.relStore.RelatedForContents(relCtx, contents, sess.UserID(), 12)
		cancel()
		if rerr != nil {
			log.Printf("[pipeline] relationship lookup error (continuing): %v", rerr)
		}

		// Multi-hop traversal. The vocab cache is non-blocking — if
		// it has not loaded yet we silently skip this layer instead
		// of racing a database call on every request.
		if p.entityVocab != nil {
			haystack := strings.Join(contents, "\n")
			entities := p.entityVocab.FindIn(haystack)
			// Cap the number of seed entities so a meme-heavy memory
			// doesn't kick off twenty recursive CTEs in a row.
			if len(entities) > 4 {
				entities = entities[:4]
			}
			for _, ent := range entities {
				travCtx, tcancel := context.WithTimeout(ctx, 2*time.Second)
				deeper, terr := p.relStore.TraverseFrom(travCtx, ent, sess.UserID(), 2, 8)
				tcancel()
				if terr != nil {
					log.Printf("[pipeline] traverse %q error (continuing): %v", ent, terr)
					continue
				}
				rels = append(rels, deeper...)
			}
		}

		rels = dedupeRelationships(rels)
		if len(rels) > 20 {
			rels = rels[:20]
		}
		if len(rels) > 0 {
			relLines = memory.FormatLines(rels)
			log.Printf("[pipeline] attached %d relationship triples to context", len(rels))
		}
		// Surface the retrieved rels for the post-turn extract pipeline
		// (CHAT-REARCH §"Memory Write Pipeline" — raw retrieved
		// relationships are one of the batched-call inputs).
		info.RetrievedRelationships = rels
	}

	// Scale the builder's window to the target model so a small-context
	// llama and a 200K Sonnet don't share the same budget.
	effCfg := p.ctxCfg
	if effCfg.WindowSize == 0 {
		effCfg = ctxbuild.DefaultConfig()
	}
	if modelCfg := p.router.GetRegistry().GetModelConfig(modelID); modelCfg != nil && modelCfg.ContextWindow > 0 {
		effCfg.WindowSize = modelCfg.ContextWindow
	}

	sysPrompt := p.systemPrompt
	if p.promptStore != nil && p.promptStore.Loaded() {
		// CHAT-REARCH §"Smaller Hardening" — re-stat prompt files at
		// most once per cooldown and reload any whose mtime advanced.
		// Cheap on the hot path; lets operators edit prompts without
		// a gateway restart.
		p.promptStore.MaybeReload()
		sysPrompt = p.promptStore.Assemble(tier)
	}

	assembled := ctxbuild.New(effCfg).Build(ctxbuild.Input{
		SystemPrompt:      sysPrompt,
		UserPrompt:        userPrompt,
		Summary:           summary,
		Turns:             turns,
		Memories:          mems,
		RelationshipLines: relLines,
		ToolResults:       toolResults,
		ReservedTokens:    ctxbuild.EstimateTokens(userMsg),
	})

	info.MemHits = len(assembled.Memories)
	log.Printf("[pipeline] ctxbuild: sys=%d mem=%d tools=%d conv=%d total=%d/%d headroom=%d evicted=%d",
		assembled.TokenUsage.System,
		assembled.TokenUsage.Memories,
		assembled.TokenUsage.Tools,
		assembled.TokenUsage.Conversation,
		assembled.TokenUsage.Total,
		assembled.TokenUsage.Budget,
		assembled.TokenUsage.Headroom,
		len(assembled.EvictedTurns))

	return flattenAssembled(assembled, userMsg)
}

// searchPgVector retrieves the top-k memories for a turn: hybrid
// search (dense pgvector + sparse FTS, RRF-fused) over one or more
// queries, unioned, then — when a reranker is configured — a
// cross-encoder precision pass over a wide candidate pool.
//
// When the tier enables ExpandQueries and a sidecar is available the
// retrieval query is decomposed into 2-4 targeted sub-queries; each
// runs its own hybrid search and the results union by content. The
// primary query always runs so a single-query fallback happens even
// when expansion fails.
//
// Both expansion and reranking are best-effort: any sidecar /
// reranker / embedder failure degrades to a simpler path without
// erroring the turn.
func (p *Pipeline) searchPgVector(
	ctx context.Context,
	userID string,
	userMsg string,
	queryVec []float32,
	tier ctxbuild.PromptTier,
	limit int,
	threshold float64,
	onStatus func(string),
) []memory.MemoryResult {
	if limit <= 0 {
		limit = 5
	}
	rerankOn := p.rerankCfg.Enabled && p.reranker.Available()

	// With a reranker we pull a wide candidate pool (PoolSize) so the
	// cross-encoder has real choice; without one, each arm only needs
	// the final top-k since RRF order is the verdict.
	perSearchLimit := limit
	if rerankOn {
		perSearchLimit = p.rerankCfg.PoolSize
		if perSearchLimit < limit {
			perSearchLimit = limit
		}
	}

	type searchJob struct {
		label string
		vec   []float32
	}
	jobs := []searchJob{{label: userMsg, vec: queryVec}}

	if tier.MemoryConfig.ExpandQueries && p.sidecarClient != nil && p.embedder != nil {
		expandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		expanded, err := p.sidecarClient.ExpandQueries(expandCtx, userMsg)
		cancel()
		if err != nil {
			log.Printf("[pipeline] query expansion failed (continuing with single query): %v", err)
		} else if len(expanded) > 0 {
			if onStatus != nil {
				onStatus(fmt.Sprintf("Expanded queries: %d\n", len(expanded)))
			}
			log.Printf("[pipeline] query expansion: %q -> %v", userMsg, expanded)
			for _, q := range expanded {
				if q == "" || q == userMsg {
					continue
				}
				vec := p.embedText(ctx, q)
				if len(vec) == 0 {
					continue
				}
				jobs = append(jobs, searchJob{label: q, vec: vec})
			}
		}
	}

	// Union by content. Keep the row with the highest RRF fused score
	// across all sub-queries — that's the candidate-pool ranking the
	// reranker (or the top-k cut) consumes next.
	bestByContent := make(map[string]memory.MemoryResult)
	for _, job := range jobs {
		// Bound each sub-query search like its siblings (engine 5s,
		// rels 2s, rerank 5s) so a slow Postgres can't stall the whole
		// turn indefinitely on the otherwise-unbounded request ctx.
		searchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		results, err := p.memStore.HybridSearch(searchCtx, job.label, job.vec, perSearchLimit, threshold, userID)
		cancel()
		if err != nil {
			log.Printf("[pipeline] hybrid search error (%q): %v", job.label, err)
			continue
		}
		for _, r := range results {
			if prev, ok := bestByContent[r.Content]; !ok || r.FusedScore > prev.FusedScore {
				bestByContent[r.Content] = r
			}
		}
	}

	merged := make([]memory.MemoryResult, 0, len(bestByContent))
	for _, r := range bestByContent {
		merged = append(merged, r)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].FusedScore > merged[j].FusedScore
	})

	// Cross-encoder rerank: score the whole pool jointly against the
	// primary query and reorder. On any failure fall back to the RRF
	// order already in `merged`.
	if rerankOn && len(merged) > 1 {
		docs := make([]string, len(merged))
		for i, m := range merged {
			docs[i] = m.Content
		}
		rerankCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		scored, err := p.reranker.Rerank(rerankCtx, userMsg, docs)
		cancel()
		if err != nil {
			log.Printf("[pipeline] rerank failed (using RRF order): %v", err)
		} else {
			reordered := make([]memory.MemoryResult, 0, len(scored))
			for _, s := range scored {
				reordered = append(reordered, merged[s.Index])
			}
			merged = reordered
			if onStatus != nil {
				onStatus(fmt.Sprintf("Reranked %d candidates\n", len(docs)))
			}
		}
	}

	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// flattenAssembled converts the structured AssembledContext into the
// provider-neutral []llm.Message shape. System prompt, summary, memories,
// and tool results collapse into a single system message (matching prior
// pipeline behavior); recent turns become user/assistant messages; the
// current user message is appended last.
func flattenAssembled(a ctxbuild.AssembledContext, userMsg string) []llm.Message {
	var messages []llm.Message
	var sysParts []string

	// Zone order follows the chat-turn context review §2 canonical
	// layout: stable head (system + tool catalog) → user personality
	// prompt → long-term memory → entity graph → current-turn tool
	// results → rolling summary of older conversation. The summary
	// sits LAST so it's adjacent to the recent verbatim turns that
	// follow — "lost in the middle" research wants the recency-
	// critical context (summary + recent turns) bunched at the tail.
	if a.SystemPrompt != "" {
		sysParts = append(sysParts, a.SystemPrompt)
	}
	// User-authored personality prompt — its own labeled section
	// right after the admin system prompt. The "(set by the user)"
	// framing keeps the model from treating it as authoritative
	// system policy that could override admin or safety rules.
	if a.UserPrompt != "" {
		sysParts = append(sysParts, "## Personality preferences (set by the user)\n"+a.UserPrompt)
	}
	if len(a.Memories) > 0 {
		var mb strings.Builder
		mb.WriteString("Relevant context:\n")
		for _, m := range a.Memories {
			mb.WriteString(m.Content)
			mb.WriteByte('\n')
		}
		sysParts = append(sysParts, strings.TrimRight(mb.String(), "\n"))
	}
	if len(a.RelationshipLines) > 0 {
		// Sort the lines so the assembled block is byte-stable
		// turn-to-turn — the relationship store doesn't guarantee
		// row order, and a reshuffled block busts llama.cpp's
		// KV-cache prefix for no benefit.
		lines := append([]string(nil), a.RelationshipLines...)
		sort.Strings(lines)
		var rb strings.Builder
		rb.WriteString("## Entity Knowledge Graph\nThese are verified structured facts extracted from your memory. Use them to answer questions directly.\nWhen the user asks for a complete list (e.g., \"all my X\", \"every Y\", \"list all Z\"), scan the ENTIRE section below for matching triples. Do NOT stop after finding the first few — surface every match.\n")
		for _, line := range lines {
			rb.WriteString(line)
			rb.WriteByte('\n')
		}
		sysParts = append(sysParts, strings.TrimRight(rb.String(), "\n"))
	}
	for _, tr := range a.ToolResults {
		sysParts = append(sysParts, tr.Content)
	}
	if a.ConversationSummary != "" {
		sysParts = append(sysParts, "<conversation_summary>\n"+a.ConversationSummary+"\n</conversation_summary>")
	}

	if len(sysParts) > 0 {
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: strings.Join(sysParts, "\n\n"),
		})
	}
	// Replay history with the tool shape intact: an assistant turn
	// that called tools surfaces its ToolCalls; the following tool
	// rows surface their ToolCallID. Providers need this so the
	// next-turn prompt looks identical to what the model emitted
	// during the original turn — without it, the model loses the
	// work it already did and starts re-solving from scratch.
	for _, t := range a.RecentTurns {
		messages = append(messages, llm.Message{
			Role:       t.Role,
			Content:    t.Content,
			ToolCalls:  unmarshalToolCalls(t.ToolCalls),
			ToolCallID: t.ToolCallID,
		})
	}
	messages = append(messages, llm.Message{Role: "user", Content: userMsg})
	return messages
}

// beginTurn runs the shared pre-tool setup: identity resolution,
// session hydration, and routing. Handle and HandleStream both delegate
// here so the setup block only exists once.
//
// When `overrides` is non-nil and overrides.SkipSessionHydration is
// true, the persisted-summary load is skipped (ephemeral shards start
// fresh every invocation).
func (p *Pipeline) beginTurn(ctx context.Context, sess *session.Session, userMsg string, convCtx *sidecar.ConversationContext, overrides *ShardOverrides) (*routeResult, *RouteInfo, error) {
	info := &RouteInfo{}
	p.resolveIdentity(sess)
	if overrides == nil || !overrides.SkipSessionHydration {
		p.hydrateSession(ctx, sess)
	}
	route, err := p.classifyRequest(ctx, sess, userMsg, convCtx, overrides)
	if err != nil {
		return nil, nil, err
	}
	info.ModelID = route.modelID
	return route, info, nil
}

// executeToolsSync runs pre-execution tool orchestration synchronously.
// HandleStream uses its own concurrent variant (running tools in
// parallel with the preamble); this helper is only for Handle.
func (p *Pipeline) executeToolsSync(ctx context.Context, route *routeResult) prefetch.ExecuteResult {
	var toolResult prefetch.ExecuteResult
	if p.toolOrchestrator != nil && len(route.toolsNeeded) > 0 {
		toolResult = p.toolOrchestrator.Execute(ctx, route.toolsNeeded, route.searchQueries, route.complexityLabel())
		if toolResult.Context != "" {
			log.Printf("[pipeline] tool results: %d bytes, %d results", len(toolResult.Context), toolResult.ResultCount)
		}
	}
	return toolResult
}

// runTurn handles the post-tool portion of a turn: context assembly,
// LLM dispatch, and commit. Streaming and non-streaming both call this
// — streaming passes real callbacks, Handle passes nil for all three.
// The stream flag on the LLM request is derived from onChunk (non-nil
// ⇒ streaming provider path).
//
// When `overrides` is non-nil, context assembly takes the shard path
// (buildShardMessages) and the LLM dispatch, tool loop, and commit are
// all parameterized by the shard envelope.
func (p *Pipeline) runTurn(
	ctx context.Context,
	sess *session.Session,
	userMsg string,
	route *routeResult,
	toolResult prefetch.ExecuteResult,
	info *RouteInfo,
	onChunk func(string),
	onReasoningChunk func(string),
	onStatus func(string),
	preamble string,
	overrides *ShardOverrides,
) (string, error) {
	// Resolve the prompt tier for shard invocations too, so the web-search
	// budget and any future tier-driven knobs inside the LLM dispatch path
	// have a tier to consult. assembleMessages does this for the trusted
	// path; we duplicate it here because buildShardMessages skips that
	// entire function.
	var messages []llm.Message
	if overrides != nil {
		info.Tier = ctxbuild.TierFor(route.complexityLabel())
		messages = p.buildShardMessages(sess, userMsg, overrides, info)
	} else {
		messages = p.assembleMessages(ctx, sess, userMsg, route.modelID, route.complexityLabel(), route.classifier.MemoryDepth, toolResult.Context, info, onStatus)
	}

	// USER-SKILLS-SPEC Phase B: on trusted turns, the user's
	// chat-enabled personal skills ride in as a prompt block, and a
	// non-empty block unlocks the (otherwise shard-only) skillpacks
	// tools for THIS turn. Shard turns get their equivalent via
	// ShardAugment; the two grants never mix.
	userSkillsUnlocked := false
	if overrides == nil && p.userSkillsAugment != nil {
		if block := p.userSkillsAugment(ctx, sess.UserID()); block != "" {
			messages = appendToSystemMessage(messages, block)
			userSkillsUnlocked = true
		}
	}

	if onStatus != nil {
		onStatus("Generating response...\n")
	}

	stream := onChunk != nil
	llmReq := p.buildLLMRequest(messages, route, info, stream, onReasoningChunk, overrides, userSkillsUnlocked)

	llmCtx, llmCancel := context.WithTimeout(ctx, turnHardCap)
	defer llmCancel()

	complete := route.provider.Complete
	if stream {
		complete = func(c context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
			return route.provider.CompleteStream(c, req, onChunk)
		}
	}

	llmResp, loopMsgs, err := p.runCompletion(llmCtx, sess, route.provider, llmReq, route.complexityLabel(), route.classifier.SearchDepth, complete, onStatus, overrides, userSkillsUnlocked, &info.PagesFetched, &info.ResearchNote)
	if err != nil {
		return "", err
	}
	info.InputTokens += llmResp.InputTokens
	info.OutputTokens += llmResp.OutputTokens
	info.DecodeMs += llmResp.DecodeMs
	if llmResp.ReasoningContent != "" {
		info.ReasoningContent = llmResp.ReasoningContent
	}
	// User Stop: StopTurn cancelled turnCtx with the errUserStopped cause
	// mid-stream, and the provider salvaged the partial (FinishReason
	// "stopped"). The partial in llmResp.Content is committed below like
	// any answer; this flag lets the done event label the turn. Require
	// BOTH signals: the cause alone would also fire for a Stop that lands
	// in the sliver of time AFTER a full answer generated (mislabeling a
	// complete turn), and the "stopped" finish alone also covers a
	// hard-cap timeout (not a user stop).
	if llmResp.FinishReason == "stopped" && errors.Is(context.Cause(ctx), errUserStopped) {
		info.Stopped = true
	}
	log.Printf("[pipeline] LLM iter: in=%d out=%d decode_ms=%.0f", llmResp.InputTokens, llmResp.OutputTokens, llmResp.DecodeMs)

	// Fold the streamed preamble (if any) into the committed turn so the
	// stored conversation matches what the user saw live. The preamble
	// already carries its trailing "---" separator. Empty for
	// non-streaming and shard turns, which never generate a preamble.
	responseText := preamble + llmResp.Content

	// Persist only when there's a REAL answer. The heavy model can
	// return empty content (its whole max_tokens went to thinking —
	// the empty-response case commitAndExtract guards against). But
	// when a "let me think…" preamble was streamed, responseText is
	// preamble+"" — non-empty — so commitAndExtract's own guard (which
	// checks the combined text) wouldn't fire, and a no-answer turn
	// would be committed and extracted into memory. Gate on the
	// model's actual content here, where the preamble is visible.
	if strings.TrimSpace(llmResp.Content) != "" {
		p.commitAndExtract(ctx, sess, userMsg, responseText, loopMsgs, info, overrides)
	} else {
		log.Printf("[pipeline] skip commit: empty model answer for session %s (preamble-only turn not persisted)", sess.ID)
	}
	return responseText, nil
}

// Handle processes one user message and returns the assistant response.
// Callers on trusted surfaces (OpenAI adapter, Slack DM, CLI, scheduler)
// use this directly; shard invocations go through HandleShard instead.
func (p *Pipeline) Handle(ctx context.Context, sess *session.Session, userMsg string, convCtx *sidecar.ConversationContext) (string, *RouteInfo, error) {
	return p.handle(ctx, sess, userMsg, convCtx, nil)
}

// handle is the overrides-aware implementation backing both Handle
// (trusted path, overrides=nil) and HandleShard (shard path, overrides
// non-nil). Keeping one implementation means the two paths can't drift
// on anything except the parts the overrides struct explicitly controls.
func (p *Pipeline) handle(ctx context.Context, sess *session.Session, userMsg string, convCtx *sidecar.ConversationContext, overrides *ShardOverrides) (string, *RouteInfo, error) {
	p.augmentShardOverrides(ctx, overrides)
	route, info, err := p.beginTurn(ctx, sess, userMsg, convCtx, overrides)
	if err != nil {
		return "", nil, err
	}

	// Same detach as the streaming path: once we start producing the
	// turn, a client that hangs up shouldn't cost us the generated
	// answer (and its extraction). See turnContext.
	turnCtx, turnCancel := p.turnContext(ctx, sess.ID)
	defer turnCancel()

	// Shards skip pre-execution tool orchestration — their tools are in
	// the allowlist attached to the LLM request, not prefetched by the
	// sidecar. executeToolsSync is a no-op when route.toolsNeeded is
	// empty, which is always true on the shard path; the explicit
	// branch just makes that intention visible.
	var toolResult prefetch.ExecuteResult
	if overrides == nil {
		toolResult = p.executeToolsSync(turnCtx, route)
	}

	// Non-streaming path: no preamble (it's a streaming-only lead-in).
	text, err := p.runTurn(turnCtx, sess, userMsg, route, toolResult, info, nil, nil, nil, "", overrides)
	if err != nil {
		return "", info, err
	}
	return text, info, nil
}

// HandleStream is like Handle but streams chunks via onChunk callback.
// Divergence from Handle is limited to what must differ: tool
// orchestration runs concurrently with the preamble, LLM calls use the
// streaming provider path, and status/reasoning callbacks are wired up.
func (p *Pipeline) HandleStream(
	ctx context.Context,
	sess *session.Session,
	userMsg string,
	convCtx *sidecar.ConversationContext,
	onChunk func(string),
	onReasoningChunk func(string),
	onStatus func(string),
) (string, *RouteInfo, error) {
	return p.handleStream(ctx, sess, userMsg, convCtx, onChunk, onReasoningChunk, onStatus, nil)
}

// handleStream is the overrides-aware implementation backing both
// HandleStream (trusted) and HandleShardStream (shard).
func (p *Pipeline) handleStream(
	ctx context.Context,
	sess *session.Session,
	userMsg string,
	convCtx *sidecar.ConversationContext,
	onChunk func(string),
	onReasoningChunk func(string),
	onStatus func(string),
	overrides *ShardOverrides,
) (string, *RouteInfo, error) {
	p.augmentShardOverrides(ctx, overrides)
	route, info, err := p.beginTurn(ctx, sess, userMsg, convCtx, overrides)
	if err != nil {
		return "", nil, err
	}

	// From here on we produce the actual turn. Detach from the request
	// context so an SSE client that disconnects mid-stream gets the
	// whole turn finished and persisted rather than truncated — the
	// stream writes become best-effort no-ops, but generation, tools,
	// and the commit run to completion (bounded by turnHardCap /
	// shutdown). See turnContext.
	turnCtx, turnCancel := p.turnContext(ctx, sess.ID)
	defer turnCancel()

	// Tool execution and preamble run concurrently on the trusted path.
	// Shards skip both: their tools are attached to the LLM request
	// directly (no prefetch), and they don't run the preamble model
	// (which is a Familiar-voice UX affordance, not a shard concern).
	var toolResult prefetch.ExecuteResult
	var preamble string
	if overrides == nil {
		var toolWg sync.WaitGroup
		if p.toolOrchestrator != nil && len(route.toolsNeeded) > 0 {
			toolWg.Add(1)
			go func() {
				defer toolWg.Done()
				toolResult = p.toolOrchestrator.Execute(turnCtx, route.toolsNeeded, route.searchQueries, route.complexityLabel())
				if toolResult.Context != "" {
					log.Printf("[pipeline] tool results: %d bytes, %d results", len(toolResult.Context), toolResult.ResultCount)
				}
			}()
		}

		// Preamble fires when the classifier flags the request as
		// thinking=high — the user is going to wait, so a "let me
		// think about this" stream from the sidecar bridges the gap
		// while the heavy chat model warms up. Per CHAT-REARCH
		// §"Phase 3" gate.
		if route.classifier.Thinking == classifier.ThinkingHigh {
			preamble = p.generatePreamble(turnCtx, userMsg, route.complexityLabel(), onChunk)
		}
		toolWg.Wait()

		// Surface routing metadata after the preamble so reasoning
		// chunks stay contiguous in the thinking block.
		if onStatus != nil {
			complexityLabel := route.complexityLabel()
			if complexityLabel == "" {
				complexityLabel = "unclassified"
			}
			modelShort := route.modelID
			if idx := strings.LastIndex(route.modelID, "/"); idx >= 0 {
				modelShort = route.modelID[idx+1:]
			}
			statusMsg := fmt.Sprintf("Complexity: %s | Model: %s", complexityLabel, modelShort)
			if len(route.toolsNeeded) > 0 {
				statusMsg += fmt.Sprintf(" | Tools: %s", strings.Join(route.toolsNeeded, ", "))
			}
			onStatus(statusMsg + "\n")
			if len(toolResult.Queries) > 0 {
				onStatus(fmt.Sprintf("Searched: %s\n", strings.Join(toolResult.Queries, " | ")))
				onStatus(fmt.Sprintf("Found: %d results\n", toolResult.ResultCount))
			}
		}
	}

	text, err := p.runTurn(turnCtx, sess, userMsg, route, toolResult, info, onChunk, onReasoningChunk, onStatus, preamble, overrides)
	if err != nil {
		return "", info, err
	}
	return text, info, nil
}

// modelSupportsTools reports whether the routed model advertises the
// "tools" capability in its config. Models without this tag get
// conventional completions — no tool specs in the request, no
// tool-loop dispatch.
func (p *Pipeline) modelSupportsTools(modelID string) bool {
	if p.router == nil {
		return false
	}
	mc := p.router.GetRegistry().GetModelConfig(modelID)
	if mc == nil {
		return false
	}
	for _, cap := range mc.Capabilities {
		if cap == "tools" {
			return true
		}
	}
	return false
}

// skillToolSpecs projects the registered skill tools into the
// provider-agnostic llm.ToolSpec shape. Returns nil when the registry
// is absent or empty, which is how callers signal "skip tools" without
// a separate flag.
//
// includeUserSkillTools flips whether the shard-only tools
// (imported-skill access) are offered: normally never on the trusted
// path — the shard path advertises them via the allowlist filter
// (filterToolSpecs) instead — EXCEPT on a trusted turn where the
// user-skills grant is active (USER-SKILLS-SPEC Phase B).
func (p *Pipeline) skillToolSpecs(includeUserSkillTools bool) []llm.ToolSpec {
	if p.skillRegistry == nil {
		return nil
	}
	defs := p.skillRegistry.ToolDefinitions()
	if len(defs) == 0 {
		return nil
	}
	specs := make([]llm.ToolSpec, 0, len(defs))
	for _, d := range defs {
		if p.shardOnlyTools[d.Name] && !includeUserSkillTools {
			continue
		}
		specs = append(specs, llm.ToolSpec{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.Parameters,
		})
	}
	return specs
}

// appendToSystemMessage folds an extra block into the turn's system
// message (or prepends one when the turn has none). Used for the
// per-user skills block, which is computed after ctxbuild assembly —
// the block is small and hard-capped upstream, so bypassing the
// token-budget accounting is acceptable, same as the shard path's
// prompt-block append.
func appendToSystemMessage(messages []llm.Message, block string) []llm.Message {
	for i := range messages {
		if messages[i].Role == "system" {
			messages[i].Content += "\n\n" + block
			return messages
		}
	}
	return append([]llm.Message{{Role: "system", Content: block}}, messages...)
}

// augmentShardOverrides applies the optional shard augmenter (bound
// imported skills → prompt block + skillpacks tools). Failures
// degrade: the turn proceeds without skills rather than failing —
// consistent with the pipeline's resilience hierarchy.
func (p *Pipeline) augmentShardOverrides(ctx context.Context, ov *ShardOverrides) {
	if ov == nil || p.shardAugment == nil {
		return
	}
	if err := p.shardAugment(ctx, ov); err != nil {
		log.Printf("[pipeline] shard augment (%s): %v — continuing without imported skills", ov.ShardID, err)
	}
}

// completeFn is the per-iteration LLM call used by runToolLoop. It lets
// callers plug in either provider.Complete (non-streaming) or a
// CompleteStream-backed closure without duplicating the loop body. The
// returned response must have ToolCalls populated if the model
// requested any — both provider implementations take care of that.
type completeFn func(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error)

// runToolLoop drives a multi-turn completion when the model chooses to
// invoke tools. Each iteration calls `complete` with the running
// message slice; while the response contains tool_calls and we are
// under the iteration cap, it dispatches each call through the skill
// registry, appends the assistant message + tool results to the
// conversation, and calls `complete` again.
//
// It returns the final response and the updated message slice (with
// every intermediate assistant + tool turn) so callers can inspect
// what the model saw.
//
// Cancellation: each complete call honours the provided ctx. Tool
// dispatches use a bounded per-call timeout derived from ctx so a slow
// tool can't stall the whole exchange.
//
// `allowlist`, when non-nil, constrains which tool names the registry
// will dispatch. A tool_call naming a tool outside the allowlist is
// rejected with a synthetic tool-result error and logged at warn
// level. Trusted-path callers pass nil to keep the historical
// unrestricted behavior.
//
// `userSkillsUnlocked` lifts the trusted-path ban on shard-only tools
// for this turn — set only when the user-skills grant advertised them
// (USER-SKILLS-SPEC Phase B); shard turns (non-nil allowlist) ignore it.
func (p *Pipeline) runToolLoop(
	ctx context.Context,
	baseReq llm.CompletionRequest,
	complexity string,
	searchDepth classifier.SearchDepth,
	searchBudgetOverride int,
	complete completeFn,
	onStatus func(string),
	allowlist map[string]bool,
	userSkillsUnlocked bool,
	pagesFetched *int, // optional fetch_page tally (nil to skip); §6.7 stats
	researchNote *ResearchNoteRef, // optional research-note sink (nil to skip)
) (*llm.CompletionResponse, []llm.Message, error) {
	messages := baseReq.Messages
	// baseLen marks the boundary between the incoming history (system
	// + prior turns + this turn's user message) and the loop's own
	// additions. On exit we slice `messages[baseLen:]` so the caller
	// gets ONLY the assistant↔tool messages this loop produced —
	// commitAndExtract persists those into the session so subsequent
	// turns inherit the work.
	baseLen := len(messages)
	maxIters := p.maxToolIters
	if maxIters <= 0 {
		maxIters = 5
	}

	// Per-turn budget for web_search tool calls. The classifier-driven
	// SearchBudget is the new authority; we fall through to the legacy
	// tier-driven cap so deployments without [effort.search.*] overrides
	// keep working. When exhausted, we short-circuit web_search with a
	// synthetic tool error so the model sees "budget exhausted" and
	// moves on without hitting the Brave API. Other tools are unaffected.
	tier := ctxbuild.TierFor(complexity)
	webSearchBudget := tier.MaxWebSearches
	webSearchDisabled := false
	searchBudget := p.effort.SearchFor(searchDepth)
	if searchBudget.Skip {
		webSearchDisabled = true
	} else if searchBudget.MaxSearches > 0 {
		webSearchBudget = searchBudget.MaxSearches
	}
	// Envelope-level grant (ShardOverrides.SearchBudget): purpose-built
	// shard envelopes — research workers — get web_search despite the
	// SearchNone their synthesized classifier output carries. The
	// envelope is constructed server-side; page content and skill text
	// can never set it.
	if searchBudgetOverride > 0 {
		webSearchDisabled = false
		webSearchBudget = searchBudgetOverride
	}
	webSearchesUsed := 0

	// If the web-search budget exceeds the base tool-loop iteration
	// cap, the model might spend every iteration on a single search
	// (no batching) and run out of iterations before writing its
	// response. Grow the iteration cap to accommodate: budget + 3
	// gives room for (budget) search rounds plus 2 synthesis passes
	// plus 1 buffer for any other tool calls.
	if webSearchBudget > 0 && webSearchBudget+3 > maxIters {
		maxIters = webSearchBudget + 3
	}

	// Tool-loop diagnostics. Kept at INFO so a single grep `[tools]`
	// answers "is the model emitting tool calls? are they dispatching?
	// are any being blocked?" without re-deploying with a debug flag.
	// The trusted-path tool loop has been a black box for months —
	// per FAMILIAR-SHARDS-PHASE1-FINDINGS the same ambiguity bites
	// shard invocations too, so the logs stay on for both paths.
	advertisedNames := make([]string, 0, len(baseReq.Tools))
	for _, t := range baseReq.Tools {
		advertisedNames = append(advertisedNames, t.Name)
	}
	allowlistNames := make([]string, 0, len(allowlist))
	for n := range allowlist {
		allowlistNames = append(allowlistNames, n)
	}
	log.Printf("[tools] loop start: model=%s advertised=%v allowlist=%v max_iters=%d",
		baseReq.Model, advertisedNames, allowlistNames, maxIters)

	var lastResp *llm.CompletionResponse
	var accumIn, accumOut int
	var accumDecodeMs float64
	// Tool-content budget for the whole loop. Each result is head+tail
	// capped, and once the accumulated tool output would exceed the
	// tools zone, further results collapse to a short "answer from what
	// you have" notice. Without this a chain of large results overflows
	// the model's context window into a hard provider 400 that fails
	// the entire turn (only some skills self-cap; wiki/memory/notes
	// don't). The notices keep the context bounded and nudge the model
	// to synthesize.
	perResultCap := p.ctxCfg.MaxToolResultTokens
	if perResultCap <= 0 {
		perResultCap = 2000
	}
	toolTokenBudget := p.ctxCfg.Resolve().Tools
	var toolTokensUsed int
	for i := 0; i < maxIters; i++ {
		// User Stop (or hard cap / shutdown) landing between iterations:
		// return the last good response so its partial commits, rather
		// than erroring the whole turn. A mid-stream cut is handled inside
		// the provider's CompleteStream, which returns its partial; this
		// guard only covers the gap between completions. With no response
		// yet there's nothing to salvage — propagate the cancellation.
		if err := ctx.Err(); err != nil {
			if lastResp != nil {
				lastResp.InputTokens = accumIn
				lastResp.OutputTokens = accumOut
				lastResp.DecodeMs = accumDecodeMs
				if errors.Is(context.Cause(ctx), errUserStopped) {
					lastResp.FinishReason = "stopped"
				}
				return lastResp, messages[baseLen:], nil
			}
			return nil, messages[baseLen:], err
		}

		req := baseReq
		req.Messages = messages

		resp, err := complete(ctx, req)
		if err != nil {
			return nil, messages[baseLen:], fmt.Errorf("llm complete (iter %d): %w", i, err)
		}
		lastResp = resp
		accumIn += resp.InputTokens
		accumOut += resp.OutputTokens
		accumDecodeMs += resp.DecodeMs
		log.Printf("[tools] iter=%d tokens: in=%d out=%d decode_ms=%.0f",
			i, resp.InputTokens, resp.OutputTokens, resp.DecodeMs)

		// Stop landing DURING this completion: the provider salvaged the
		// partial, but it may carry a tool call the model had begun (the
		// user often never saw it — llama buffers the whole <tool_call>
		// block). Dispatching it would run the exact side effect the user
		// pressed Stop to cancel, so drop the calls and return the text
		// only. The between-iterations guard above catches a cut in the
		// gap between completions; this catches a cut mid-completion.
		if ctx.Err() != nil {
			resp.ToolCalls = nil
			resp.InputTokens = accumIn
			resp.OutputTokens = accumOut
			resp.DecodeMs = accumDecodeMs
			if errors.Is(context.Cause(ctx), errUserStopped) {
				resp.FinishReason = "stopped"
			}
			return resp, messages[baseLen:], nil
		}

		callNames := make([]string, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			callNames = append(callNames, tc.Name)
		}
		log.Printf("[tools] iter=%d finish=%s content_len=%d tool_calls=%d names=%v",
			i, resp.FinishReason, len(resp.Content), len(resp.ToolCalls), callNames)

		if len(resp.ToolCalls) == 0 {
			// Stamp accumulated totals from all iterations.
			resp.InputTokens = accumIn
			resp.OutputTokens = accumOut
			resp.DecodeMs = accumDecodeMs
			// Diagnostic: when tools were advertised but the model
			// returned only text, note it — but log only shape metrics,
			// never the model's content (which can carry user PII or
			// extracted secrets). Whether the text looked tool-call-shaped
			// is enough signal for the "model knew but llama-server didn't
			// parse" vs "genuinely declined" distinction.
			if len(advertisedNames) > 0 {
				looksToolShaped := strings.Contains(resp.Content, "<tool_call>") ||
					strings.Contains(resp.Content, "<function") ||
					strings.Contains(resp.Content, "\"name\"")
				log.Printf("[tools] iter=%d no tool_calls despite %d tools advertised (content_len=%d tool_shaped=%t)",
					i, len(advertisedNames), len(resp.Content), looksToolShaped)
			}
			return resp, messages[baseLen:], nil
		}

		if onStatus != nil {
			names := make([]string, 0, len(resp.ToolCalls))
			for _, tc := range resp.ToolCalls {
				names = append(names, tc.Name)
			}
			onStatus(fmt.Sprintf("Calling tools: %s\n", strings.Join(names, ", ")))
		}

		// Echo the assistant turn (including its tool_calls) so the
		// model sees its own prior decision on the next round.
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		for _, tc := range resp.ToolCalls {
			// Shard allowlist enforcement. A non-nil allowlist restricts
			// what the registry will dispatch; a blocked call is logged
			// at warn level and returned to the model as a synthetic
			// tool-result error so the LLM can adapt without hanging.
			// Trusted-path callers pass a nil allowlist and skip this
			// branch entirely.
			if allowlist == nil && p.shardOnlyTools[tc.Name] && !userSkillsUnlocked {
				// Shard-only tools are never advertised on the
				// trusted path, but a model can hallucinate a name —
				// refuse at dispatch too. The one exception: a turn
				// where the user-skills grant advertised them
				// (userSkillsUnlocked); the skillpacks backend still
				// authorizes per-skill on top.
				log.Printf("[pipeline] blocked shard-only tool on trusted path: tool=%q", tc.Name)
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    fmt.Sprintf("tool %q is only available inside a shard.", tc.Name),
					ToolCallID: tc.ID,
					Name:       tc.Name,
				})
				continue
			}
			if allowlist != nil && !allowlist[tc.Name] {
				log.Printf("[shards] blocked tool call outside shard allowlist: tool=%q", tc.Name)
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    fmt.Sprintf("tool %q is not available in this shard's allowlist.", tc.Name),
					ToolCallID: tc.ID,
					Name:       tc.Name,
				})
				continue
			}
			// web_search per-turn budget enforcement. webSearchDisabled
			// means the classifier explicitly chose SearchNone — block
			// every call. Otherwise a positive budget short-circuits
			// once exhausted; budget 0 means "no limit" (legacy behavior
			// when the tier has no MaxWebSearches set).
			if tc.Name == "web_search" && webSearchDisabled {
				log.Printf("[pipeline] web_search disabled by classifier (tier=%s)", complexity)
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    "web_search is not available for this turn. Synthesize an answer from what you already have.",
					ToolCallID: tc.ID,
					Name:       tc.Name,
				})
				continue
			}
			if tc.Name == "web_search" && webSearchBudget > 0 && webSearchesUsed >= webSearchBudget {
				log.Printf("[pipeline] web_search budget exhausted (tier=%s, used=%d, max=%d)",
					complexity, webSearchesUsed, webSearchBudget)
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    fmt.Sprintf("web_search budget exhausted for this turn (%d/%d used at tier %s). Synthesize an answer from what you already have.", webSearchesUsed, webSearchBudget, complexity),
					ToolCallID: tc.ID,
					Name:       tc.Name,
				})
				continue
			}
			if tc.Name == "web_search" {
				webSearchesUsed++
			}
			if tc.Name == "fetch_page" && pagesFetched != nil {
				*pagesFetched++
			}

			toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			result, execErr := p.skillRegistry.Execute(toolCtx, tc.Name, tc.Arguments)
			cancel()

			var content string
			switch {
			case execErr != nil:
				log.Printf("[pipeline] tool %q transport error: %v", tc.Name, execErr)
				content = fmt.Sprintf("tool %q failed: %v", tc.Name, execErr)
			case result.Error != "":
				log.Printf("[pipeline] tool %q error: %s", tc.Name, result.Error)
				content = fmt.Sprintf("tool %q error: %s", tc.Name, result.Error)
			default:
				content = result.Content
			}

			// Head+tail cap this single result, then charge it against
			// the turn's cumulative tool budget. Over budget → collapse
			// to a short notice instead of the payload so the context
			// stays bounded and the model answers from what it has.
			content, toolTokensUsed = budgetToolResult(tc.Name, content, perResultCap, toolTokenBudget, toolTokensUsed)

			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})

			// Notify streaming clients about side effects so the
			// workspace UI can react immediately (e.g. refresh the
			// notes panel when append_to_note completes, or reload
			// an open wiki page after the AI edits it). The signal
			// rides the existing onStatus → reasoning_content SSE
			// channel with a prefix that chat.js detects mid-stream
			// and converts into a familiar:notesChanged event.
			if onStatus != nil && execErr == nil && result.Error == "" {
				if strings.Contains(tc.Name, "note") || strings.Contains(tc.Name, "page") {
					onStatus("__TOOL_EFFECT__:note_changed:" + tc.Name + "\n")
				}
			}

			// Record a research note written this turn so the "done"
			// event can auto-open + link it (§research inline path).
			if researchNote != nil && execErr == nil && result.Error == "" {
				if ref, ok := researchNoteFrom(tc.Name, result.Data); ok {
					*researchNote = ref
				}
			}
		}
	}

	log.Printf("[pipeline] tool loop hit max iterations (%d); returning last response", maxIters)
	if lastResp != nil {
		lastResp.InputTokens = accumIn
		lastResp.OutputTokens = accumOut
		lastResp.DecodeMs = accumDecodeMs
	}
	return lastResp, messages[baseLen:], nil
}

// budgetToolResult bounds one tool result for the tool loop: it
// head+tail caps the content to perResultCap tokens, then charges it
// against the turn's cumulative tool-token budget. If adding it would
// exceed that budget, the payload is dropped for a short notice (so the
// context can't overflow into a hard provider 400) that tells the model
// to synthesize. budget <= 0 disables the cumulative check (the
// per-result cap still applies). Returns the content to append and the
// new running total.
func budgetToolResult(toolName, content string, perResultCap, budget, used int) (string, int) {
	content = ctxbuild.CapToolResult(content, perResultCap)
	tk := ctxbuild.EstimateTokens(content)
	if budget > 0 && used+tk > budget {
		content = fmt.Sprintf("[%s output omitted — this turn's tool results hit their ~%d-token budget. Answer from what you already have; do not call more tools.]", toolName, budget)
		tk = ctxbuild.EstimateTokens(content)
	}
	return content, used + tk
}

// MakeEmbedder returns an EmbedFunc that calls an OpenAI-compatible /v1/embeddings endpoint.
func (p *Pipeline) MakeEmbedder(cfg config.EmbedderConfig) EmbedFunc {
	if cfg.Endpoint == "" {
		return nil
	}

	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	model := cfg.Model
	if model == "" {
		model = "nomic-embed-text"
	}

	return func(ctx context.Context, text string) ([]float32, error) {
		type embedReq struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		type embedResp struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}

		// nomic-embed-text-v1.5 requires task prefixes for optimal retrieval.
		// Queries get "search_query: ", documents get "search_document: ".
		prefixedText := "search_query: " + text

		body, err := json.Marshal(embedReq{Model: model, Input: prefixedText})
		if err != nil {
			return nil, fmt.Errorf("marshaling embed request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			endpoint+"/v1/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("building embed request: %w", err)
		}
		req.Header.Set("content-type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("embed request: %w", err)
		}
		defer resp.Body.Close()

		respBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading embed response: %w", err)
		}

		var er embedResp
		if err := json.Unmarshal(respBytes, &er); err != nil {
			return nil, fmt.Errorf("parsing embed response: %w", err)
		}

		if er.Error != nil {
			return nil, fmt.Errorf("embed API error: %s", er.Error.Message)
		}

		if len(er.Data) == 0 {
			return nil, fmt.Errorf("empty embedding response")
		}

		return er.Data[0].Embedding, nil
	}
}

// GetRouter returns the pipeline's router (for external access).
func (p *Pipeline) GetRouter() *router.Router {
	return p.router
}

// modelIDToProviderModel extracts the model name from an ID like "llama-server/qwen3-30b".
func modelIDToProviderModel(modelID string) string {
	if idx := strings.Index(modelID, "/"); idx >= 0 {
		return modelID[idx+1:]
	}
	return modelID
}

// routeResult bundles everything routing produces: the selected provider
// and model, plus the per-turn classifier output that drives every
// downstream zone (memory depth, thinking, search budget, tools).
//
// CHAT-REARCH S2.3: classifier.Output is now the single source of
// truth for effort. The legacy `complexity` string + booleans are
// gone; a small helper derives a complexity-shaped label from the
// thinking level for transitional readers (prefetch.Execute, ctxbuild
// .TierFor) that still take a string.
type routeResult struct {
	modelID    string
	provider   llm.Provider
	classifier classifier.Output

	// Populated from classifier.Tools at construction; readers
	// expect a populated slice today, so we mirror it here. Will
	// be inlined into the classifier field as readers cut over.
	toolsNeeded   []string
	searchQueries []string
}

// complexityLabel maps the classifier's thinking level to a tier key
// in ctxbuild's `tiers` table. Every value returned here MUST be a
// real key in that table — TierFor silently falls back to "knowledge"
// on a miss, which is exactly the bug that used to send every
// high-effort turn to the knowledge tier ("deep" was returned but the
// table key is "deep_reasoning"). Mapping:
//
//	off    → "trivial"
//	low    → "knowledge"
//	medium → "analytical"      (reasoning overlay + bigger budgets)
//	high   → "deep_reasoning"
//
// low and medium are deliberately distinct now — the middle band
// gets the reasoning tier rather than collapsing onto knowledge.
func (r *routeResult) complexityLabel() string {
	switch r.classifier.Thinking {
	case classifier.ThinkingOff:
		return "trivial"
	case classifier.ThinkingLow:
		return "knowledge"
	case classifier.ThinkingMedium:
		return "analytical"
	case classifier.ThinkingHigh:
		return "deep_reasoning"
	}
	return "knowledge"
}

// classifyRequest resolves the per-turn classifier output + the chat
// model that will generate the response. Two paths:
//
//  1. shard overrides (non-nil): the shard's pinned model + a
//     synthetic classifier.Output reflecting the shard's
//     "no memory, no router" posture;
//  2. trusted path: classifier (sidecar) emits the ordinal effort
//     levels; chat model is whichever the router picks.
//
// Replaces the old routeRequest which carried complexity strings,
// inject_memory / enable_thinking booleans, and force_tools flags.
func (p *Pipeline) classifyRequest(ctx context.Context, sess *session.Session, userMsg string, convCtx *sidecar.ConversationContext, overrides *ShardOverrides) (*routeResult, error) {
	r := &routeResult{}

	// Shard path: explicit model wins. Synthesize a conservative
	// classifier.Output — shards skip memory entirely, run no
	// search, and get their tools from the shard's own allowlist
	// (not from the classifier).
	if modelID, complexity, ok := p.shardModelOverride(overrides); ok {
		provider, err := p.router.GetRegistry().GetProvider(modelID, p.apiKeyFn)
		if err != nil {
			return nil, fmt.Errorf("shard model provider: %w", err)
		}
		r.modelID = modelID
		r.provider = provider
		r.classifier = shardClassifierOutput(complexity)
		return r, nil
	}

	// Trusted path. CHAT-REARCH single-chat-model architecture:
	//   1. chat model is the registry's only non-role-tagged entry
	//      (rule-based fallback if multiple are registered)
	//   2. classification comes from the sidecar's small slot via
	//      Client.Classify, returning ordinal effort levels
	chatID := p.router.GetChatModelID()
	// Maintenance mode: the big model is down / drained — route this
	// turn to the operator-selected fallback model instead. The
	// classifier (sidecar small slot) still runs unchanged; only the
	// answering model changes. If the fallback isn't a usable provider
	// we fall through to the normal path rather than failing the turn.
	if p.maintenance != nil {
		if active, fallbackID := p.maintenance.Active(); active && fallbackID != "" {
			if provider, err := p.router.GetRegistry().GetProvider(fallbackID, p.apiKeyFn); err == nil {
				log.Printf("[pipeline] maintenance mode: routing chat to fallback %q", fallbackID)
				r.modelID = fallbackID
				r.provider = provider
			} else {
				log.Printf("[pipeline] maintenance fallback %q unavailable: %v — using normal route", fallbackID, err)
			}
		}
	}
	// Normal resolution — skipped when maintenance already picked a
	// provider above.
	if r.provider == nil {
		if chatID == "" {
			// Empty registry or every model claims a role; let the
			// router pick something so we still return a usable
			// provider rather than 500ing. Router.Select has its own
			// rule-based fallback that picks the first online model.
			modelID, provider, err := p.router.Select(ctx, userMsg, sess.ChannelID, p.apiKeyFn)
			if err != nil {
				return nil, fmt.Errorf("no chat model available: %w", err)
			}
			r.modelID = modelID
			r.provider = provider
		} else {
			provider, err := p.router.GetRegistry().GetProvider(chatID, p.apiKeyFn)
			if err != nil {
				return nil, fmt.Errorf("chat model provider %q: %w", chatID, err)
			}
			r.modelID = chatID
			r.provider = provider
		}
	}

	// Classify. Sidecar Client.Classify always returns a valid
	// Output (failures fall back to ConservativeFallback per spec).
	if p.sidecarClient != nil {
		// Build the 2-3 turn history slice the classifier prompt
		// expects, in chronological order. Spec is explicit about
		// not reversing — reversed dialogue reads as off-distribution.
		recent := sess.RecentTurns(3)
		history := make([]sidecar.Turn, 0, len(recent))
		for _, t := range recent {
			history = append(history, sidecar.Turn{Role: t.Role, Content: t.Content})
		}
		r.classifier = p.sidecarClient.Classify(ctx, history, userMsg)
	} else {
		// No sidecar wired: conservative fallback.
		r.classifier = classifier.ConservativeFallback()
	}
	r.toolsNeeded = append([]string(nil), r.classifier.Tools...)

	// CHAT-REARCH §"Smaller Hardening" — stamp the classifier verdict
	// onto the session for debug surfaces + future rapid-follow-up
	// heuristics. Cheap atomic write; nil-safe in shard mock paths
	// since SetLastClassifier handles a nil receiver.
	sess.SetLastClassifier(r.classifier)

	// Per-turn verdict log — makes the classifier observable. Shows
	// the raw ordinal levels alongside the resolved tier so a
	// "everything is knowledge" pattern is visible at a glance.
	log.Printf("[pipeline] classified: thinking=%s memory=%s search=%s tools=%v → tier=%s",
		r.classifier.Thinking, r.classifier.MemoryDepth, r.classifier.SearchDepth,
		r.classifier.Tools, r.complexityLabel())

	return r, nil
}

// shardClassifierOutput synthesizes a classifier verdict for the
// shard path. Shards deliberately bypass classification: memory and
// search are off; thinking is mapped from the shard's tier hint;
// tools come from the shard's allowlist (not from this struct).
func shardClassifierOutput(complexity string) classifier.Output {
	thinking := classifier.ThinkingMedium
	switch complexity {
	case "trivial":
		thinking = classifier.ThinkingOff
	case "knowledge":
		thinking = classifier.ThinkingLow
	case "technical":
		thinking = classifier.ThinkingMedium
	case "deep_reasoning":
		thinking = classifier.ThinkingHigh
	}
	return classifier.Output{
		Thinking:    thinking,
		MemoryDepth: classifier.MemoryNone,
		SearchDepth: classifier.SearchNone,
	}
}

// buildLLMRequest packages the assembled messages + routing decisions
// into a provider-neutral request. stream selects streaming vs
// non-streaming; onReasoningChunk is non-nil only for HandleStream.
//
// When `overrides` is non-nil:
//   - MaxTokens comes from overrides.MaxTokens (or falls through to the
//     default when zero).
//   - Temperature is propagated to the provider.
//   - Tool advertisement follows the shard's allowlist exclusively; the
//     tier-driven InjectTools logic does not apply. An empty or nil
//     allowlist yields no tools.
func (p *Pipeline) buildLLMRequest(messages []llm.Message, route *routeResult, info *RouteInfo, stream bool, onReasoningChunk func(string), overrides *ShardOverrides, userSkillsUnlocked bool) llm.CompletionRequest {
	// answerTokens is the budget reserved for the visible reply.
	answerTokens := 4096
	if overrides != nil && overrides.MaxTokens > 0 {
		answerTokens = overrides.MaxTokens
	}
	// Thinking is gated by the classifier's ordinal level; the
	// EffortResolver maps the level to a concrete token budget
	// (operator-tunable via [effort.thinking.<level>]). Falls
	// back to spec defaults when no override is set.
	thinkingBudget := p.effort.ThinkingFor(route.classifier.Thinking)

	// On local llama-server (and any OpenAI-compatible reasoning model)
	// the <think> tokens come out of the SAME output allowance as the
	// answer — there's no separate thinking budget field, it's all
	// max_tokens / n_predict. So a thinking budget larger than the
	// answer budget (e.g. deep_reasoning's 8000 vs the 4096 default)
	// could be spent entirely inside <think>, leaving zero tokens for
	// the reply — the empty-response failure commitAndExtract has to
	// defend against. Reserve max_tokens for the ANSWER and add the
	// thinking budget on top. See EXTERNAL-READINESS-REVIEW.md.
	maxTokens := answerTokens
	if thinkingBudget.Enabled && thinkingBudget.TokenBudget > 0 {
		maxTokens += thinkingBudget.TokenBudget
	}
	req := llm.CompletionRequest{
		Model:             modelIDToProviderModel(route.modelID),
		Messages:          messages,
		MaxTokens:         maxTokens,
		Stream:            stream,
		EnableThinking:    thinkingBudget.Enabled,
		MaxThinkingTokens: thinkingBudget.TokenBudget,
		OnReasoningChunk:  onReasoningChunk,
	}
	if overrides != nil {
		req.Temperature = overrides.Temperature
		req.Tools = p.filterToolSpecs(overrides.ToolAllowlist)
		return req
	}
	// Always include tools for models that support them. The model
	// decides whether to use tools, not the classifier — gating tools
	// by tier loses context (e.g. memory search for "what's my dog's
	// name?") and also crashes Cohere2's Jinja template when history
	// contains tool messages but the current turn has no tools array.
	if p.modelSupportsTools(route.modelID) {
		req.Tools = p.skillToolSpecs(userSkillsUnlocked)
	}
	return req
}

// runCompletion dispatches one LLM call, routing through runToolLoop
// when tools are attached and directly through complete otherwise.
// complexity is the router classification used to size per-turn tool
// budgets (currently just web_search; see ctxbuild.PromptTier).
//
// When `overrides` is non-nil, the skill dispatch context carries the
// shard's scope_tag so memory-writing skills can tag rows, and the
// tool loop runs with an allowlist derived from overrides.ToolAllowlist.
// Trusted-path callers pass nil.
func (p *Pipeline) runCompletion(ctx context.Context, sess *session.Session, provider llm.Provider, req llm.CompletionRequest, complexity string, searchDepth classifier.SearchDepth, complete completeFn, onStatus func(string), overrides *ShardOverrides, userSkillsUnlocked bool, pagesFetched *int, researchNote *ResearchNoteRef) (*llm.CompletionResponse, []llm.Message, error) {
	if len(req.Tools) > 0 {
		loopCtx := skills.WithContext(ctx, skills.SessionContext{
			SessionID:      sess.ID,
			UserID:         sess.UserID(),
			AgentID:        p.agentID,
			ChannelID:      sess.ChannelID,
			ShardID:        shardIDFor(overrides),
			ScopeTag:       scopeTagFor(overrides),
			BookScope:      bookScopeFor(overrides),
			ExcludeFromHot: excludeFromHotFor(overrides),
		})
		var allowlist map[string]bool
		if overrides != nil {
			allowlist = toolAllowlistSet(overrides.ToolAllowlist)
		}
		resp, loopMsgs, err := p.runToolLoop(loopCtx, req, complexity, searchDepth, searchBudgetFor(overrides), complete, onStatus, allowlist, userSkillsUnlocked, pagesFetched, researchNote)
		if err != nil {
			return nil, nil, fmt.Errorf("LLM tool loop: %w", err)
		}
		return resp, loopMsgs, nil
	}
	resp, err := complete(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("LLM completion: %w", err)
	}
	return resp, nil, nil
}

// commitAndExtract persists the exchange and kicks off the post-turn
// memory write pipeline:
//  1. Save a single FactProto for the conversation turn.
//  2. Append verbatim turns to the session buffer.
//  3. Async: extract facts from this turn (small slot) → batched
//     conflict + relationship pass (medium slot) → commit candidates
//     and relationships. Wrapped in a 10s soft deadline per
//     CHAT-REARCH §"Memory Write Pipeline" — on miss, log + drop.
//  4. Async: maybeSummarize fires only when the verbatim window has
//     been exceeded. Compaction is now decoupled from extraction —
//     it summarizes but does NOT extract facts (per-turn extraction
//     covers that).
//
// When `overrides` is non-nil AND overrides.SkipCommit is set (ephemeral
// shards), this function returns immediately without writing anything.
// Persistent shards commit with their trusted-path FactProto shape.
func (p *Pipeline) commitAndExtract(ctx context.Context, sess *session.Session, userMsg, responseText string, loopMsgs []llm.Message, info *RouteInfo, overrides *ShardOverrides) {
	if overrides != nil && overrides.SkipCommit {
		return
	}
	// Never persist empty assistant turns. An empty responseText means the
	// upstream LLM returned either truly nothing (`{"role":"assistant"}`
	// with no content, no tool_calls) or its entire max_tokens budget was
	// eaten by extracted thinking. Either way, committing it as
	// conversation history would poison every subsequent request in the
	// session: the next outbound call to llama-server would include the
	// malformed assistant turn in messages[] and get rejected with HTTP
	// 400 "Assistant message must contain either 'content' or 'tool_calls'!",
	// which the gateway would then return to the caller as a 500.
	//
	// Skipping the commit here means the bad turn leaves no trace in the
	// in-memory session buffer or the memories table. The caller
	// (runTurn) still returns the empty responseText, so the user sees
	// an empty reply for that one request — recoverable — but the session
	// remains usable on the next request instead of being poisoned until
	// the gateway is restarted.
	if strings.TrimSpace(responseText) == "" {
		log.Printf("[pipeline] skip commit: empty assistant response for session %s (not persisting to session buffer or memories)", sess.ID)
		return
	}

	// Detach from the request context for the durable writes below.
	// By the time we're here the user has already seen the final SSE
	// token; a client that disconnects immediately after must NOT
	// cancel the fact commit or the intermediate-message persistence,
	// or the in-memory session history (which already has these
	// turns) and the DB would diverge. The per-op timeouts still
	// apply — they're layered on this detached base. The async
	// extract/summarize kicked off at the end spawn their own
	// contexts and are unaffected. See EXTERNAL-READINESS-REVIEW.md P1.
	ctx = context.WithoutCancel(ctx)

	// Identity for the conversation fact. Use sess.UserID() (canonical
	// id, falling back to the platform SenderID) — NOT sess.CanonicalID
	// directly, which is empty for CLI / scheduler / unresolved
	// identities. pgvector treats an empty/NULL user_id as
	// "visible to every user" (pgvector.go: `user_id IS NULL OR
	// user_id = $n`), so a fact written with an empty owner leaks
	// across tenants. This also matches the extraction path, which
	// already uses sess.UserID() (summarize.go). When there's no
	// identity at all, skip the durable commit rather than write a
	// globally-visible row. See EXTERNAL-READINESS-REVIEW.md P0.
	factUserID := sess.UserID()
	if factUserID == "" {
		log.Printf("[pipeline] skip fact commit for session %s: no resolved identity (would be globally visible)", sess.ID)
	} else {
		factContent := fmt.Sprintf("user: %s\nassistant: %s", userMsg, responseText)
		now := time.Now()
		fact := &pb.FactProto{
			Id:           uuid.NewString(),
			Content:      factContent,
			Embedding:    p.embedText(ctx, factContent),
			SourceType:   "conversation",
			Confidence:   1.0,
			Scope:        "session",
			CreatedAt:    timestamppb.New(now),
			LastAccessed: timestamppb.New(now),
			UserId:       factUserID,
			// ScopeTag carries the shard's scope through to pgvector when a
			// persistent shard commits a conversation fact. Empty on the
			// trusted path (nil overrides). Ephemeral shards never reach
			// here — SkipCommit short-circuits at the top of this function.
			ScopeTag: scopeTagFor(overrides),
			// ExcludeFromHot forces an isolated shard's conversation fact
			// past the engine's RAM cache so top-level retrieval can never
			// see it (FAMILIAR-SHARDS-PHASE1-FINDINGS Issue 3).
			ExcludeFromHot: excludeFromHotFor(overrides),
		}

		commitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if _, err := p.engine.CommitFacts(commitCtx, sess.ID, []*pb.FactProto{fact}); err != nil {
			log.Printf("[pipeline] CommitFacts error: %v", err)
		}
		cancel()
	}

	// Preserve the full turn shape in the in-memory session:
	//
	//   user → [assistant w/ tool_calls → tool result]* → assistant final
	//
	// Without the intermediate messages the next turn can't see the
	// tool plan the model built or the results it received, so it
	// has to rediscover identifiers it already figured out (the
	// "lost context across turns" issue Canyon hit). loopMsgs is
	// empty for non-tool turns; the user + final pair is identical
	// to the historical behavior in that case.
	sess.AddTurn("user", userMsg)
	for _, m := range loopMsgs {
		sess.AddMessage(session.Turn{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  marshalToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		})
	}
	sess.AddTurn("assistant", responseText)

	// Mirror the intermediate messages into the messages table so a
	// gateway restart's hydration path can replay them too. The
	// frontend continues to POST the user prompt + final assistant
	// text via /console/api/conversations/{id}/messages; this fills
	// in the middle. A non-UUID session id (Slack hash etc.) is a
	// no-op inside AppendIntermediateMessages.
	if p.conversations != nil && len(loopMsgs) > 0 {
		ims := make([]IntermediateMessage, 0, len(loopMsgs))
		for _, m := range loopMsgs {
			ims = append(ims, IntermediateMessage{
				Role:       m.Role,
				Content:    m.Content,
				ToolCalls:  marshalToolCalls(m.ToolCalls),
				ToolCallID: m.ToolCallID,
			})
		}
		appendCtx, cancelAppend := context.WithTimeout(ctx, 5*time.Second)
		if err := p.conversations.AppendIntermediateMessages(appendCtx, sess.ID, ims); err != nil {
			log.Printf("[pipeline] AppendIntermediateMessages error (continuing): %v", err)
		}
		cancelAppend()
	}

	// Snapshot retrieved rels for the post-turn extract pipeline before
	// info goes out of scope (the goroutine outlives the request ctx).
	var retrievedRels []memory.Relationship
	if info != nil {
		retrievedRels = info.RetrievedRelationships
	}
	p.kickoffPostTurnExtract(sess, userMsg, responseText, retrievedRels, overrides)
	p.maybeSummarize(sess, overrides)
}

// marshalToolCalls encodes an llm.ToolCall slice into the JSON
// shape stored on session.Turn.ToolCalls (and the messages table's
// tool_calls JSONB column). Returns nil for empty/missing slices so
// the storage row stays NULL instead of holding an empty array.
func marshalToolCalls(tcs []llm.ToolCall) []byte {
	if len(tcs) == 0 {
		return nil
	}
	b, err := json.Marshal(tcs)
	if err != nil {
		log.Printf("[pipeline] marshalToolCalls: %v (dropping tool_calls metadata)", err)
		return nil
	}
	return b
}

// unmarshalToolCalls is the inverse — called by flattenAssembled to
// turn the stored JSON back into the LLM-side slice when replaying
// turn history to the model.
func unmarshalToolCalls(raw []byte) []llm.ToolCall {
	if len(raw) == 0 {
		return nil
	}
	var tcs []llm.ToolCall
	if err := json.Unmarshal(raw, &tcs); err != nil {
		log.Printf("[pipeline] unmarshalToolCalls: %v (dropping historical tool_calls)", err)
		return nil
	}
	return tcs
}

// dedupeRelationships collapses exact (subject, predicate, object)
// duplicates so the one-hop and multi-hop layers don't produce
// redundant lines in the injected context block. Order is preserved
// so the higher-signal one-hop matches appear first.
func dedupeRelationships(rels []memory.Relationship) []memory.Relationship {
	if len(rels) <= 1 {
		return rels
	}
	seen := make(map[string]struct{}, len(rels))
	out := make([]memory.Relationship, 0, len(rels))
	for _, r := range rels {
		key := r.Subject + "|" + r.Predicate + "|" + r.Object
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}
