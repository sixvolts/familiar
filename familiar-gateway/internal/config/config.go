package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level gateway configuration.
type Config struct {
	Engine       EngineConfig       `toml:"engine"`
	Node         NodeConfig         `toml:"node"`
	Instance     InstanceConfig     `toml:"instance"`
	Identity     IdentityConfig     `toml:"identity"`
	Models       []ModelConfig      `toml:"models"`
	Router       RouterConfig       `toml:"router"`
	Sidecar      SidecarConfig      `toml:"sidecar"`
	Memory       MemoryConfig       `toml:"memory"`
	Rerank       RerankConfig       `toml:"rerank"`
	Pipeline     PipelineConfig     `toml:"pipeline"`
	Context      ContextConfig      `toml:"context"`
	Adapter      AdapterConfig      `toml:"adapter"`
	Embedder     EmbedderConfig     `toml:"embedder"`
	SystemPrompt SystemPromptConfig `toml:"system_prompt"`
	Tools        ToolsConfig        `toml:"tools"`
	Skills       SkillsConfig       `toml:"skills"`
	Media        MediaConfig        `toml:"media"`
	Admin        AdminConfig        `toml:"admin"`
	Effort       EffortConfig       `toml:"effort"`
	// Sleep / consolidation cycle.
	// Read only when [engine] mode = "inprocess". DefaultConfig()
	// applies DefaultSleepConfig() so the block is optional.
	Sleep SleepConfig `toml:"sleep"`

	// Public-link sharing (notes / wiki pages). Empty PublicHosts
	// disables share serving entirely — the toggle endpoint and the
	// public /p/{key} render both refuse. Restricting by Host header
	// is how a deployment that fronts the gateway behind both a
	// public hostname and a Tailscale-direct hostname keeps shares
	// from rendering on the private side.
	Sharing SharingConfig `toml:"sharing"`

	// Web Push (PWA notifications). Empty VAPID keys disable push
	// entirely — the subscribe endpoints + the `push` delivery target
	// both no-op. Generate a keypair once (the gateway logs a fresh one
	// at startup if unset) and put it here; the private key is a secret.
	Push PushConfig `toml:"push"`
}

// PushConfig holds the VAPID keypair the Web Push subsystem signs with
// and the contact subject (a mailto:/https: URL push services may use to
// reach the operator about abuse). Keys are base64url (raw) as produced
// by webpush-go's GenerateVAPIDKeys. Push is OFF unless both are set.
type PushConfig struct {
	VAPIDPublicKey  string `toml:"vapid_public_key"`
	VAPIDPrivateKey string `toml:"vapid_private_key"`
	// Subject is the VAPID "sub" — a mailto: or https: URL. Defaults to
	// a placeholder mailto when blank.
	Subject string `toml:"subject"`
}

// Enabled reports whether Web Push is configured (both VAPID keys set).
func (p PushConfig) Enabled() bool {
	return p.VAPIDPublicKey != "" && p.VAPIDPrivateKey != ""
}

// SharingConfig is the public-link sharing policy. PublicHosts is the
// allow-list of inbound Host headers on which /p/{key} renders;
// PublicBaseURL is the base the toggle endpoint hands the frontend
// for composing copy-link URLs (e.g. "https://host-a.familiar.wiki").
type SharingConfig struct {
	PublicHosts   []string `toml:"public_hosts"`
	PublicBaseURL string   `toml:"public_base_url"`
}

// EffortConfig translates the classifier's ordinal effort levels
// into concrete budgets — token counts, top-k, max-searches.
// Zero-valued levels fall back to package defaults; absent levels
// behave identically to defaults. The config side of CHAT-REARCH
// §"Effort Level Configuration".
//
// All fields are optional; the gateway boots with reasonable
// defaults if [effort.*] is not supplied at all.
type EffortConfig struct {
	Thinking    EffortThinkingConfig    `toml:"thinking"`
	MemoryDepth EffortMemoryDepthConfig `toml:"memory_depth"`
	SearchDepth EffortSearchDepthConfig `toml:"search_depth"`
}

// EffortThinkingConfig — token budgets per ThinkingLevel. The "off"
// level disables thinking; the others scale a max-tokens budget.
type EffortThinkingConfig struct {
	Off    EffortThinkingLevel `toml:"off"`
	Low    EffortThinkingLevel `toml:"low"`
	Medium EffortThinkingLevel `toml:"medium"`
	High   EffortThinkingLevel `toml:"high"`
}

// EffortThinkingLevel is one row of [effort.thinking.<level>].
type EffortThinkingLevel struct {
	Enabled     *bool `toml:"enabled"` // nil = use level-default
	TokenBudget int   `toml:"token_budget"`
}

// EffortMemoryDepthConfig — retrieval knobs per MemoryDepth.
type EffortMemoryDepthConfig struct {
	None    EffortMemoryDepthLevel `toml:"none"`
	Shallow EffortMemoryDepthLevel `toml:"shallow"`
	Deep    EffortMemoryDepthLevel `toml:"deep"`
}

