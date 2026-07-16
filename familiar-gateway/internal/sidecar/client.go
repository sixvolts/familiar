package sidecar

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
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

// criticalPathTasks block time-to-first-token: they run before the
// model can start generating. They enter their endpoint's sync gate
// so a slow background call yields to them when the two share a
// model. The remaining tasks are background / post-turn.
var criticalPathTasks = map[string]bool{
	TaskClassify:      true,
	TaskCondense:      true,
	TaskExpandQueries: true,
}

// ErrNoModelConfigured is returned by a Client method whose task has
// no model assigned (no <task>_model, no default_model, no legacy
// role fallback). Callers treat it like any other sidecar miss —
// the feature the task powers is an optimization, never required.
var ErrNoModelConfigured = errors.New("sidecar: no model configured for this task")

// taskRoute is a resolved task → model → endpoint binding.
type taskRoute struct {
	modelID  string
	endpoint string
	router   *HTTPRouter
}

// Client manages the connection to the GPU sidecar services.
//
// SIDECAR-SLOT-FIXES.md: each sidecar task (classify, condense,
// extract, …) is assigned a model ID via [sidecar].<task>_model,
// falling back to default_model, falling back to the legacy
// role-tagged endpoints. The Client resolves each task to an
// HTTPRouter once at construction; methods just look up their task.
type Client struct {
	cfg  config.SidecarConfig
	rCfg config.RouterConfig

	// routes maps a task name to its resolved binding. A missing key
	// means the task is unconfigured — its method returns
	// ErrNoModelConfigured.
	routes map[string]taskRoute
	// gates serialize background (async) work against critical-path
	// (sync) work, keyed by endpoint. Two tasks on the same model
	// share a gate; tasks on distinct models never contend.
	gates map[string]*slotGate

	mu        sync.RWMutex
	healthy   map[string]bool // endpoint → last health-probe result
	available bool            // any task endpoint healthy

	stopOnce sync.Once
	stopCh   chan struct{}
}

// EndpointResolver lets the sidecar Client resolve a model ID — or a
// legacy role tag — to the HTTP endpoint of the [[models]] entry that
// carries it. *router.Registry satisfies this; wire it in main.go
// after the registry is built.
//
// EndpointForModel("") and EndpointForRole("") return "". A nil
// resolver is tolerated (every lookup yields "") so tests can
// construct a Client without a registry.
type EndpointResolver interface {
	EndpointForRole(role string) string
	EndpointForModel(modelID string) string
}

// NewClient creates a sidecar client. Call Start() to begin health
// checking.
//
// Task → endpoint resolution, per task, in order:
//  1. [sidecar].<task>_model  → resolver.EndpointForModel
//  2. [sidecar].default_model → resolver.EndpointForModel
//  3. Legacy role fallback (only when NO *_model / default_model key
//     is set anywhere): critical-path tasks → role="small", extract
//     → role="small_async" (then medium, then small), the remaining
//     background tasks → role="medium" (then small). Role endpoints
//     fall back to the literal sidecar.router_endpoint.
//  4. Unresolved → task skipped; its method returns
//     ErrNoModelConfigured.
func NewClient(sidecarCfg config.SidecarConfig, routerCfg config.RouterConfig, resolver EndpointResolver) *Client {
	c := &Client{
		cfg:     sidecarCfg,
		rCfg:    routerCfg,
		routes:  make(map[string]taskRoute),
		gates:   make(map[string]*slotGate),
		healthy: make(map[string]bool),
		stopCh:  make(chan struct{}),
	}

	endpointForModel := func(id string) string {
		if id == "" || resolver == nil {
			return ""
		}
		return resolver.EndpointForModel(id)
	}

	// extract_large is bound additively (in either routing mode) so it
	// never disables the other tasks' fallback: setting only it must not
	// leave classify/extract/… unbound. Unset → the route stays absent
	// and ExtractFactsLarge falls back to the normal extract route.
	if sidecarCfg.ExtractLargeModel != "" {
		if ep := endpointForModel(sidecarCfg.ExtractLargeModel); ep != "" {
			c.bindTask(TaskExtractLarge, sidecarCfg.ExtractLargeModel, ep)
		}
	}

	if sidecarCfg.HasExplicitTaskModels() {
		// Explicit task → model assignment (with default_model
		// fallback already applied by SidecarTaskModels).
		for _, tm := range sidecarCfg.SidecarTaskModels() {
			ep := endpointForModel(tm.Model)
			if ep == "" {
				continue // task skipped — logged by LogRouting
			}
			c.bindTask(tm.Task, tm.Model, ep)
		}
	} else {
		// Legacy role fallback — no explicit task assignment present.
		smallEP, mediumEP, asyncEP := "", "", ""
		if resolver != nil {
			smallEP = resolver.EndpointForRole(config.ModelSlotSmall)
			mediumEP = resolver.EndpointForRole(config.ModelSlotMedium)
			asyncEP = resolver.EndpointForRole(config.ModelSlotSmallAsync)
		}
		if smallEP == "" {
			smallEP = sidecarCfg.RouterEndpoint
		}
		if mediumEP == "" {
			mediumEP = smallEP
		}
		if asyncEP == "" {
			asyncEP = mediumEP
		}
		legacy := map[string]string{
			TaskClassify:      smallEP,
			TaskCondense:      smallEP,
			TaskExpandQueries: smallEP,
			TaskExtract:       asyncEP,
			TaskSummarize:     mediumEP,
			TaskConflict:      mediumEP,
			TaskRelationship:  mediumEP,
			TaskEntityGroup:   mediumEP,
		}
		for _, task := range allTasks {
			if ep := legacy[task]; ep != "" {
				c.bindTask(task, "(legacy role)", ep)
			}
		}
	}

	return c
}

