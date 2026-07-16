// Package skills defines the Familiar skill framework: a modular tool
// abstraction that lets native Go skills, and eventually MCP servers and
// WASM plugins, expose LLM-callable tools through a single Registry.
//
// Phase 2 ships with native Go skills only (search, weather, ...). The
// interface is designed so later phases can add transport-backed skills
// behind the same Skill contract. See FAMILIAR-PHASE2-SPEC.md §4 for the
// overall design.
//
// Skills are registered once at gateway startup and looked up by the
// scheduler or the LLM tool-call loop at runtime. Registry methods are
// safe for concurrent use.
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
)

// Skill is one cohesive module that exposes one or more LLM-callable tools.
//
// Implementations are typically constructed with a typed New() function
// taking dependencies (HTTP client, API keys, etc.) directly; Init is a
// lifecycle hook for transports that want to defer configuration until
// after construction (MCP handshake, WASM module load, etc.). Native Go
// skills generally return nil from Init.
//
// Tool names must be globally unique across all registered skills. The
// Registry enforces this at registration time.
type Skill interface {
	// Name is the human-readable skill identifier ("weather", "search").
	Name() string

	// Description is a one-line summary used in logs and introspection.
	Description() string

	// Version lets the registry (and, later, MCP clients) track schema
	// changes. Semantic-version strings are preferred but not enforced.
	Version() string

	// Tools returns every tool this skill exposes, with JSON Schema
	// parameter definitions suitable for OpenAI-style function calling.
	Tools() []ToolDefinition

	// Execute dispatches one tool call. toolName is guaranteed by the
	// Registry to be a name this skill declared in Tools(). params is
	// the raw JSON object the caller passed; implementations unmarshal
	// it into a typed struct.
	Execute(ctx context.Context, toolName string, params json.RawMessage) (ToolResult, error)

	// Init is called once by the Registry after Register, before any
	// Execute. cfg may be nil — native skills typically ignore it.
	Init(cfg json.RawMessage) error

	// Close releases any resources the skill holds (open HTTP
	// connections, background goroutines, caches). Called by
	// Registry.Close during gateway shutdown.
	Close() error
}

// ToolDefinition is the LLM-facing description of a single callable tool.
//
// Parameters is a raw JSON object matching the OpenAI function-calling
// schema format (JSON Schema draft 2020-12, the "parameters" field). It
// is stored as json.RawMessage so skills can write their schema as a
// literal blob without round-tripping through a typed struct.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolResult is the outcome of one tool execution.
//
// Content is the human-/LLM-readable text the model should reason about.
// Data is optional structured output for programmatic consumers (the
// scheduler, for instance, may prefer structured weather data over the
// prose summary). Tokens is the caller's estimate of Content's token
// cost, primarily for the ctxbuild tool-result budget. Error is set when
// the tool ran but produced a user-facing failure; transport errors
// (network, panic, etc.) return a Go error from Execute instead.
type ToolResult struct {
	Content string          `json:"content"`
	Data    json.RawMessage `json:"data,omitempty"`
	Tokens  int             `json:"tokens"`
	Cached  bool            `json:"cached"`
	Error   string          `json:"error,omitempty"`
}

// Registry is the skill container wired into the gateway at startup.
// Callers register concrete Skill implementations, then dispatch tool
// calls by tool name without knowing which skill owns the tool.
type Registry struct {
	mu     sync.RWMutex
	skills map[string]Skill  // name → skill
	tools  map[string]string // tool name → owning skill name
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{
		skills: make(map[string]Skill),
		tools:  make(map[string]string),
	}
}

