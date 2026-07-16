package sidecar

import (
	"errors"
	"testing"

	"github.com/familiar/gateway/internal/config"
)

// fakeResolver satisfies EndpointResolver from in-memory maps so the
// routing tests don't need a live registry.
type fakeResolver struct {
	roles  map[string]string // role  → endpoint
	models map[string]string // model → endpoint
}

func (f fakeResolver) EndpointForRole(role string) string { return f.roles[role] }
func (f fakeResolver) EndpointForModel(id string) string  { return f.models[id] }

// routeEndpoint is a test helper: the endpoint a task resolved to,
// or "" when the task is unconfigured.
func routeEndpoint(c *Client, task string) string {
	if r, ok := c.routes[task]; ok {
		return r.endpoint
	}
	return ""
}

// Explicit per-task assignment: each *_model key resolves through the
// registry to its model's endpoint.
func TestExplicitTaskRouting(t *testing.T) {
	resolver := fakeResolver{
		models: map[string]string{
			"sidecar/fast":    "http://127.0.0.1:8400",
			"sidecar/capable": "http://127.0.0.1:8200",
		},
	}
	cfg := config.SidecarConfig{
		Enabled:       true,
		ClassifyModel: "sidecar/fast",
		CondenseModel: "sidecar/fast",
		ExtractModel:  "sidecar/capable",
	}
	c := NewClient(cfg, config.RouterConfig{}, resolver)

	if got := routeEndpoint(c, TaskClassify); got != "http://127.0.0.1:8400" {
		t.Errorf("classify → %q, want fast endpoint", got)
	}
	if got := routeEndpoint(c, TaskCondense); got != "http://127.0.0.1:8400" {
		t.Errorf("condense → %q, want fast endpoint", got)
	}
	if got := routeEndpoint(c, TaskExtract); got != "http://127.0.0.1:8200" {
		t.Errorf("extract → %q, want capable endpoint", got)
	}
	// Unassigned tasks (no *_model, no default_model) are skipped.
	if routeEndpoint(c, TaskSummarize) != "" {
		t.Error("summarize should be unconfigured")
	}
	if c.routerFor(TaskSummarize) != nil {
		t.Error("routerFor(summarize) should be nil")
	}
	// Tasks on the same endpoint share one HTTPRouter + one gate.
	if c.routes[TaskClassify].router != c.routes[TaskCondense].router {
		t.Error("classify + condense share an endpoint — expected the same router")
	}
	if c.gateForTask(TaskClassify) != c.gateForTask(TaskCondense) {
		t.Error("classify + condense should share a gate")
	}
	if c.gateForTask(TaskClassify) == c.gateForTask(TaskExtract) {
		t.Error("classify + extract are on distinct endpoints — gates must differ")
	}
}

// default_model backs every task with no explicit *_model key.
func TestDefaultModelFallback(t *testing.T) {
	resolver := fakeResolver{
		models: map[string]string{
			"sidecar/fast":    "http://127.0.0.1:8400",
			"sidecar/capable": "http://127.0.0.1:8200",
		},
	}
	cfg := config.SidecarConfig{
		Enabled:      true,
		DefaultModel: "sidecar/fast",
		ExtractModel: "sidecar/capable", // one explicit override
	}
	c := NewClient(cfg, config.RouterConfig{}, resolver)

	for _, task := range []string{TaskClassify, TaskCondense, TaskExpandQueries, TaskSummarize} {
		if got := routeEndpoint(c, task); got != "http://127.0.0.1:8400" {
			t.Errorf("%s → %q, want default (fast) endpoint", task, got)
		}
	}
	if got := routeEndpoint(c, TaskExtract); got != "http://127.0.0.1:8200" {
		t.Errorf("extract override → %q, want capable endpoint", got)
	}
}

// extract_large is an additive route: it binds the big-model endpoint
// without disabling the other tasks' fallback, gets a generous request
// ceiling, and stays unbound (so ExtractFactsLarge falls back to the
// extract route) when unconfigured.
func TestExtractLargeRouting(t *testing.T) {
	resolver := fakeResolver{
		models: map[string]string{
			"sidecar/fast": "http://127.0.0.1:8400",
			"gpu-host/big": "http://10.0.0.10:8080",
		},
	}

	t.Run("bound additively without breaking default fallback", func(t *testing.T) {
		cfg := config.SidecarConfig{
			Enabled:           true,
			DefaultModel:      "sidecar/fast",
			ExtractLargeModel: "gpu-host/big",
		}
		c := NewClient(cfg, config.RouterConfig{}, resolver)

		if got := routeEndpoint(c, TaskExtractLarge); got != "http://10.0.0.10:8080" {
			t.Errorf("extract_large → %q, want big endpoint", got)
		}
		// Setting only extract_large must NOT strand the other tasks:
		// default_model still backs them.
		if got := routeEndpoint(c, TaskExtract); got != "http://127.0.0.1:8400" {
			t.Errorf("extract → %q, want default (fast) endpoint — extract_large must not disable fallback", got)
		}
		// The big-model router carries the generous ceiling, not 10s.
		r := c.routerFor(TaskExtractLarge)
		if r == nil {
			t.Fatal("extract_large should have a router")
		}
		if r.client.Timeout != largeExtractTimeout {
			t.Errorf("extract_large router timeout = %v, want %v", r.client.Timeout, largeExtractTimeout)
		}
	})

	t.Run("unconfigured stays unbound", func(t *testing.T) {
		cfg := config.SidecarConfig{Enabled: true, DefaultModel: "sidecar/fast"}
		c := NewClient(cfg, config.RouterConfig{}, resolver)
		if routeEndpoint(c, TaskExtractLarge) != "" {
			t.Error("extract_large should be unconfigured when extract_large_model is unset")
		}
	})

	t.Run("typo resolves to no endpoint, stays unbound", func(t *testing.T) {
		cfg := config.SidecarConfig{
			Enabled:           true,
			DefaultModel:      "sidecar/fast",
			ExtractLargeModel: "gpu-host/typo",
		}
		c := NewClient(cfg, config.RouterConfig{}, resolver)
		if routeEndpoint(c, TaskExtractLarge) != "" {
			t.Error("extract_large with an unknown model must stay unbound (falls back to extract)")
		}
	})
}