// EffortMemoryDepthLevel is one row of [effort.memory_depth.<level>].
type EffortMemoryDepthLevel struct {
	Skip                bool    `toml:"skip"`
	TopK                int     `toml:"top_k"`
	SimilarityThreshold float64 `toml:"similarity_threshold"`
}

// EffortSearchDepthConfig — web-search budgets per SearchDepth.
type EffortSearchDepthConfig struct {
	None    EffortSearchDepthLevel `toml:"none"`
	Shallow EffortSearchDepthLevel `toml:"shallow"`
	Deep    EffortSearchDepthLevel `toml:"deep"`
}

// EffortSearchDepthLevel is one row of [effort.search_depth.<level>].
type EffortSearchDepthLevel struct {
	Skip        bool `toml:"skip"`
	MaxSearches int  `toml:"max_searches"`
}

// IdentityConfig is operator-curated identity bootstrap data: which
// platform IDs map to which canonical users on this deployment. Was
// previously hardcoded as a schema migration; now sourced from config
// so portability and re-deploys don't carry one operator's data.
//
// Seeds run on every gateway startup via internal/identity.Bootstrap;
// inserts are idempotent (ON CONFLICT DO NOTHING) so re-runs are
// safe and operator edits propagate without manual SQL.
type IdentityConfig struct {
	Seed []IdentitySeed `toml:"seed"`
}

// IdentitySeed is one mapping from (platform, platform_id) to a
// canonical user. Mirrors the identity_map table shape minus the
// timestamp.
type IdentitySeed struct {
	Platform    string `toml:"platform"`     // "slack", "openai", "cli", ...
	PlatformID  string `toml:"platform_id"`  // platform-native ID
	CanonicalID string `toml:"canonical_id"` // canonical user this maps to
	DisplayName string `toml:"display_name"` // optional human label
}

// InstanceConfig describes this deployment so the `instance` skill can
// answer user questions like "how do I register", "where's the admin
// console", or "who do I contact for account issues". All fields are
// optional — when every field is empty the skill is not registered at
// all, so the tool won't appear in the LLM's toolbox on a fresh box
// until operators fill in their values.
type InstanceConfig struct {
	Name         string `toml:"name"`          // human-readable deployment name, e.g. "Familiar (Host-a)"
	AdminURL     string `toml:"admin_url"`     // admin console entrypoint
	RegisterURL  string `toml:"register_url"`  // where new users register a passkey (often same as admin_url)
	AdminContact string `toml:"admin_contact"` // e.g. "@roo on Slack" or "ops@example.com"
	DocsURL      string `toml:"docs_url"`      // link to user-facing documentation
	HelpNotes    string `toml:"help_notes"`    // freeform guidance injected verbatim into the tool response
}

// AdminConfig controls the WebAuthn-gated admin console. When Enabled
// is true the gateway mounts /admin/ and /admin/api/* on the HTTP
// adapter.
//
// PUBLIC-PROXY-MIGRATION introduces multi-RP support: the gateway is
// now fronted by both a public hostname (host-a.familiar.wiki via a
// DigitalOcean droplet) and the existing Tailscale-direct hostname.
// WebAuthn requires the RPID to be a registrable suffix of the
// origin's host, so the two paths can't share credentials — each
// inbound Host header maps to its own *webauthn.WebAuthn instance.
//
// Backwards compatibility: when RelyingParties is empty the loader
// synthesises a single RP from the legacy RPID + RPOrigins fields,
// so existing deployments keep working without a config edit.
type AdminConfig struct {
	Enabled       bool   `toml:"enabled"`
	RPDisplayName string `toml:"rp_display_name"`
	SessionMaxAge int    `toml:"session_max_age"` // seconds

	// FirstUserID is the canonical_id assigned to the first WebAuthn
	// credential registered on a fresh deploy. Pre-OWNER-MIGRATION
	// this was hard-coded to "owner" — the seam that let any
	// unresolved identity quietly become the bootstrap admin. Now
	// operators must declare it explicitly (e.g. "operator", "ali").
	// Also used as the scope key for the per-user entity vocab cache
	// during the single-tenant entry window.
	FirstUserID string `toml:"first_user_id"`

	// CookieSecure stamps the Secure attribute on every session /
	// pending cookie the admin path sets. Required in production
	// (both public + Tailscale paths are HTTPS-fronted); left off
	// in dev so localhost still works without a TLS terminator.
	CookieSecure bool `toml:"cookie_secure"`

	// RelyingParties is the post-migration shape. Each entry binds
	// one RPID + its allowed origins to a list of inbound Host
	// headers. The gateway picks the right WebAuthn instance per
	// request based on r.Host.
	RelyingParties []RelyingPartyConfig `toml:"relying_party"`

	// Legacy single-RP fields. Still parsed so deployments that
	// haven't migrated their toml keep working. When
	// RelyingParties is empty AND these are set, the loader
	// synthesises one RP entry whose Hosts list is derived from
	// the origins' hostnames. Slated for removal one release
	// after the migration; see PUBLIC-PROXY-MIGRATION §Cleanup.
	RPID      string   `toml:"rp_id"`
	RPOrigins []string `toml:"rp_origins"`
}

