package sidecar

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/config"
)

// Sidecar task names. Each is a discrete unit of sidecar work the
// operator can point at a specific model via [sidecar].<task>_model.
// SIDECAR-SLOT-FIXES.md replaced the opaque small/medium/small_async
// "slot" abstraction with this explicit task → model mapping.
const (
	TaskClassify      = "classify"
	TaskCondense      = "condense"
	TaskExpandQueries = "expand_queries"
	TaskExtract       = "extract"
	TaskExtractLarge  = "extract_large" // big-model route for large documents (§research)
	TaskSummarize     = "summarize"
	TaskConflict      = "conflict"
	TaskRelationship  = "relationship"
	TaskEntityGroup   = "entity_group"
)

// allTasks is the canonical ordered task list — used for resolution,
// startup logging, and so the log output is stable run to run.
var allTasks = []string{
	TaskClassify, TaskCondense, TaskExpandQueries,
	TaskExtract, TaskExtractLarge, TaskSummarize, TaskConflict,
	TaskRelationship, TaskEntityGroup,
}

// largeExtractTimeout is the HTTP request ceiling for the extract_large
// route. A big model (qwen3.5-122b) reading a 5–12K-char research
// write-up in one pass runs minutes, not the 10s the small tasks use.
// It's a detached, best-effort post-delivery pass, so a generous
// ceiling costs nothing on the user's path.
const largeExtractTimeout = 5 * time.Minute

// Critical-path tasks (classify, condense, expand_queries) block
// time-to-first-token — they run before the model can start
// generating, so their methods syncEnter the endpoint's gate to take
// priority over background work sharing the same model. The remaining
// tasks are background / post-turn and acquireAsync instead. The
// distinction is applied per-method (see Classify vs ExtractFacts),
// not from a lookup table.

// ErrNoModelConfigured is returned by a Client method whose task has
// no model assigned (its role chain names no model). Callers treat it
// like any other sidecar miss — the feature the task powers is an
// optimization, never required.
var ErrNoModelConfigured = errors.New("sidecar: no model configured for this task")

// Client manages the connection to the GPU sidecar services.
//
// ROLE-FAILOVER: each sidecar task name IS a role name (classify,
// condense, extract, …). The Client resolves a task to a live model ID
// on every call by walking that role's primary→backup→fallback chain
// and skipping any candidate the shared model registry reports offline,
// then turns the model ID into an endpoint. There is no frozen route
// table and no separate sidecar health loop — health comes from the one
// registry heartbeat that also drives chat failover and maintenance
// mode, so a task follows its role's failover the moment a probe
// condemns the primary.
type Client struct {
	cfg  config.SidecarConfig
	rCfg config.RouterConfig

	// roles resolves a task/role name to a live model ID + health;
	// endpoints turns a model ID into its HTTP endpoint. A nil roles
	// resolver disables every task (taskReady → ErrNoModelConfigured).
	roles     RoleResolver
	endpoints EndpointResolver

	mu sync.Mutex
	// routers caches one HTTPRouter per endpoint (extract_large's
	// long-timeout router is cached under "large:"+endpoint); gates
	// cache one sync/async gate per endpoint. Both are lazily built
	// as tasks resolve, and reused across tasks that land on the same
	// endpoint.
	routers map[string]*HTTPRouter
	gates   map[string]*slotGate

	stopOnce sync.Once
	stopCh   chan struct{}
}

// EndpointResolver turns a model ID into the HTTP endpoint of the
// [[models]] entry that carries it. *router.Registry satisfies this.
//
// EndpointForModel("") returns "". A nil resolver is tolerated (every
// lookup yields "") so tests can construct a Client without a registry.
//
// EndpointForRole is retained for the (now config-normalized) role tags
// but is no longer consulted on the routing path — task→model→endpoint
// resolution goes through the RoleResolver + EndpointForModel.
type EndpointResolver interface {
	EndpointForRole(role string) string
	EndpointForModel(modelID string) string
}

// RoleResolver resolves a role name (== a sidecar task name) to the
// live model ID to use, walking the role's failover chain and skipping
// offline candidates, and reports a model's current health. It is the
// same resolver that drives chat failover. *modelrole.Resolver
// satisfies it. A nil RoleResolver disables every sidecar task.
type RoleResolver interface {
	Resolve(role string) (modelID string, tier int, ok bool)
	Status(modelID string) string
	Chain(role string) []string
}

