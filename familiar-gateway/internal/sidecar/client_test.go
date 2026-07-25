package sidecar

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/modelrole"
)

// fakeEndpoints satisfies EndpointResolver from an in-memory model→
// endpoint map so the routing tests don't need a live registry.
type fakeEndpoints struct {
	models map[string]string
}

func (f fakeEndpoints) EndpointForRole(role string) string { return "" }
func (f fakeEndpoints) EndpointForModel(id string) string  { return f.models[id] }

// newTestClient wires a Client from explicit role chains + a health map,
// mirroring how main.go composes the real resolver over the registry.
func newTestClient(chains map[string][]string, health map[string]string, endpoints map[string]string) *Client {
	res := modelrole.New(chains, func(id string) string { return health[id] })
	return NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{},
		fakeEndpoints{models: endpoints}, res)
}

// The sidecar task names MUST equal the config role names — the whole
// call-time resolution path relies on task == role. Lock it so a rename
// on either side fails loudly instead of silently unrouting every task.
func TestTaskNamesMatchConfigRoles(t *testing.T) {
	pairs := map[string]string{
		TaskClassify:      config.RoleClassify,
		TaskCondense:      config.RoleCondense,
		TaskExpandQueries: config.RoleExpandQueries,
		TaskExtract:       config.RoleExtract,
		TaskExtractLarge:  config.RoleExtractLarge,
		TaskSummarize:     config.RoleSummarize,
		TaskConflict:      config.RoleConflict,
		TaskRelationship:  config.RoleRelationship,
		TaskEntityGroup:   config.RoleEntityGroup,
	}
	for task, role := range pairs {
		if task != role {
			t.Errorf("sidecar task %q must equal config role %q", task, role)
		}
	}
}

func TestTaskResolvesToPrimaryEndpoint(t *testing.T) {
	c := newTestClient(
		map[string][]string{TaskClassify: {"fast"}},
		map[string]string{"fast": modelrole.StatusOnline},
		map[string]string{"fast": "http://127.0.0.1:8400"},
	)
	if got := c.TaskEndpoint(TaskClassify); got != "http://127.0.0.1:8400" {
		t.Fatalf("classify → %q, want fast endpoint", got)
	}
	if _, err := c.taskReady(TaskClassify); err != nil {
		t.Fatalf("healthy primary should be ready: %v", err)
	}
}

func TestTaskFailsOverToBackupEndpoint(t *testing.T) {
	c := newTestClient(
		map[string][]string{TaskClassify: {"fast", "spare"}},
		map[string]string{"fast": modelrole.StatusOffline, "spare": modelrole.StatusOnline},
		map[string]string{"fast": "http://127.0.0.1:8400", "spare": "http://127.0.0.1:8500"},
	)
	if got := c.TaskEndpoint(TaskClassify); got != "http://127.0.0.1:8500" {
		t.Fatalf("primary offline: classify → %q, want backup endpoint", got)
	}
	if _, err := c.taskReady(TaskClassify); err != nil {
		t.Fatalf("backup is online — task should be ready: %v", err)
	}
}

func TestTaskWholeChainOfflineUnavailable(t *testing.T) {
	c := newTestClient(
		map[string][]string{TaskClassify: {"fast", "spare"}},
		map[string]string{"fast": modelrole.StatusOffline, "spare": modelrole.StatusOffline},
		map[string]string{"fast": "http://127.0.0.1:8400", "spare": "http://127.0.0.1:8500"},
	)
	_, err := c.taskReady(TaskClassify)
	if err == nil || errors.Is(err, ErrNoModelConfigured) {
		t.Fatalf("whole chain offline should be an 'unavailable' error, got %v", err)
	}
}

func TestTaskUnconfiguredReturnsErrNoModel(t *testing.T) {
	c := newTestClient(map[string][]string{}, nil, nil)
	if _, err := c.taskReady(TaskClassify); !errors.Is(err, ErrNoModelConfigured) {
		t.Fatalf("unconfigured task err = %v, want ErrNoModelConfigured", err)
	}
	// A nil RoleResolver disables everything too.
	c2 := NewClient(config.SidecarConfig{Enabled: true}, config.RouterConfig{}, nil, nil)
	if _, err := c2.taskReady(TaskClassify); !errors.Is(err, ErrNoModelConfigured) {
		t.Fatalf("nil resolver err = %v, want ErrNoModelConfigured", err)
	}
}