// EffectiveRelyingParties returns the canonical RP list for the
// admin handler. When operators have set [[admin.relying_party]]
// blocks the function returns those verbatim. Otherwise it
// synthesises a single RP from the legacy rp_id + rp_origins fields,
// deriving the Hosts list from each origin's hostname so the
// handler's per-Host routing has something to match. Returns an
// empty slice if neither form is configured (Validate catches that
// case at startup).
func (c AdminConfig) EffectiveRelyingParties() []RelyingPartyConfig {
	if len(c.RelyingParties) > 0 {
		return c.RelyingParties
	}
	if c.RPID == "" || len(c.RPOrigins) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(c.RPOrigins))
	seen := make(map[string]struct{}, len(c.RPOrigins))
	for _, origin := range c.RPOrigins {
		host := originHost(origin)
		if host == "" {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	return []RelyingPartyConfig{{
		RPID:    c.RPID,
		Origins: c.RPOrigins,
		Hosts:   hosts,
	}}
}

// originHost strips scheme + port from an origin string so the
// legacy fallback can derive the Hosts list automatically. Returns
// "" when the input is unparseable.
func originHost(origin string) string {
	s := origin
	if i := indexAfter(s, "://"); i >= 0 {
		s = s[i:]
	}
	if i := indexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if i := indexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}

// indexAfter returns the position immediately after `sep` in `s`,
// or -1 if `sep` is absent. Avoids importing strings just for the
// originHost helper.
func indexAfter(s, sep string) int {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return i + len(sep)
		}
	}
	return -1
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// RelyingPartyConfig describes one WebAuthn relying party.
//   - RPID is the registrable-suffix domain (e.g. "familiar.wiki").
//   - Origins is the full list of allowed origins (scheme + host
//   - optional port). WebAuthn enforces exact match against the
//     browser origin during ceremonies.
//   - Hosts is the set of inbound Host header values that route to
//     this RP. The handler's webauthnFor() helper does the lookup
//     case-insensitively after stripping any port.
type RelyingPartyConfig struct {
	RPID    string   `toml:"rp_id"`
	Origins []string `toml:"origins"`
	Hosts   []string `toml:"hosts"`
}

// SkillsConfig holds per-skill configuration blocks. Skills that need
// no config (weather) are absent here; skills that do (news feeds)
// slot in as named sub-tables.
// MediaConfig configures page-attached image storage
// (MEDIA-DIAGRAMS Phase 1). Bytes live as flat files under Dir;
// metadata lives in page_media.
type MediaConfig struct {
	// Dir is the media root. Default ~/.familiar/media.
	Dir string `toml:"dir"`
	// MaxUploadMB caps a single upload. Default 10.
	MaxUploadMB int `toml:"max_upload_mb"`
}

type SkillsConfig struct {
	News     NewsConfig     `toml:"news"`
	Research ResearchConfig `toml:"research"`
	// Dir is the imported-skills library root (SKILL-PACKAGES-SPEC
	// Phase 2): one agentskills.io-shaped directory per skill.
	// Operators can cp -r a skill in and rescan from the console.
	Dir string `toml:"dir"`
}

// ResearchConfig configures the research-workers skill
// (RESEARCH-SKILL-SPEC §7): parallel virtual-shard workers behind
// spawn_research_workers. Disabled by default — the workers are
// useless without web search, so it's opt-in like [tools.brave]
// itself, and main.go skips registration when search is off.
type ResearchConfig struct {
	Enabled bool `toml:"enabled"`
	// MaxWorkers caps concurrently running workers (semaphore). Keep
	// it ≤ inference slots − 2 so the orchestrating turn and
	// interactive chat always have slots. Default 3.
	MaxWorkers int `toml:"max_workers"`
	// WorkerSearchBudget is each worker's per-turn web_search grant
	// (ShardOverrides.SearchBudget). Default 4.
	WorkerSearchBudget int `toml:"worker_search_budget"`
	// WorkerTier maps to the shard tier aliases: "technical" (tier3)
	// runs workers on the chat model; "tier2" pins the sidecar model
	// for cheap workers without touching gpu-host slots. Default
	// "technical".
	WorkerTier string `toml:"worker_tier"`
	// WorkerModel pins workers to an explicit [[models]] ID instead of
	// the tier alias (the tier still shapes the thinking budget). The
	// model must speak tools; main.go validates and falls back to tier
	// routing with a warning. Empty = tier routing.
	WorkerModel string `toml:"worker_model"`
	// WriterModel enables compose_research_note: the note-writing pass
	// runs as a no-tools completion pinned to this [[models]] ID — so
	// the coordinator (chat) and writer can be different models. Empty
	// = the chat model writes the note in-turn (no compose tool).
	WriterModel string `toml:"writer_model"`
	// EvidenceRetentionHours bounds how long deep-tier evidence pages
	// live in the hidden per-user research books before a periodic
	// sweep reaps them. They're hidden from every listing, so the sweep
	// is the only thing keeping them from piling up forever. Default
	// 72h; ≤0 disables the sweep.
	EvidenceRetentionHours int `toml:"evidence_retention_hours"`
	// MaxRounds caps autonomous deep-run gap-fill rounds (§6.7): the
	// initial batch plus (MaxRounds-1) retries of failed sub-questions.
	// Default 2.
	MaxRounds int `toml:"max_rounds"`
}

