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
	TaskClassify, TaskExpandQueries,
	TaskExtract, TaskExtractLarge, TaskSummarize, TaskConflict,
	TaskRelationship, TaskEntityGroup,
}

// LargeExtractTimeout is the HTTP request ceiling for the extract_large
// route. A big model (qwen3.5-122b) reading a 5–12K-char research
// write-up in one pass runs minutes, not the 10s the small tasks use.
// It's a detached, best-effort post-delivery pass, so a generous
// ceiling costs nothing on the user's path.
const LargeExtractTimeout = 5 * time.Minute

// Critical-path tasks (classify, expand_queries) block time-to-first-
// token — they run before the model can start generating, so their
// methods syncEnter the endpoint's gate to take priority over
// background work sharing the same model. Every other task is
// background work (post-turn extraction and summarizing, titles, the
// memory backfills) and runs through background(): one at a time per
// endpoint, and never while a critical-path call is in flight. Only
// extraction used to take the gate, so a post-turn batch or summary
// could hold the slot and push the next turn's classify into its
// timeout.

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
	cfg config.SidecarConfig

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
}

// EndpointResolver turns a model ID into the HTTP endpoint of the
// [[models]] entry that carries it. *router.Registry satisfies this.
//
// EndpointForModel("") returns "". A nil resolver is tolerated (every
// lookup yields "") so tests can construct a Client without a registry.
type EndpointResolver interface {
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
// through the RoleResolver on each call. Health is driven by the shared
// registry heartbeat.
func NewClient(sidecarCfg config.SidecarConfig, endpoints EndpointResolver, roles RoleResolver) *Client {
	return &Client{
		cfg:       sidecarCfg,
		roles:     roles,
		endpoints: endpoints,
		routers:   make(map[string]*HTTPRouter),
		gates:     make(map[string]*slotGate),
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

// routerForEndpoint returns the cached HTTPRouter for an endpoint and
// request model name, building it on first use. Routers are keyed by
// both, so a failover to another model on the same server sends that
// model's name; the slot gate stays per endpoint (gateForTask), since
// two models on one server still share its slot. extract_large gets its
// own long-timeout router so co-locating it on a shared endpoint doesn't
// hand a critical-path task the 5-minute ceiling.
func (c *Client) routerForEndpoint(endpoint, model string, large bool) *HTTPRouter {
	key := endpoint + "|" + model
	if large {
		key = "large:" + key
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.routers[key]; r != nil {
		return r
	}
	var r *HTTPRouter
	if large {
		r = NewHTTPRouterWithTimeout(endpoint, LargeExtractTimeout)
	} else {
		r = NewHTTPRouterWithTimeout(endpoint, c.taskTimeout())
	}
	r.model = model
	c.routers[key] = r
	return r
}

// defaultTaskTimeout is the request ceiling for the small sidecar tasks
// when [sidecar].request_timeout_ms is unset.
const defaultTaskTimeout = 10 * time.Second

// taskTimeout is the HTTP ceiling for every task but extract_large:
// [sidecar].request_timeout_ms, as documented, or 10s. It used to be a
// fixed 10s that only the classifier's own (tighter) deadline read the
// setting for, so raising it for a slower extract model did nothing.
func (c *Client) taskTimeout() time.Duration {
	if c != nil && c.cfg.RequestTimeoutMs > 0 {
		return time.Duration(c.cfg.RequestTimeoutMs) * time.Millisecond
	}
	return defaultTaskTimeout
}

// RequestModelNamer is implemented by an EndpointResolver that knows
// each model's configured request name (*router.Registry does).
type RequestModelNamer interface {
	RequestModelFor(modelID string) string
}

// requestModelFor is the name requests for modelID send as `model`: the
// configured name when the endpoint resolver is a RequestModelNamer,
// else the id without its namespace (the rule chat uses too).
func (c *Client) requestModelFor(modelID string) string {
	if r, ok := c.endpoints.(RequestModelNamer); ok {
		return r.RequestModelFor(modelID)
	}
	return config.StripModelNamespace(modelID)
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

// TaskTarget is the endpoint and model name to send for a task, for
// callers that post to a task's endpoint themselves (the preamble
// generator). Resolved on every call, so it follows the task's failover;
// ("", "") when the task has no model or its whole chain is offline.
func (c *Client) TaskTarget(task string) (endpoint, model string) {
	if c == nil {
		return "", ""
	}
	modelID, ep, err := c.resolveTask(task)
	if err != nil {
		return "", ""
	}
	return ep, c.requestModelFor(modelID)
}

// taskReady resolves a task to its router and confirms the resolved
// model is not offline. Returns (nil, ErrNoModelConfigured) when the
// task names no model, or (nil, error) when its whole chain is down.
func (c *Client) taskReady(task string) (*HTTPRouter, error) {
	modelID, ep, err := c.resolveTask(task)
	if err != nil {
		return nil, err
	}
	return c.routerForEndpoint(ep, c.requestModelFor(modelID), task == TaskExtractLarge), nil
}

// Summarize produces a rolling summary of a conversation using the
// sidecar. Returns prevSummary unchanged when the summarize task is
// unconfigured or its endpoint is down.
func (c *Client) Summarize(ctx context.Context, prevSummary string, turns []Turn) (string, error) {
	r, err := c.taskReady(TaskSummarize)
	if err != nil {
		return prevSummary, err
	}
	out := prevSummary
	err = c.background(ctx, TaskSummarize, func() (err error) {
		out, err = r.Summarize(ctx, prevSummary, turns)
		return err
	})
	return out, err
}

// ExtractFacts asks the sidecar to extract discrete facts and
// relationship triples from a set of turns in one LLM call. Async
// post-turn work — yields to any in-flight critical-path call that
// shares its endpoint via the per-endpoint sync gate.
func (c *Client) ExtractFacts(ctx context.Context, turns []Turn) (ExtractionResult, error) {
	return c.ExtractFactsWithContext(ctx, turns, nil)
}

// ExtractFactsWithContext is ExtractFacts with a read-only prior-turns block
// for reference resolution (see HTTPRouter.ExtractFactsWithContext). Same
// small-slot routing + async gate as ExtractFacts.
func (c *Client) ExtractFactsWithContext(ctx context.Context, turns, context []Turn) (ExtractionResult, error) {
	r, err := c.taskReady(TaskExtract)
	if err != nil {
		return ExtractionResult{}, err
	}
	var out ExtractionResult
	err = c.background(ctx, TaskExtract, func() (err error) {
		out, err = r.ExtractFactsWithContext(ctx, turns, context)
		return err
	})
	return out, err
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
	var out ExtractionResult
	err = c.background(ctx, TaskExtractLarge, func() (err error) {
		out, err = r.ExtractFactsLarge(ctx, turns)
		return err
	})
	return out, err
}

// ExtractRelationshipsFromFacts mines entity-relationship triples
// from a batch of existing memory facts.
func (c *Client) ExtractRelationshipsFromFacts(ctx context.Context, facts []string) ([]ExtractedRelationship, error) {
	r, err := c.taskReady(TaskRelationship)
	if err != nil {
		return nil, err
	}
	var out []ExtractedRelationship
	err = c.background(ctx, TaskRelationship, func() (err error) {
		out, err = r.ExtractRelationshipsFromFacts(ctx, facts)
		return err
	})
	return out, err
}

// GroupEntities clusters a list of noisy entity names into alias
// groups for the entity-resolution pass.
func (c *Client) GroupEntities(ctx context.Context, names []string) ([]EntityGroup, error) {
	r, err := c.taskReady(TaskEntityGroup)
	if err != nil {
		return nil, err
	}
	var out []EntityGroup
	err = c.background(ctx, TaskEntityGroup, func() (err error) {
		out, err = r.GroupEntities(ctx, names)
		return err
	})
	return out, err
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
	var out BatchExtractResult
	err = c.background(ctx, TaskConflict, func() (err error) {
		out, err = r.BatchClassifyAndRelate(ctx, in)
		return err
	})
	return out, err
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
	var out string
	err = c.background(ctx, TaskClassify, func() (err error) {
		out, err = r.GenerateTitle(ctx, userMsg, assistantMsg)
		return err
	})
	return out, err
}

// background runs fn as background work on task's endpoint: after any
// critical-path call in flight there, and one background call at a time
// (see slot_gate.go). Waiting for the gate counts against ctx.
func (c *Client) background(ctx context.Context, task string, fn func() error) error {
	if gate := c.gateForTask(task); gate != nil {
		if err := gate.acquireAsync(ctx); err != nil {
			return err
		}
		defer gate.releaseAsync()
	}
	return fn()
}

// Embedding does not route through the sidecar. It resolves through the
// [roles.embedder] chain to a provider="embeddings" [[models]] entry
// (see llm.EmbeddingsProvider), which puts it under the shared heartbeat
// and gives it primary/backup failover. The former Client.Embed stub
// here always returned an error, so [memory].use_sidecar_embedder
// silently fell through to the HTTP embedder on every call; both are
// gone.

// gateForTask returns the sync/async gate guarding a task's currently-
// resolved endpoint, building it on first use. Critical-path tasks
// syncEnter it; background tasks acquireAsync it (see background). Tasks that
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