// Register installs a skill and indexes every tool it exposes. It fails
// if the skill name is already registered or any of its tool names
// collide with an existing tool — collisions are almost always a wiring
// bug, and failing loudly at startup is better than dispatching to the
// wrong skill at runtime.
//
// If the skill's Init returns an error, it is not added to the registry.
func (r *Registry) Register(s Skill) error {
	if s == nil {
		return fmt.Errorf("skills: nil skill")
	}
	name := s.Name()
	if name == "" {
		return fmt.Errorf("skills: skill has empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.skills[name]; exists {
		return fmt.Errorf("skills: %q already registered", name)
	}

	// Check every tool name before we mutate anything, so a partial
	// collision doesn't leave the registry in a half-registered state.
	tools := s.Tools()
	for _, t := range tools {
		if t.Name == "" {
			return fmt.Errorf("skills: %q exposes a tool with empty name", name)
		}
		if owner, exists := r.tools[t.Name]; exists {
			return fmt.Errorf("skills: tool %q already owned by skill %q", t.Name, owner)
		}
	}

	if err := s.Init(nil); err != nil {
		return fmt.Errorf("skills: init %q: %w", name, err)
	}

	r.skills[name] = s
	for _, t := range tools {
		r.tools[t.Name] = name
	}
	return nil
}

// Get returns a registered skill by name.
func (r *Registry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// SkillNames returns the registered skill names in alphabetical order.
// Used mainly by startup logging and introspection endpoints.
func (r *Registry) SkillNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.skills))
	for n := range r.skills {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ToolDefinitions returns every tool across every registered skill, sorted
// by tool name so the LLM sees a stable ordering across requests (prompt
// caching, debugging, etc.). The Registry owns the slice — callers should
// not mutate it.
func (r *Registry) ToolDefinitions() []ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var all []ToolDefinition
	for _, s := range r.skills {
		all = append(all, s.Tools()...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all
}

// FilterToolDefinitions returns the subset of registered tool
// definitions whose names appear in the allowlist. Ordering matches
// ToolDefinitions (alphabetical). An allowlist entry that is not a
// registered tool is dropped with a warn-level log — shards created
// against a tool that has since been removed from the gateway keep
// invoking, they just don't see the missing tool. A nil or empty
// allowlist returns a nil slice.
//
// This is the canonical way for shard-aware callers (the shard
// invocation path, the admin UI's "what is this shard allowed to do"
// introspection) to narrow the tool set. Keeping the logic here means
// every caller gets the same unknown-tool handling.
func (r *Registry) FilterToolDefinitions(allowlist []string) []ToolDefinition {
	if len(allowlist) == 0 {
		return nil
	}
	r.mu.RLock()
	byName := make(map[string]ToolDefinition)
	for _, s := range r.skills {
		for _, t := range s.Tools() {
			byName[t.Name] = t
		}
	}
	r.mu.RUnlock()

	out := make([]ToolDefinition, 0, len(allowlist))
	for _, name := range allowlist {
		if t, ok := byName[name]; ok {
			out = append(out, t)
			continue
		}
		log.Printf("[skills] FilterToolDefinitions: allowlisted tool %q is not registered — skipping", name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// KnownToolNames returns a set-shaped lookup of every registered tool
// name. Exported for shard validation (ValidateAllowlist) so admin
// handlers can reject shards that reference removed tools at save
// time, before any invocation hits runToolLoop.
func (r *Registry) KnownToolNames() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]bool, len(r.tools))
	for name := range r.tools {
		out[name] = true
	}
	return out
}

// Execute dispatches a tool call to the owning skill. Returns an error
// if no skill owns the tool.
//
// Each dispatch is logged at INFO with the tool name, owning skill,
// arg byte count, and outcome. Per FAMILIAR-SHARDS-PHASE1-FINDINGS
// the tool loop's silent-success failure mode (model claims a tool
// fired but the registry never saw it) is the worst kind of bug to
// debug from the outside; this gives `grep [skills]` a useful trail.
func (r *Registry) Execute(ctx context.Context, toolName string, params json.RawMessage) (ToolResult, error) {
	r.mu.RLock()
	skillName, ok := r.tools[toolName]
	var s Skill
	if ok {
		s = r.skills[skillName]
	}
	r.mu.RUnlock()

	if !ok || s == nil {
		log.Printf("[skills] dispatch rejected: unknown tool %q (params=%dB)", toolName, len(params))
		return ToolResult{}, fmt.Errorf("skills: unknown tool %q", toolName)
	}
	log.Printf("[skills] dispatch: tool=%s skill=%s params=%dB", toolName, skillName, len(params))
	result, err := s.Execute(ctx, toolName, params)
	switch {
	case err != nil:
		log.Printf("[skills] dispatch failed: tool=%s err=%v", toolName, err)
	case result.Error != "":
		log.Printf("[skills] dispatch error-result: tool=%s err=%q", toolName, result.Error)
	default:
		log.Printf("[skills] dispatch ok: tool=%s content_len=%d", toolName, len(result.Content))
	}
	return result, err
}

// Close calls Close on every registered skill. The first error is
// returned, but every skill's Close is still invoked so partial shutdown
// leaks are minimised.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for name, s := range r.skills {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("skills: close %q: %w", name, err)
		}
	}
	return firstErr
}