// NewsConfig configures the news/RSS skill. Feeds maps a topic label
// (e.g. "ai", "automotive") to a list of RSS/Atom feed URLs. When the
// map is empty the get_news tool returns an error; search_news still
// works via the Brave fallback.
type NewsConfig struct {
	Feeds        map[string][]string `toml:"feeds"`
	CacheMinutes int                 `toml:"cache_minutes"` // default 15
}

// The config-file scheduler ([scheduler] / SchedulerConfig /
// ScheduledTask) was retired by SCHEDULED-ACTIONS-SPEC: scheduled
// actions live in the database (scheduled_actions), are managed from
// the console, and carry per-user ownership, a run ledger, failure
// breaker, shard envelopes, and event triggers. A leftover
// [scheduler] block in gateway.toml is ignored by the TOML decoder.

// ToolsConfig controls agentic tool integration.
type ToolsConfig struct {
	Brave         BraveConfig         `toml:"brave"`
	PirateWeather PirateWeatherConfig `toml:"pirate_weather"`
}

// PirateWeatherConfig controls the Pirate Weather API integration.
// Pirate Weather is a drop-in replacement for the (sunset) Dark Sky API;
// the free tier is generous enough for a homelab assistant.
type PirateWeatherConfig struct {
	APIKey string `toml:"api_key"`
}

// BraveConfig controls the Brave Search API integration.
type BraveConfig struct {
	APIKey     string `toml:"api_key"`
	MaxResults int    `toml:"max_results"`
	Enabled    bool   `toml:"enabled"`
}

// PipelineConfig controls context budgeting and prompt assembly.
type PipelineConfig struct {
	ResponseReserveTokens int `toml:"response_reserve_tokens"` // tokens reserved for generation
	MemoryBudgetPct       int `toml:"memory_budget_pct"`       // max % of remaining budget for memories
	MinHistoryTurns       int `toml:"min_history_turns"`       // always keep at least N recent turns
}

// ContextConfig controls the Phase 2 context window builder (ctxbuild).
// The three ratios carve up (WindowSize - OutputReservation); the conversation
// zone gets whatever is left. See FAMILIAR-PHASE2-SPEC.md §2 for the design.
type ContextConfig struct {
	WindowSize          int     `toml:"window_size"`
	OutputReservation   int     `toml:"output_reservation"`
	SystemPromptRatio   float64 `toml:"system_prompt_ratio"`
	MemoryRatio         float64 `toml:"memory_ratio"`
	ToolResultRatio     float64 `toml:"tool_result_ratio"`
	MaxToolResultTokens int     `toml:"max_tool_result_tokens"` // per-result head+tail cap; 0 = zone-eviction only
}

// SystemPromptConfig controls the persistent system prompt loaded at startup.
//
// Dir points at a tiered-prompt directory (base.md + tier_*.md + tool_policy.md).
// When Dir is set and readable, the context builder selects an overlay per
// request based on the router's complexity classification. File is the legacy
// monolithic fallback used when Dir is empty or missing.
type SystemPromptConfig struct {
	Dir  string `toml:"prompt_dir"` // directory of base.md + tier_*.md, supports ~ expansion
	File string `toml:"file"`       // legacy monolithic prompt, supports ~ expansion
}

// NodeConfig identifies this gateway node.
type NodeConfig struct {
	Name string `toml:"name"`
	Role string `toml:"role"` // "gateway", "worker", "hybrid"
}

