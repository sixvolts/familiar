package ctxbuild

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PromptTier describes how one routing complexity class should be prompted.
//
// The tier is selected by the classifier→tier mapping (trivial /
// knowledge / analytical / deep_reasoning) and drives three things:
// which overlay file the PromptStore concatenates onto base.md,
// whether tool schemas are injected into the LLM request, and the
// suggested thinking token budget. See FAMILIAR-TIERED-PROMPTS-SPEC.md.
type PromptTier struct {
	Name              string // canonical tier key used for file lookup
	OverlayFile       string // filename under the prompt dir
	InjectTools       bool   // gate for tool schema injection
	IncludeToolPolicy bool   // whether to append tool_policy.md
	ThinkingBudget    int    // suggested thinking tokens, 0 = disabled
	MaxWebSearches    int    // per-turn cap on web_search tool calls; 0 = unlimited
	MemoryConfig      TierMemoryConfig
	// ConvBudget caps how many tokens of conversation history the
	// engine returns for this tier. Trivial requests don't need
	// long history; deep-reasoning requests benefit from more
	// context. Pre-tier-aware defaults below were sized for a 32K
	// world; modern Qwen-122B at 262K can afford much more.
	// 0 falls back to a global default at the call site.
	ConvBudget int
	// MemBudget likewise caps memory tokens. Same falls-back-to-
	// global behavior at zero.
	MemBudget int
}

// TierMemoryConfig lets a tier override global memory retrieval settings.
// Zero values mean "use global default".
type TierMemoryConfig struct {
	Enabled       bool
	Threshold     float64
	MaxResults    int
	ExpandQueries bool
}

// tiers is the canonical tier table keyed by router complexity string.
var tiers = map[string]PromptTier{
	"trivial": {
		Name:              "trivial",
		OverlayFile:       "tier_trivial.md",
		InjectTools:       false,
		IncludeToolPolicy: false,
		ThinkingBudget:    0,
		MaxWebSearches:    1,
		MemoryConfig:      TierMemoryConfig{Enabled: false},
		ConvBudget:        2000, // ~1.5K words — short follow-ups don't need long history
		MemBudget:         1000,
	},
	"knowledge": {
		Name:              "knowledge",
		OverlayFile:       "tier_knowledge.md",
		InjectTools:       true,
		IncludeToolPolicy: true,
		ThinkingBudget:    400,
		MaxWebSearches:    2,
		MemoryConfig:      TierMemoryConfig{Enabled: true, ExpandQueries: true},
		ConvBudget:        12000,
		MemBudget:         3000,
	},
	"analytical": {
		Name:              "reasoning",
		OverlayFile:       "tier_reasoning.md",
		InjectTools:       true,
		IncludeToolPolicy: true,
		ThinkingBudget:    1500,
		MaxWebSearches:    5,
		MemoryConfig:      TierMemoryConfig{Enabled: true, Threshold: 0.45, MaxResults: 10, ExpandQueries: true},
		ConvBudget:        32000,
		MemBudget:         6000,
	},
	"deep_reasoning": {
		Name:              "deep",
		OverlayFile:       "tier_deep.md",
		InjectTools:       true,
		IncludeToolPolicy: true,
		ThinkingBudget:    4000,
		MaxWebSearches:    10,
		MemoryConfig:      TierMemoryConfig{Enabled: true, Threshold: 0.40, MaxResults: 15, ExpandQueries: true},
		ConvBudget:        64000,
		MemBudget:         10000,
	},
}

// TierFor maps a router complexity string to its PromptTier. Unknown values
// fall back to "knowledge", which is the safe middle ground — it still
// injects tools and memory but doesn't burn a large thinking budget.
func TierFor(complexity string) PromptTier {
	if t, ok := tiers[complexity]; ok {
		return t
	}
	return tiers["knowledge"]
}

// PromptStore holds the parsed contents of a tiered-prompt directory.
//
// Files are loaded at construction and cached. Callers that want
// hot-reload should call MaybeReload() before Assemble — the store
// re-stats each file at most once per reloadCooldown and rereads
// only when mtime changed. Concurrent reads are safe; reloads
// take a write lock for the duration of the reread.
//
// CHAT-REARCH §"Smaller Hardening" — when a file's content actually
// changes (not just mtime touched), the reload logs a warning. Useful
// for correlating "why did the model behavior change mid-session?"
// with prompt edits.
type PromptStore struct {
	dir        string
	base       string
	toolPolicy string
	overlays   map[string]string // keyed by overlay filename
	fallback   string            // monolithic prompt used when dir lookup fails

	// baseOverride is an admin-supplied replacement for the base
	// layer, sourced from instance_settings.system_prompt_base and
	// seeded at boot. When non-empty it replaces base.md in
	// Assemble; tier overlays + tool_policy still apply on top.
	// Empty = use the file-loaded base. Held under mu like the
	// other mutable fields.
	baseOverride string

	mu sync.RWMutex

	// mtime tracking. Keyed by full file path. Zero value means
	// "never loaded" — used to distinguish first-load from reload
	// for the warning log.
	mtimes        map[string]time.Time
	lastStatCheck time.Time
}

// reloadCooldown bounds how often MaybeReload re-stats files. Prompt
// edits during a live session are rare; paying for an os.Stat on
// every turn would be wasteful. The window is small enough that an
// operator editing a prompt sees their change inside one chat.
const reloadCooldown = 5 * time.Second