// bindTask wires a task to an endpoint, de-duplicating HTTPRouters
// and sync/async gates by endpoint so tasks sharing a model also
// share one client + one gate.
func (c *Client) bindTask(task, modelID, endpoint string) {
	gate := c.gates[endpoint]
	var router *HTTPRouter
	for _, r := range c.routes {
		if r.endpoint == endpoint {
			router = r.router
			break
		}
	}
	if router == nil {
		// extract_large reads a whole multi-KB document in one pass on a
		// big model — far past the default 10s ceiling. The timeout is
		// per-endpoint (routers de-dup by endpoint), which is correct
		// when the big model has its own endpoint (the intended setup);
		// don't co-locate a critical-path task on it or it inherits this
		// ceiling.
		if task == TaskExtractLarge {
			router = NewHTTPRouterWithTimeout(endpoint, largeExtractTimeout)
		} else {
			router = NewHTTPRouter(endpoint)
		}
	}
	if gate == nil {
		gate = &slotGate{}
		c.gates[endpoint] = gate
	}
	c.routes[task] = taskRoute{modelID: modelID, endpoint: endpoint, router: router}
}

// LogRouting prints the resolved task → model → endpoint table at
// startup so the operator can verify routing without reverse-
// engineering the config. Unconfigured tasks are listed as skipped.
func (c *Client) LogRouting() {
	log.Printf("[sidecar] task routing:")
	for _, task := range allTasks {
		if r, ok := c.routes[task]; ok {
			log.Printf("[sidecar]   %-14s → %s  (%s)", task, r.modelID, r.endpoint)
		} else {
			log.Printf("[sidecar]   %-14s → (skipped — no model configured)", task)
		}
	}
}

// routerFor returns the HTTPRouter for a task, or nil when the task
// has no model assigned.
func (c *Client) routerFor(task string) *HTTPRouter {
	if r, ok := c.routes[task]; ok {
		return r.router
	}
	return nil
}

// TaskEndpoint returns the resolved HTTP endpoint for a task, or ""
// when the task is unconfigured. Used by callers that need a raw
// endpoint URL outside the routed-method path — e.g. the pipeline's
// preamble generator, which posts directly to /v1/chat/completions.
func (c *Client) TaskEndpoint(task string) string {
	if r, ok := c.routes[task]; ok {
		return r.endpoint
	}
	return ""
}

// endpointHealthy reports the last health-probe result for an
// endpoint. Unknown endpoints (never probed) read as false.
func (c *Client) endpointHealthy(endpoint string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.healthy[endpoint]
}

// taskReady resolves a task to its router and confirms the endpoint
// is healthy. Returns (nil, error) when the task is unconfigured or
// its endpoint is currently unreachable.
func (c *Client) taskReady(task string) (*HTTPRouter, error) {
	r, ok := c.routes[task]
	if !ok || r.router == nil {
		return nil, ErrNoModelConfigured
	}
	if !c.endpointHealthy(r.endpoint) {
		return nil, fmt.Errorf("sidecar: %s endpoint %s unavailable", task, r.endpoint)
	}
	return r.router, nil
}

// distinctEndpoints returns the unique set of endpoints across all
// configured tasks — the set the health loop probes.
func (c *Client) distinctEndpoints() []string {
	seen := make(map[string]struct{})
	for _, r := range c.routes {
		seen[r.endpoint] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for ep := range seen {
		out = append(out, ep)
	}
	sort.Strings(out)
	return out
}

// Start begins the health check loop in the background.
func (c *Client) Start(ctx context.Context) {
	go c.healthLoop(ctx)
}

// Stop shuts down the client.
func (c *Client) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
		c.mu.Lock()
		c.available = false
		c.mu.Unlock()
	})
}

// Available reports whether at least one sidecar task endpoint is
// currently reachable.
func (c *Client) Available() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.available
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

// gateForTask returns the sync/async gate guarding a task's endpoint.
// Critical-path tasks syncEnter it; the async extract task
// acquireAsync it. Tasks on distinct endpoints get distinct gates and
// never contend. Returns nil when the task is unconfigured.
func (c *Client) gateForTask(task string) *slotGate {
	r, ok := c.routes[task]
	if !ok {
		return nil
	}
	return c.gates[r.endpoint]
}

// healthLoop periodically probes every distinct task endpoint.
func (c *Client) healthLoop(ctx context.Context) {
	retryInterval := time.Duration(c.cfg.RetryIntervalSecs) * time.Second
	if retryInterval == 0 {
		retryInterval = 10 * time.Second
	}

	c.checkHealth(ctx)

	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.checkHealth(ctx)
		}
	}
}

// checkHealth probes each distinct task endpoint and updates the
// per-endpoint health map. available is true when any endpoint
// responds.
func (c *Client) checkHealth(ctx context.Context) {
	endpoints := c.distinctEndpoints()
	if len(endpoints) == 0 {
		return
	}

	// Probe outside the lock — one router per endpoint, reused.
	results := make(map[string]bool, len(endpoints))
	for _, ep := range endpoints {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := NewHTTPRouter(ep).HealthCheck(checkCtx)
		cancel()
		results[ep] = err == nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	anyUp := false
	for ep, up := range results {
		was := c.healthy[ep]
		if up && !was {
			log.Printf("[sidecar] endpoint %s available", ep)
		} else if !up && was {
			log.Printf("[sidecar] endpoint %s became unavailable", ep)
		}
		c.healthy[ep] = up
		if up {
			anyUp = true
		}
	}
	c.available = anyUp
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