// SidecarConfig controls the GPU sidecar connection.
//
// SIDECAR-SLOT-FIXES: each sidecar task (classify, condense, …) is
// assigned an explicit model ID from [[models]] via the *Model
// fields below. A task with no explicit assignment falls back to
// DefaultModel; with neither set the legacy role-based resolution
// runs (role="small" for the critical-path tasks, role="medium" for
// the background ones). RouterEndpoint is deprecated — kept only so
// the legacy fallback and pre-existing configs keep working.
type SidecarConfig struct {
	Enabled           bool   `toml:"enabled"`
	SocketPath        string `toml:"socket_path"`       // gRPC Unix socket (future)
	RouterEndpoint    string `toml:"router_endpoint"`   // deprecated — legacy small-slot URL
	EmbedderEndpoint  string `toml:"embedder_endpoint"` // HTTP endpoint for embedder
	ConnectTimeoutMs  int    `toml:"connect_timeout_ms"`
	RequestTimeoutMs  int    `toml:"request_timeout_ms"`
	RetryIntervalSecs int    `toml:"retry_interval_seconds"`
	FallbackOnFailure bool   `toml:"fallback_on_failure"`

	// Task → model ID assignment. Each value must match a [[models]]
	// entry ID; the sidecar client resolves it to that model's
	// endpoint. Empty → DefaultModel → legacy role resolution.
	DefaultModel       string `toml:"default_model"`
	ClassifyModel      string `toml:"classify_model"`
	CondenseModel      string `toml:"condense_model"`
	ExpandQueriesModel string `toml:"expand_queries_model"`
	ExtractModel       string `toml:"extract_model"`
	SummarizeModel     string `toml:"summarize_model"`
	ConflictModel      string `toml:"conflict_model"`
	RelationshipModel  string `toml:"relationship_model"`
	EntityGroupModel   string `toml:"entity_group_model"`
	// ExtractLargeModel routes extraction of LARGE documents (research
	// notes) to a bigger model that can hold the whole thing in context
	// — the small extract model overruns on a multi-KB note. Additive:
	// unset → large extractions fall back to the normal extract route.
	// Not folded into the default_model fallback (a research note should
	// only use the big model when explicitly opted in).
	ExtractLargeModel string `toml:"extract_large_model"`
}

// SidecarTaskModels returns the per-task model IDs in a stable order,
// each already resolved through the DefaultModel fallback. Used by
// the sidecar client to build its routing table and by validate.go
// to check the IDs against [[models]]. Tasks with no assignment (and
// no default) carry an empty string.
func (s SidecarConfig) SidecarTaskModels() []struct{ Task, Model string } {
	pick := func(explicit string) string {
		if explicit != "" {
			return explicit
		}
		return s.DefaultModel
	}
	return []struct{ Task, Model string }{
		{"classify", pick(s.ClassifyModel)},
		{"condense", pick(s.CondenseModel)},
		{"expand_queries", pick(s.ExpandQueriesModel)},
		{"extract", pick(s.ExtractModel)},
		{"summarize", pick(s.SummarizeModel)},
		{"conflict", pick(s.ConflictModel)},
		{"relationship", pick(s.RelationshipModel)},
		{"entity_group", pick(s.EntityGroupModel)},
	}
}

// HasExplicitTaskModels reports whether the operator set any task →
// model assignment (including DefaultModel). When false the sidecar
// client uses the legacy role-based slot resolution.
func (s SidecarConfig) HasExplicitTaskModels() bool {
	return s.DefaultModel != "" ||
		s.ClassifyModel != "" || s.CondenseModel != "" ||
		s.ExpandQueriesModel != "" || s.ExtractModel != "" ||
		s.SummarizeModel != "" || s.ConflictModel != "" ||
		s.RelationshipModel != "" || s.EntityGroupModel != ""
}

// MemoryConfig controls the memory/pgvector integration.
//
// The engine migration retired the RAM-tier promote-on-access
// fields (PromoteEnabled, PromoteThreshold) — there's no RAM cache
// to mirror into now that writes are synchronous to pgvector.
type MemoryConfig struct {
	UseSidecarEmbedder bool    `toml:"use_sidecar_embedder"`
	Store              string  `toml:"store"` // "local", "remote"
	LocalDSN           string  `toml:"local_dsn"`
	RelevanceThreshold float64 `toml:"relevance_threshold"`
	MaxInjected        int     `toml:"max_injected_memories"`
	DedupThreshold     float64 `toml:"dedup_threshold"`
}

// RerankConfig controls the cross-encoder reranking stage that runs
// after hybrid memory retrieval. A reranker scores (query, memory)
// pairs jointly and trims a wide candidate pool down to the few
// genuinely relevant facts — the precision step pure cosine /
// hybrid retrieval can't do on its own (chat-turn context review §5).
//
// Disabled by default: it needs a dedicated reranker model served on
// its own endpoint (e.g. a llama.cpp --reranking instance). When
// disabled the pipeline takes the hybrid-search top-k directly.
type RerankConfig struct {
	Enabled  bool   `toml:"enabled"`
	Endpoint string `toml:"endpoint"` // reranker server base URL
	Model    string `toml:"model"`    // served model name
	// PoolSize is how many hybrid-search candidates to feed the
	// reranker. The reranker trims this pool down to the caller's
	// top-k. Larger pool = better recall before the precision pass,
	// at the cost of a bigger rerank request. Default 50.
	PoolSize int `toml:"pool_size"`
}

// SleepConfig drives the in-process consolidation cycle
// . Read only when [engine] mode =
// "inprocess"; the gRPC engine ignores this block.
type SleepConfig struct {
	Enabled           bool    `toml:"enabled"`
	IntervalSecs      int     `toml:"interval_secs"`      // default 1800 (30 min)
	ConflictThreshold float64 `toml:"conflict_threshold"` // default 0.92 cosine similarity
	// SessionStaleDays is DEPRECATED and ignored: the decay phase it
	// governed wrote decay_score, which nothing anywhere read. Kept
	// so existing gateway.toml [sleep] blocks still parse.
	SessionStaleDays   int `toml:"session_stale_days"`
	SessionArchiveDays int `toml:"session_archive_days"` // default 90 — when hard-delete runs
}