func TestSharedEndpointSharesRouterAndGate(t *testing.T) {
	c := newTestClient(
		map[string][]string{
			TaskClassify: {"fast"},
			TaskCondense: {"fast"},
			TaskExtract:  {"capable"},
		},
		map[string]string{"fast": modelrole.StatusOnline, "capable": modelrole.StatusOnline},
		map[string]string{"fast": "http://127.0.0.1:8400", "capable": "http://127.0.0.1:8200"},
	)
	if c.routerFor(TaskClassify) != c.routerFor(TaskCondense) {
		t.Error("classify + condense resolve to the same endpoint — expected one shared router")
	}
	if c.gateForTask(TaskClassify) != c.gateForTask(TaskCondense) {
		t.Error("classify + condense should share a gate")
	}
	if c.gateForTask(TaskClassify) == c.gateForTask(TaskExtract) {
		t.Error("classify + extract are on distinct endpoints — gates must differ")
	}
}

func TestExtractLargeGetsLongTimeout(t *testing.T) {
	c := newTestClient(
		map[string][]string{TaskExtractLarge: {"big"}},
		map[string]string{"big": modelrole.StatusOnline},
		map[string]string{"big": "http://10.0.0.10:8080"},
	)
	r := c.routerFor(TaskExtractLarge)
	if r == nil {
		t.Fatal("extract_large should resolve to a router")
	}
	if r.client.Timeout != largeExtractTimeout {
		t.Errorf("extract_large router timeout = %v, want %v", r.client.Timeout, largeExtractTimeout)
	}
	// A critical-path task on a *different* endpoint keeps the short
	// ceiling — the long timeout is scoped to the large route's key.
	c2 := newTestClient(
		map[string][]string{TaskClassify: {"fast"}},
		map[string]string{"fast": modelrole.StatusOnline},
		map[string]string{"fast": "http://127.0.0.1:8400"},
	)
	if got := c2.routerFor(TaskClassify).client.Timeout; got == largeExtractTimeout {
		t.Errorf("classify router should not carry the large-extract ceiling, got %v", got)
	}
}

// End-to-end back-compat: a pre-roles [sidecar].*_model config, run
// through the real config loader, routes each task to the right model
// through the resolver — proving the legacy keys still work untouched.
func TestLegacySidecarConfigRoutesThroughRoles(t *testing.T) {
	cfg := writeAndLoadConfig(t, `
[[models]]
id = "sidecar/fast"
provider = "llama-server"
endpoint = "http://127.0.0.1:8400"

[[models]]
id = "sidecar/capable"
provider = "llama-server"
endpoint = "http://127.0.0.1:8200"

[sidecar]
enabled = true
default_model = "sidecar/fast"
extract_model = "sidecar/capable"
`)

	// The chains below come from the loader's own normalization — not
	// hand-built — so this fails if back-compat folding regresses.
	endpoints := map[string]string{}
	health := map[string]string{}
	for _, m := range cfg.Models {
		endpoints[m.ID] = m.Endpoint
		health[m.ID] = modelrole.StatusOnline
	}
	c := newTestClient(cfg.Roles.ResolvedChains(), health, endpoints)

	if got := c.TaskEndpoint(TaskClassify); got != "http://127.0.0.1:8400" {
		t.Errorf("classify → %q, want default_model (fast) endpoint", got)
	}
	if got := c.TaskEndpoint(TaskExtract); got != "http://127.0.0.1:8200" {
		t.Errorf("extract → %q, want extract_model (capable) endpoint", got)
	}
	if got := c.TaskEndpoint(TaskSummarize); got != "http://127.0.0.1:8400" {
		t.Errorf("summarize → %q, want default_model (fast) endpoint", got)
	}
}

// writeAndLoadConfig writes a gateway.toml into a temp dir and loads it
// through config.Load, so tests exercise the real normalization path.
func writeAndLoadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading test config: %v", err)
	}
	return cfg
}

func TestAvailableDefault(t *testing.T) {
	c := &Client{stopCh: make(chan struct{})}
	if c.Available() {
		t.Error("new client with no resolver should not be available")
	}
}

func TestAvailableTrueWhenATaskResolves(t *testing.T) {
	c := newTestClient(
		map[string][]string{TaskClassify: {"fast"}},
		map[string]string{"fast": modelrole.StatusOnline},
		map[string]string{"fast": "http://127.0.0.1:8400"},
	)
	if !c.Available() {
		t.Error("a resolvable, online task should make the client available")
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