// NewClient creates a sidecar client. The task→model chains come from
// [roles] (config.normalizeRoles folds the legacy [sidecar].*_model
// keys, role= tags, and router_endpoint into them at load), so the
// client itself holds no routing config — it resolves every task
// through the RoleResolver on each call. Start() is a no-op kept for
// caller symmetry; health is driven by the shared registry heartbeat.
func NewClient(sidecarCfg config.SidecarConfig, routerCfg config.RouterConfig, endpoints EndpointResolver, roles RoleResolver) *Client {
	return &Client{
		cfg:       sidecarCfg,
		rCfg:      routerCfg,
		roles:     roles,
		endpoints: endpoints,
		routers:   make(map[string]*HTTPRouter),
		gates:     make(map[string]*slotGate),
		stopCh:    make(chan struct{}),
	}
}

// endpointFor resolves a model ID to its endpoint via the injected
// EndpointResolver, tolerating a nil resolver.
func (c *Client) endpointFor(modelID string) string {
	if modelID == "" || c.endpoints == nil {
		return ""
	}
	return c.endpoints.EndpointForModel(modelID)
}

// resolveTask walks a task's role chain to the model that should serve
// it right now and returns (modelID, endpoint, err):
//   - ErrNoModelConfigured when the role names no model (or resolves to
//     an endpoint the registry doesn't know) — callers treat this like
//     any sidecar miss (the feature it powers is an optimization).
//   - a non-nil "unavailable" error when every candidate in the chain is
//     currently offline (distinct from unconfigured so callers can tell
//     "not set up" from "down right now").
func (c *Client) resolveTask(task string) (modelID, endpoint string, err error) {
	if c.roles == nil {
		return "", "", ErrNoModelConfigured
	}
	modelID, _, ok := c.roles.Resolve(task)
	if !ok || modelID == "" {
		return "", "", ErrNoModelConfigured
	}
	endpoint = c.endpointFor(modelID)
	if endpoint == "" {
		return "", "", ErrNoModelConfigured
	}
	// Resolve already prefers a non-offline candidate; a returned model
	// still marked offline means the whole chain is down.
	if c.roles.Status(modelID) == "offline" {
		return modelID, endpoint, fmt.Errorf("sidecar: %s: model %s (%s) offline", task, modelID, endpoint)
	}
	return modelID, endpoint, nil
}