// DefaultSleepConfig matches the previous engine's documented defaults
// + the gateway's existing noopDedupThreshold (0.92). Operators
// override via the [sleep] block in gateway.toml.
func DefaultSleepConfig() SleepConfig {
	return SleepConfig{
		Enabled:            true,
		IntervalSecs:       1800,
		ConflictThreshold:  0.92,
		SessionStaleDays:   30,
		SessionArchiveDays: 90,
	}
}

// EngineConfig is preserved as an empty struct so existing
// gateway.toml files that carry a (now-defunct) [engine] block
// still parse cleanly. The previous out-of-process engine and its
// gRPC dial were removed; everything lives in
// internal/memengine now. A followup cleanup can drop the
// block entirely once operators have migrated.
type EngineConfig struct{}

// ModelConfig describes a single LLM model endpoint.
type ModelConfig struct {
	ID             string   `toml:"id"`
	Provider       string   `toml:"provider"` // "llama-server" | "openai" | "ollama" | "vllm" | "llama-completion"
	Endpoint       string   `toml:"endpoint"`
	VaultKey       string   `toml:"vault_key"`
	APIKey         string   `toml:"api_key"`
	ContextWindow  int      `toml:"context_window"`
	Capabilities   []string `toml:"capabilities"`
	LatencyProfile string   `toml:"latency_profile"` // "local", "remote"
	MaxConcurrent  int      `toml:"max_concurrent"`

	// DisplayName is the human-readable label rendered in the chat
	// UI's model menu. Falls back to ID when blank — operators can
	// add new models with just `id` + `endpoint` and the dropdown
	// will show the raw ID, which is fine for diagnostic backends.
	// MODEL-SELECTOR.md.
	DisplayName string `toml:"display_name"`

	// Role tags this model with a single cross-cutting job in the
	// pipeline. Exactly zero or one model may carry each role.
	// Roles double as sidecar slot names under CHAT-REARCH:
	//   "small"       — classifier, query expansion, fact extraction
	//   "medium"      — preamble, conflict, relationship extraction
	//   "small_async" — optional dedicated instance for async post-turn
	//                   work (fact extraction). When absent, async work
	//                   shares the small slot.
	//   "embedder"    — text → vector
	//   "classifier"  — legacy alias for "small"; kept so existing
	//                   gateway.toml configs keep working through the
	//                   transition. Mapped to the small slot at startup.
	// Empty string means "chat-eligible model" — registered in the
	// router pool, not bound to any sidecar slot.
	Role string `toml:"role"`

	// Formatter selects the ModelFormatter used by the
	// llama-completion provider (qwen35 today; gemma / other
	// families slot in later). Ignored by every other provider —
	// the llama-server / openai chat-API providers pass their own
	// prompts to the model server. Required when Provider is
	// "llama-completion". CHAT-REARCH familiar-raw-completion-design.md.
	Formatter string `toml:"formatter"`

	// Sampling overlays per-model sampling params onto the
	// formatter's defaults. Only consumed by the llama-completion
	// provider today; other providers translate their own request
	// shape. Unknown keys are forwarded as-is to /completion.
	Sampling SamplingConfig `toml:"sampling"`
}

// SamplingConfig holds optional per-model sampling overrides. Zero
// values fall through to the formatter's default for that field —
// llama.cpp ignores fields it doesn't recognise so unknown overrides
// are safely additive.
type SamplingConfig struct {
	Temperature   *float64 `toml:"temperature"`
	TopP          *float64 `toml:"top_p"`
	TopK          *int     `toml:"top_k"`
	RepeatPenalty *float64 `toml:"repeat_penalty"`
	RepeatLastN   *int     `toml:"repeat_last_n"`
}

// AsMap renders the configured (non-nil) sampling fields into a
// map keyed by llama.cpp's /completion field names. Used by the
// llama-completion provider's WithSampling overlay.
func (s SamplingConfig) AsMap() map[string]any {
	out := make(map[string]any)
	if s.Temperature != nil {
		out["temperature"] = *s.Temperature
	}
	if s.TopP != nil {
		out["top_p"] = *s.TopP
	}
	if s.TopK != nil {
		out["top_k"] = *s.TopK
	}
	if s.RepeatPenalty != nil {
		out["repeat_penalty"] = *s.RepeatPenalty
	}
	if s.RepeatLastN != nil {
		out["repeat_last_n"] = *s.RepeatLastN
	}
	return out
}

// Role / slot constants. Use these instead of string literals.
const (
	ModelRoleEmbedder = "embedder"

	// Sidecar slots. Spec: CHAT-REARCH §"Sidecar Slot Architecture".
	ModelSlotSmall      = "small"
	ModelSlotMedium     = "medium"
	ModelSlotSmallAsync = "small_async"

	// Legacy alias — pre-CHAT-REARCH configs use "classifier"
	// where the new schema expects "small". Treated as small at
	// load time.
	ModelRoleClassifier = "classifier"
)