// NewPromptStore loads base.md, tier_*.md, and tool_policy.md from dir.
//
// If dir is empty or unreadable, the store still works but every Assemble
// call returns the fallback string. The fallback is typically the legacy
// monolithic system_prompt.md so existing deployments keep working when
// the prompts dir is missing.
func NewPromptStore(dir, fallback string) (*PromptStore, error) {
	ps := &PromptStore{
		dir:      dir,
		overlays: make(map[string]string),
		fallback: strings.TrimSpace(fallback),
		mtimes:   make(map[string]time.Time),
	}
	if dir == "" {
		return ps, nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		// Directory missing — fall back silently. Caller decides whether
		// to log.
		return ps, nil
	}

	ps.base = ps.readAndStamp("base.md")
	ps.toolPolicy = ps.readAndStamp("tool_policy.md")

	// Load every tier overlay referenced by the tier table.
	for _, t := range tiers {
		if t.OverlayFile == "" {
			continue
		}
		if _, ok := ps.overlays[t.OverlayFile]; ok {
			continue
		}
		ps.overlays[t.OverlayFile] = ps.readAndStamp(t.OverlayFile)
	}

	if ps.base == "" {
		return ps, fmt.Errorf("ctxbuild: base.md not found or empty in %s", dir)
	}
	return ps, nil
}

// readAndStamp reads dir/name (returning "" on error) and records the
// file's mtime. Caller holds whatever lock is appropriate; constructor
// runs single-threaded, MaybeReload holds the write lock.
func (s *PromptStore) readAndStamp(name string) string {
	path := filepath.Join(s.dir, name)
	info, err := os.Stat(path)
	if err != nil {
		delete(s.mtimes, path)
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s.mtimes[path] = info.ModTime()
	return strings.TrimSpace(string(data))
}

// MaybeReload re-stats every tracked prompt file at most once per
// reloadCooldown and rereads any whose mtime advanced. When a file's
// content changes, logs a warning so operators can correlate
// mid-session prompt edits with model behavior shifts.
//
// Safe to call from the chat hot path; the cooldown keeps the syscall
// rate bounded.
func (s *PromptStore) MaybeReload() {
	if s == nil || s.dir == "" {
		return
	}
	s.mu.RLock()
	cooldownPassed := time.Since(s.lastStatCheck) >= reloadCooldown
	s.mu.RUnlock()
	if !cooldownPassed {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Recheck under the lock — another goroutine may have just reloaded.
	if time.Since(s.lastStatCheck) < reloadCooldown {
		return
	}
	s.lastStatCheck = time.Now()

	type tracked struct {
		name string
		set  func(string)
	}
	files := []tracked{
		{"base.md", func(v string) { s.base = v }},
		{"tool_policy.md", func(v string) { s.toolPolicy = v }},
	}
	for _, t := range tiers {
		if t.OverlayFile == "" {
			continue
		}
		name := t.OverlayFile
		files = append(files, tracked{name, func(v string) { s.overlays[name] = v }})
	}

	for _, f := range files {
		path := filepath.Join(s.dir, f.name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		prev, hadPrev := s.mtimes[path]
		if hadPrev && !info.ModTime().After(prev) {
			continue
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		f.set(strings.TrimSpace(string(data)))
		s.mtimes[path] = info.ModTime()
		if hadPrev {
			log.Printf("[promptstore] %s reloaded (mtime changed); active sessions will see the new content on their next turn", f.name)
		}
	}
}

// Loaded reports whether the store has a usable base prompt — either
// the file-loaded base.md or an admin override. Callers use this to
// decide whether to log the fallback path.
func (s *PromptStore) Loaded() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.base != "" || s.baseOverride != ""
}

// SetBaseOverride installs (or, with an empty string, clears) an
// admin-supplied base prompt that replaces the file-loaded base.md
// layer. Tier overlays + tool_policy still apply on top either way.
// Called at boot to seed from instance_settings, and again whenever
// an admin saves the system prompt — the change takes effect on the
// next Assemble with no gateway restart.
func (s *PromptStore) SetBaseOverride(text string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseOverride = strings.TrimSpace(text)
}

// HasBaseOverride reports whether an admin override is currently in
// effect (vs the file-loaded default).
func (s *PromptStore) HasBaseOverride() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.baseOverride != ""
}

// EffectiveBase returns the base layer Assemble will use — the admin
// override when set, otherwise the file-loaded base.md. The admin
// system-prompt editor + the optional user-facing viewer both render
// this.
func (s *PromptStore) EffectiveBase() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.baseOverride != "" {
		return s.baseOverride
	}
	return s.base
}

// Assemble returns the full system prompt for the given tier: base.md plus
// the tier's overlay plus (optionally) tool_policy.md. Sections are joined
// with a blank line. If the store has no base prompt loaded, Assemble
// returns the fallback string unchanged.
func (s *PromptStore) Assemble(tier PromptTier) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	// The admin override replaces the file base when set; tier
	// overlays + tool_policy still layer on top.
	effectiveBase := s.base
	if s.baseOverride != "" {
		effectiveBase = s.baseOverride
	}
	if effectiveBase == "" {
		return s.fallback
	}

	parts := []string{effectiveBase}
	if overlay := s.overlays[tier.OverlayFile]; overlay != "" {
		parts = append(parts, overlay)
	}
	if tier.IncludeToolPolicy && s.toolPolicy != "" {
		parts = append(parts, s.toolPolicy)
	}
	return strings.Join(parts, "\n\n")
}