// routerForEndpoint returns the cached HTTPRouter for an endpoint,
// building it on first use. extract_large gets its own long-timeout
// router (cached under a distinct key) so co-locating it on a shared
// endpoint doesn't hand a critical-path task the 5-minute ceiling.
func (c *Client) routerForEndpoint(endpoint string, large bool) *HTTPRouter {
	key := endpoint
	if large {
		key = "large:" + endpoint
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.routers[key]; r != nil {
		return r
	}
	var r *HTTPRouter
	if large {
		r = NewHTTPRouterWithTimeout(endpoint, largeExtractTimeout)
	} else {
		r = NewHTTPRouter(endpoint)
	}
	c.routers[key] = r
	return r
}

// LogRouting prints, per task, the configured role chain and the model
// it resolves to right now, so the operator can verify failover routing
// without reverse-engineering the config.
func (c *Client) LogRouting() {
	log.Printf("[sidecar] task routing (task → chain → live model):")
	for _, task := range allTasks {
		chain := ""
		if c.roles != nil {
			chain = strings.Join(c.roles.Chain(task), " → ")
		}
		switch modelID, ep, err := c.resolveTask(task); {
		case err == nil:
			log.Printf("[sidecar]   %-14s [%s] → %s (%s)", task, chain, modelID, ep)
		case errors.Is(err, ErrNoModelConfigured):
			log.Printf("[sidecar]   %-14s → (skipped — no model configured)", task)
		default:
			log.Printf("[sidecar]   %-14s [%s] → (currently unavailable: %v)", task, chain, err)
		}
	}
}

// routerFor returns the HTTPRouter for a task's currently-resolved
// endpoint regardless of health, or nil when the task names no model.
// Used by callers that only need the wired router, not a readiness gate.
func (c *Client) routerFor(task string) *HTTPRouter {
	if c.roles == nil {
		return nil
	}
	modelID, _, ok := c.roles.Resolve(task)
	if !ok || modelID == "" {
		return nil
	}
	ep := c.endpointFor(modelID)
	if ep == "" {
		return nil
	}
	return c.routerForEndpoint(ep, task == TaskExtractLarge)
}

// TaskEndpoint returns the currently-resolved HTTP endpoint for a task,
// or "" when the task is unconfigured. Used by callers that need a raw
// endpoint URL outside the routed-method path — e.g. the pipeline's
// preamble generator, which posts directly to /v1/chat/completions.
func (c *Client) TaskEndpoint(task string) string {
	if _, ep, err := c.resolveTask(task); err == nil {
		return ep
	}
	// Even when the resolved model is offline, hand back the endpoint so
	// the preamble path can still attempt it (best-effort, like before).
	if c.roles != nil {
		if modelID, _, ok := c.roles.Resolve(task); ok {
			return c.endpointFor(modelID)
		}
	}
	return ""
}

// taskReady resolves a task to its router and confirms the resolved
// model is not offline. Returns (nil, ErrNoModelConfigured) when the
// task names no model, or (nil, error) when its whole chain is down.
func (c *Client) taskReady(task string) (*HTTPRouter, error) {
	_, ep, err := c.resolveTask(task)
	if err != nil {
		return nil, err
	}
	return c.routerForEndpoint(ep, task == TaskExtractLarge), nil
}

// Start is a no-op retained for caller symmetry. Health is driven by
// the shared model registry's heartbeat (router.Registry.
// StartHealthChecks), which probes every sidecar task model along with
// the chat models — there is no separate sidecar health loop anymore.
func (c *Client) Start(ctx context.Context) {}

// Stop shuts down the client.
func (c *Client) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

// Available reports whether at least one sidecar task currently
// resolves to a reachable (non-offline) model.
func (c *Client) Available() bool {
	if c.roles == nil {
		return false
	}
	for _, task := range allTasks {
		if _, _, err := c.resolveTask(task); err == nil {
			return true
		}
	}
	return false
}

// Summarize produces a rolling summary of a conversation using the
// sidecar. Returns prevSummary unchanged when the summarize task is
// unconfigured or its endpoint is down.
func (c *Client) Summarize(ctx context.Context, prevSummary string, turns []Turn) (string, error) {
	r, err := c.taskReady(TaskSummarize)
	if err != nil {
		return prevSummary, err
	}
	return r.Summarize(ctx, prevSummary, turns)
}

// ExtractFacts asks the sidecar to extract discrete facts and
// relationship triples from a set of turns in one LLM call. Async
// post-turn work — yields to any in-flight critical-path call that
// shares its endpoint via the per-endpoint sync gate.
func (c *Client) ExtractFacts(ctx context.Context, turns []Turn) (ExtractionResult, error) {
	r, err := c.taskReady(TaskExtract)
	if err != nil {
		return ExtractionResult{}, err
	}
	if gate := c.gateForTask(TaskExtract); gate != nil {
		if err := gate.acquireAsync(ctx); err != nil {
			return ExtractionResult{}, err
		}
		defer gate.releaseAsync()
	}
	return r.ExtractFacts(ctx, turns)
}

// ExtractFactsLarge routes extraction of a large document to the
// extract_large model (a bigger model that holds the whole doc in
// context). Falls back to the normal extract route when extract_large
// isn't configured or its endpoint is unhealthy, so callers can always
// use it for big content without branching on config.
func (c *Client) ExtractFactsLarge(ctx context.Context, turns []Turn) (ExtractionResult, error) {
	r, err := c.taskReady(TaskExtractLarge)
	if err != nil {
		return c.ExtractFacts(ctx, turns)
	}
	if gate := c.gateForTask(TaskExtractLarge); gate != nil {
		if err := gate.acquireAsync(ctx); err != nil {
			return ExtractionResult{}, err
		}
		defer gate.releaseAsync()
	}
	return r.ExtractFacts(ctx, turns)
}

// ExtractRelationshipsFromFacts mines entity-relationship triples
// from a batch of existing memory facts.
func (c *Client) ExtractRelationshipsFromFacts(ctx context.Context, facts []string) ([]ExtractedRelationship, error) {
	r, err := c.taskReady(TaskRelationship)
	if err != nil {
		return nil, err
	}
	return r.ExtractRelationshipsFromFacts(ctx, facts)
}

// GroupEntities clusters a list of noisy entity names into alias
// groups for the entity-resolution pass.
func (c *Client) GroupEntities(ctx context.Context, names []string) ([]EntityGroup, error) {
	r, err := c.taskReady(TaskEntityGroup)
	if err != nil {
		return nil, err
	}
	return r.GroupEntities(ctx, names)
}

// BatchClassifyAndRelate runs the post-turn conflict-resolution +
// relationship-extraction pass in one call. Routed via the conflict
// task — conflict resolution is the gating concern, and in a typical
// config conflict + relationship point at the same capable model.
func (c *Client) BatchClassifyAndRelate(ctx context.Context, in BatchExtractInput) (BatchExtractResult, error) {
	r, err := c.taskReady(TaskConflict)
	if err != nil {
		return BatchExtractResult{}, err
	}
	return r.BatchClassifyAndRelate(ctx, in)
}

// ClassifyConflict classifies the relationship between an existing
// fact and a newly-extracted one. Callers treat an error as a signal
// to fall back to ADD — the sleep cycle reconciles later.
func (c *Client) ClassifyConflict(ctx context.Context, existing, incoming string) (string, error) {
	r, err := c.taskReady(TaskConflict)
	if err != nil {
		return "", err
	}
	return r.ClassifyConflict(ctx, existing, incoming)
}

// ExpandQueries decomposes a user message into multiple targeted
// memory search queries. Critical-path: enters the sync gate so it
// takes priority over background work sharing its endpoint.
func (c *Client) ExpandQueries(ctx context.Context, userMsg string) ([]string, error) {
	r, err := c.taskReady(TaskExpandQueries)
	if err != nil {
		return nil, err
	}
	if gate := c.gateForTask(TaskExpandQueries); gate != nil {
		gate.syncEnter()
		defer gate.syncExit()
	}
	return r.ExpandQueries(ctx, userMsg)
}

// CondenseQuery rewrites a mid-conversation user message into a
// self-contained retrieval query. Critical-path: enters the sync
// gate. Returns the raw message + error on any miss.
func (c *Client) CondenseQuery(ctx context.Context, history []Turn, userMsg string) (string, error) {
	r, err := c.taskReady(TaskCondense)
	if err != nil {
		return userMsg, err
	}
	if gate := c.gateForTask(TaskCondense); gate != nil {
		gate.syncEnter()
		defer gate.syncExit()
	}
	return r.CondenseQuery(ctx, history, userMsg)
}

// GenerateTitle asks the sidecar for a 1-3 word title for a new chat
// from its opening exchange. Routed to the classify task — the fast
// small model is exactly right for a tiny one-shot prompt. Returns an
// error when the classify task is unconfigured or its endpoint is
// down; callers keep their existing derived title on any failure.
func (c *Client) GenerateTitle(ctx context.Context, userMsg, assistantMsg string) (string, error) {
	r, err := c.taskReady(TaskClassify)
	if err != nil {
		return "", err
	}
	return r.GenerateTitle(ctx, userMsg, assistantMsg)
}

// Embed is unimplemented — the pipeline's MakeEmbedder() handles HTTP
// embedding directly against the [embedder] endpoint. Kept for
// interface compatibility.
func (c *Client) Embed(ctx context.Context, text string) (*EmbedResult, error) {
	return nil, fmt.Errorf("sidecar embed: use pipeline embedder with [embedder] endpoint config instead")
}

// gateForTask returns the sync/async gate guarding a task's currently-
// resolved endpoint, building it on first use. Critical-path tasks
// syncEnter it; the async extract task acquireAsync it. Tasks that
// resolve to the same endpoint share a gate and contend; tasks on
// distinct endpoints never do. Returns nil when the task is
// unconfigured or its chain is down.
func (c *Client) gateForTask(task string) *slotGate {
	_, ep, err := c.resolveTask(task)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.gates[ep]
	if g == nil {
		g = &slotGate{}
		c.gates[ep] = g
	}
	return g
}

func slotStateString(s SlotState) string {
	switch s {
	case SlotReady:
		return "ready"
	case SlotLoading:
		return "loading"
	case SlotError:
		return "error"
	case SlotUnloading:
		return "unloading"
	default:
		return "unknown"
	}
}