// DisplayLabel returns the model's UI label, falling back to its
// ID when no DisplayName is configured.
func (m ModelConfig) DisplayLabel() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.ID
}

// RouterConfig controls model selection.
type RouterConfig struct {
	Enabled             bool        `toml:"enabled"`
	PreferLocal         bool        `toml:"prefer_local"`
	UseSidecarRouter    bool        `toml:"use_sidecar_router"`
	FallbackRouter      string      `toml:"fallback_router"` // "rule_based"
	ConfidenceThreshold float64     `toml:"confidence_threshold"`
	EnableReasoning     bool        `toml:"enable_reasoning"`
	ReasoningThreshold  float64     `toml:"reasoning_threshold"`
	Rules               RouterRules `toml:"rules"`
}

// _unusedTierBlock — placeholder removed; the tier map and
// fallback model went out in CHAT-REARCH S1.2. Don't restore them
// without revisiting Chat-Backend-Rearch.md's single-chat-model
// decision.
// RouterRules holds the force rules table.
type RouterRules struct {
	Force []ForceRule `toml:"force"`
}

// ForceRule overrides model selection based on message pattern.
type ForceRule struct {
	Pattern string `toml:"pattern"`
	Channel string `toml:"channel"`
	Model   string `toml:"model"`
	Reason  string `toml:"reason"`
}

// AdapterConfig holds adapter-specific settings.
//
// CHAT-REARCH §"Phase 0" retired the OpenAI-shape entry. The HTTP
// adapter speaks the native chat protocol now; the toml key was
// renamed [adapter.openai] → [adapter.http] in the same change.
// Existing deployments with [adapter.openai] need a one-line edit
// to keep their HTTP listener wired.
type AdapterConfig struct {
	CLI   CLIConfig   `toml:"cli"`
	Slack SlackConfig `toml:"slack"`
	HTTP  HTTPConfig  `toml:"http"`
}

// SlackConfig holds Slack adapter settings.
type SlackConfig struct {
	BotToken       string   `toml:"bot_token"`       // xoxb-...
	AppToken       string   `toml:"app_token"`       // xapp-...
	Channels       []string `toml:"channels"`        // restrict to these channels (empty = all)
	AllowedUsers   []string `toml:"allowed_users"`   // Slack user IDs; empty = allow all
	DefaultChannel string   `toml:"default_channel"` // fallback target for proactive sends
	Debug          bool     `toml:"debug"`
	// APIBaseURL overrides the Slack API root (default
	// https://slack.com/api/). Set ONLY for testing against a fake
	// Slack server — both the REST client and apps.connections.open
	// honor it. Leave empty in production.
	APIBaseURL string `toml:"api_base_url"`
}

// HTTPConfig holds the native HTTP chat adapter's settings. Renamed
// from OpenAIConfig in CHAT-REARCH §"Phase 0" — the adapter now
// speaks the native protocol (POST /api/chat).
//
// OWNER-MIGRATION removed the legacy default_sender_id field that
// used to backstop unauthenticated requests with a hard-coded
// canonical id (typically "owner"). Senderless requests are now
// rejected with 401; the native adapter requires a workspace
// session cookie (the X-User-Email path was removed).
type HTTPConfig struct {
	ListenAddr string `toml:"listen_addr"` // e.g., "0.0.0.0:8000"

	// Identity / auth on /api/chat is the admin session cookie only —
	// a server-verified canonical identity. Two prior knobs were
	// removed (EXTERNAL-READINESS-REVIEW.md):
	//   - trust_email_header / X-User-Email — a spoofable identity path.
	//   - api_key — an inbound "bearer token" that was never enforced
	//     (read nowhere). Shard invocations authenticate with the
	//     dedicated, hashed, per-shard shard_tokens system instead.
	// Old configs carrying either key still parse — unknown TOML keys
	// are ignored — they just do nothing now.
}

// CLIConfig holds CLI adapter settings.
type CLIConfig struct {
	HistoryFile string `toml:"history_file"`
	Prompt      string `toml:"prompt"`
}