// With no *_model / default_model keys, the legacy role resolution
// runs: critical-path → role=small, background → role=medium.
func TestLegacyRoleFallback(t *testing.T) {
	resolver := fakeResolver{
		roles: map[string]string{
			config.ModelSlotSmall:  "http://127.0.0.1:8400",
			config.ModelSlotMedium: "http://127.0.0.1:8200",
		},
	}
	c := NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{}, resolver)

	for _, task := range []string{TaskClassify, TaskCondense, TaskExpandQueries} {
		if got := routeEndpoint(c, task); got != "http://127.0.0.1:8400" {
			t.Errorf("%s → %q, want small-role endpoint", task, got)
		}
	}
	for _, task := range []string{TaskSummarize, TaskConflict, TaskRelationship, TaskEntityGroup} {
		if got := routeEndpoint(c, task); got != "http://127.0.0.1:8200" {
			t.Errorf("%s → %q, want medium-role endpoint", task, got)
		}
	}
}

// Legacy fallback with only role=small configured: medium-group tasks
// fall through to the small endpoint so a single-instance deploy
// keeps working.
func TestLegacyRoleFallbackMediumFallsToSmall(t *testing.T) {
	resolver := fakeResolver{
		roles: map[string]string{config.ModelSlotSmall: "http://127.0.0.1:8400"},
	}
	c := NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{}, resolver)

	if got := routeEndpoint(c, TaskSummarize); got != "http://127.0.0.1:8400" {
		t.Errorf("summarize → %q, want fall-through to small endpoint", got)
	}
}

// Legacy fallback also honors the literal router_endpoint when no
// model carries a role (pre-MODEL-ROLES configs).
func TestLegacyRouterEndpointFallback(t *testing.T) {
	cfg := config.SidecarConfig{Enabled: true, RouterEndpoint: "http://127.0.0.1:9000"}
	c := NewClient(cfg, config.RouterConfig{}, nil)
	if got := routeEndpoint(c, TaskClassify); got != "http://127.0.0.1:9000" {
		t.Errorf("classify → %q, want literal router_endpoint", got)
	}
}

// An unconfigured task's taskReady returns ErrNoModelConfigured so
// callers can distinguish "not set up" from "endpoint down".
func TestTaskReadyUnconfigured(t *testing.T) {
	c := NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{}, nil)
	if _, err := c.taskReady(TaskClassify); !errors.Is(err, ErrNoModelConfigured) {
		t.Errorf("taskReady(classify) err = %v, want ErrNoModelConfigured", err)
	}
}

// A configured task whose endpoint hasn't passed a health probe
// returns an "unavailable" error — distinct from ErrNoModelConfigured.
func TestTaskReadyConfiguredButUnhealthy(t *testing.T) {
	cfg := config.SidecarConfig{Enabled: true, RouterEndpoint: "http://127.0.0.1:9000"}
	c := NewClient(cfg, config.RouterConfig{}, nil)
	_, err := c.taskReady(TaskClassify)
	if err == nil || errors.Is(err, ErrNoModelConfigured) {
		t.Errorf("taskReady(classify) err = %v, want a non-nil 'unavailable' error", err)
	}
}

func TestAvailableDefault(t *testing.T) {
	c := &Client{stopCh: make(chan struct{})}
	if c.Available() {
		t.Error("new client should not be available")
	}
}

func TestSlotStateString(t *testing.T) {
	tests := []struct {
		state SlotState
		want  string
	}{
		{SlotReady, "ready"},
		{SlotLoading, "loading"},
		{SlotError, "error"},
		{SlotUnloading, "unloading"},
		{SlotUnknown, "unknown"},
	}
	for _, tt := range tests {
		if got := slotStateString(tt.state); got != tt.want {
			t.Errorf("slotStateString(%d) = %q, want %q", tt.state, got, tt.want)
		}
	}
}