// EmbedderConfig holds embedding settings.
type EmbedderConfig struct {
	Endpoint  string `toml:"endpoint"`
	Model     string `toml:"model"`
	Dimension int    `toml:"dimension"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	_ = home // home was used by the legacy engine socket path
	return &Config{
		Sleep: DefaultSleepConfig(),
		Node: NodeConfig{
			Role: "gateway",
		},
		Router: RouterConfig{
			Enabled:             true,
			PreferLocal:         true,
			FallbackRouter:      "rule_based",
			ConfidenceThreshold: 0.7,
			ReasoningThreshold:  0.5,
		},
		Sidecar: SidecarConfig{
			SocketPath:        "/run/familiar/sidecar.sock",
			ConnectTimeoutMs:  500,
			RequestTimeoutMs:  5000,
			RetryIntervalSecs: 10,
			FallbackOnFailure: true,
		},
		Memory: MemoryConfig{
			RelevanceThreshold: 0.72,
			MaxInjected:        10,
			DedupThreshold:     0.95,
		},
		Rerank: RerankConfig{
			Enabled:  false,
			PoolSize: 50,
		},
		Pipeline: PipelineConfig{
			ResponseReserveTokens: 4096,
			MemoryBudgetPct:       20,
			MinHistoryTurns:       2,
		},
		Skills: SkillsConfig{
			// Research stays disabled until an operator opts in; the
			// worker knobs default here so a bare `enabled = true`
			// block gets the RESEARCH-SKILL-SPEC §7 values.
			Research: ResearchConfig{
				MaxWorkers:             3,
				WorkerSearchBudget:     4,
				WorkerTier:             "technical",
				EvidenceRetentionHours: 72,
				MaxRounds:              2,
			},
		},
		Context: ContextConfig{
			WindowSize:          32768,
			OutputReservation:   4096,
			SystemPromptRatio:   0.10,
			MemoryRatio:         0.12,
			ToolResultRatio:     0.12,
			MaxToolResultTokens: 2000,
		},
		Adapter: AdapterConfig{
			CLI: CLIConfig{
				HistoryFile: filepath.Join(home, ".familiar", "cli_history"),
				Prompt:      "> ",
			},
		},
		Embedder: EmbedderConfig{
			Model:     "nomic-embed-text",
			Dimension: 768,
		},
		SystemPrompt: SystemPromptConfig{
			Dir:  "~/.familiar/prompts",
			File: "~/.familiar/system_prompt.md",
		},
	}
}

// Load reads and parses a TOML config file, applying env var expansion and
// tilde expansion to string fields. Falls back to defaults for unset values.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// Apply expansions.
	expandConfig(cfg)

	return cfg, nil
}

// expandConfig applies tilde and env var expansion to path-like fields.
func expandConfig(cfg *Config) {
	cfg.Adapter.CLI.HistoryFile = expandPath(cfg.Adapter.CLI.HistoryFile)
	cfg.Adapter.CLI.Prompt = expandEnv(cfg.Adapter.CLI.Prompt)
	cfg.Embedder.Endpoint = expandEnv(cfg.Embedder.Endpoint)

	for i := range cfg.Models {
		cfg.Models[i].APIKey = expandEnv(cfg.Models[i].APIKey)
		cfg.Models[i].Endpoint = expandEnv(cfg.Models[i].Endpoint)
	}

	cfg.Sidecar.SocketPath = expandPath(cfg.Sidecar.SocketPath)
	cfg.Memory.LocalDSN = expandEnv(cfg.Memory.LocalDSN)

	cfg.Adapter.Slack.BotToken = expandEnv(cfg.Adapter.Slack.BotToken)
	cfg.Adapter.Slack.AppToken = expandEnv(cfg.Adapter.Slack.AppToken)

	cfg.Push.VAPIDPrivateKey = expandEnv(cfg.Push.VAPIDPrivateKey)

	cfg.SystemPrompt.Dir = expandPath(cfg.SystemPrompt.Dir)
	cfg.SystemPrompt.File = expandPath(cfg.SystemPrompt.File)
	if cfg.Skills.Dir == "" {
		cfg.Skills.Dir = "~/.familiar/skills"
	}
	cfg.Skills.Dir = expandPath(cfg.Skills.Dir)
	if cfg.Media.Dir == "" {
		cfg.Media.Dir = "~/.familiar/media"
	}
	cfg.Media.Dir = expandPath(cfg.Media.Dir)
	if cfg.Media.MaxUploadMB <= 0 {
		cfg.Media.MaxUploadMB = 10
	}

	cfg.Tools.Brave.APIKey = expandEnv(cfg.Tools.Brave.APIKey)
	cfg.Tools.PirateWeather.APIKey = expandEnv(cfg.Tools.PirateWeather.APIKey)

	// Web Push VAPID keys — the private key is a secret, expand it from
	// the environment. Default the contact subject when push is on.
	cfg.Push.VAPIDPublicKey = expandEnv(cfg.Push.VAPIDPublicKey)
	cfg.Push.VAPIDPrivateKey = expandEnv(cfg.Push.VAPIDPrivateKey)
	cfg.Push.Subject = expandEnv(cfg.Push.Subject)
	if cfg.Push.Enabled() && cfg.Push.Subject == "" {
		cfg.Push.Subject = "mailto:admin@example.com"
	}
}

// expandPath expands ~/ prefix and env vars in a path string.
func expandPath(s string) string {
	s = expandEnv(s)
	if strings.HasPrefix(s, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			s = filepath.Join(home, s[2:])
		}
	}
	return s
}

// expandEnv expands $VAR and ${VAR} patterns.
func expandEnv(s string) string {
	return os.ExpandEnv(s)
}
