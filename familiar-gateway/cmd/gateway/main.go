package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	cliadapter "github.com/familiar/gateway/internal/adapter/cli"
	slackadapter "github.com/familiar/gateway/internal/adapter/slack"
	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/brave"
	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/ctxbuild"
	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/engine"
	identitypkg "github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/maintenance"
	"github.com/familiar/gateway/internal/memengine"
	"github.com/familiar/gateway/internal/memevents"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/prefetch"
	"github.com/familiar/gateway/internal/push"
	"github.com/familiar/gateway/internal/rerank"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/familiar/gateway/internal/skillpkg"
	"github.com/familiar/gateway/internal/skills"
	datetimeskill "github.com/familiar/gateway/internal/skills/datetime"
	fetchskill "github.com/familiar/gateway/internal/skills/fetch"
	instanceskill "github.com/familiar/gateway/internal/skills/instance"
	memoryskill "github.com/familiar/gateway/internal/skills/memory"
	"github.com/familiar/gateway/internal/skills/news"
	profileskill "github.com/familiar/gateway/internal/skills/profile"
	"github.com/familiar/gateway/internal/skills/search"
	"github.com/familiar/gateway/internal/skills/skillpacks"
	"github.com/familiar/gateway/internal/skills/weather"
	wikiskill "github.com/familiar/gateway/internal/skills/wiki"
	"github.com/familiar/gateway/internal/userprofile"
)

func main() {
	var (
		configPath = flag.String("config", "", "path to gateway.toml (default: auto-discover)")
		verbose    = flag.Bool("verbose", false, "show routing metadata after each response")
		useSlack   = flag.Bool("slack", false, "run Slack adapter instead of CLI")
		useHTTP    = flag.Bool("http", false, "run native HTTP chat adapter (POST /api/chat)")
		genVapid   = flag.Bool("gen-vapid", false, "print a fresh VAPID keypair for the [push] config block and exit")
	)
	flag.Parse()

	// One-shot helper: generate Web Push VAPID keys for [push] and exit.
	if *genVapid {
		priv, pub, err := push.GenerateVAPIDKeys()
		if err != nil {
			log.Fatalf("gen-vapid: %v", err)
		}
		fmt.Printf("[push]\nvapid_public_key = %q\nvapid_private_key = %q\nsubject = \"mailto:you@example.com\"\n", pub, priv)
		return
	}

	// Gateway start time is captured before any heavy init so the
	// dashboard's uptime reading is honest about when the process
	// actually began serving, not when wiring finished.
	startTime := time.Now()

	// 1. Load config.
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("[gateway] failed to load config: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("[gateway] invalid config: %v", err)
	}

	// Research workers live inside the admin/wiki wiring far below; on
	// a deploy without the admin console that block never runs, so the
	// dependency has to be called out here or enabled=true silently
	// does nothing. (The other prerequisites — DB pool, wiki store —
	// already log their own failures loudly inside the admin block.)
	if cfg.Skills.Research.Enabled && !cfg.Admin.Enabled {
		log.Printf("[research] skills.research.enabled=true requires admin.enabled=true (wiki store) — spawn_research_workers will NOT be registered")
	}

	// 2. Wire the in-process memengine. The engine migration
	// deleted the previous engine and the gRPC client entirely; there's
	// only one implementation now. The collaborators (memStore +
	// session manager) get wired below via SetDeps once they exist.
	log.Printf("[gateway] engine: in-process (memengine)")
	inProcMem := memengine.New(nil, nil, nil, "gateway")
	var eng engine.Service = inProcMem
	defer eng.Close()

	// Root context cancels on SIGINT/SIGTERM so a deploy restart winds
	// the gateway down gracefully instead of a hard kill. Every
	// long-lived goroutine started below watches this ctx (adapters,
	// sidecar, sleep loop, entity vocab, health checker), so cancelling
	// it lets each drain its own loop before the deferred resource
	// releases (pool.Close, eng.Close → sleep.Stop) run at return.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fmt.Printf("Engine: in-process memengine (memory=pgvector)\n")

	// Agent identity stays a stable string used for fact attribution
	// + the boot banner. Was an RPC against the previous engine; now just
	// a config-driven label.
	agentID := "gateway"
	fmt.Printf("Agent ID: %s\n", agentID)

	// 5. Load system prompt. Prefer the tiered prompt directory; fall back
	// to the legacy monolithic file. The PromptStore keeps both so per-tier
	// selection can degrade gracefully if the dir is missing.
	var systemPrompt string
	if cfg.SystemPrompt.File != "" {
		data, spErr := os.ReadFile(cfg.SystemPrompt.File)
		if spErr != nil {
			log.Printf("[gateway] warning: could not read system prompt from %s: %v", cfg.SystemPrompt.File, spErr)
		} else {
			systemPrompt = strings.TrimSpace(string(data))
			log.Printf("[gateway] loaded system prompt from %s (%d bytes)", cfg.SystemPrompt.File, len(systemPrompt))
		}
	}
	promptStore, psErr := ctxbuild.NewPromptStore(cfg.SystemPrompt.Dir, systemPrompt)
	if psErr != nil {
		log.Printf("[gateway] warning: prompt store load error from %s: %v (using fallback)", cfg.SystemPrompt.Dir, psErr)
	}
	if promptStore.Loaded() {
		log.Printf("[gateway] loaded tiered prompt store from %s", cfg.SystemPrompt.Dir)
	} else if cfg.SystemPrompt.Dir != "" {
		log.Printf("[gateway] tiered prompt dir %s not found — using monolithic fallback", cfg.SystemPrompt.Dir)
	}

	// 5b. Build API key function.
	apiKeyFn := makeAPIKeyFn(eng, cfg.Models)

	// 6. Build registry + router.
	reg := router.NewRegistry(cfg.Models)
	rtr := router.NewRouter(cfg.Router, reg)

	// Maintenance-mode switch: routes chat to an operator-selected
	// fallback model when the primary is down (auto) or drained
	// (manual toggle). Selection persists via instance_settings;
	// rehydrated in the admin block below. Wired into the pipeline
	// (routing) and the admin handler (toggle + /auth/status banner).
	maint := maintenance.New(
		reg.StatusOf,
		func(id string) string {
			if mc := reg.GetModelConfig(id); mc != nil {
				return mc.DisplayLabel()
			}
			return ""
		},
		rtr.GetChatModelID,
	)

	// Start health checks in background.
	healthCtx, healthCancel := context.WithCancel(ctx)
	defer healthCancel()
	reg.StartHealthChecks(healthCtx, apiKeyFn)

	// 6b. Start GPU sidecar client (optional).
	// MODEL-ROLES: pass the registry as the EndpointResolver so a
	// model tagged role="classifier" / role="embedder" supplies
	// the slot endpoints. Falls through to the literal config when
	// no role-tagged model exists.
	var sc *sidecar.Client
	if cfg.Sidecar.Enabled {
		sc = sidecar.NewClient(cfg.Sidecar, cfg.Router, reg)
		sc.Start(ctx)
		defer sc.Stop()

		rtr.SetSidecar(sc)
		log.Printf("[gateway] sidecar enabled")
		// Print the resolved task → model → endpoint table so the
		// operator can verify routing without reverse-engineering it.
		sc.LogRouting()
	}

	// 7. Build embedder. We need a throwaway helper on the Pipeline type
	// here (MakeEmbedder is a method), but the real Pipeline is
	// constructed below once all its dependencies are resolved.
	embedderHelper := &pipeline.Pipeline{}
	var embedder pipeline.EmbedFunc
	if cfg.Memory.UseSidecarEmbedder && sc != nil {
		embedder = func(ctx context.Context, text string) ([]float32, error) {
			result, err := sc.Embed(ctx, text)
			if err != nil {
				if cfg.Embedder.Endpoint != "" {
					httpEmbedder := embedderHelper.MakeEmbedder(cfg.Embedder)
					if httpEmbedder != nil {
						return httpEmbedder(ctx, text)
					}
				}
				return nil, err
			}
			return result.Embedding, nil
		}
	} else if cfg.Embedder.Endpoint != "" {
		embedder = embedderHelper.MakeEmbedder(cfg.Embedder)
	}

	// 8. Resolve optional stores + skill registry. Everything goes into
	// a pipeline.Deps bundle that's handed to pipeline.New once at the
	// end — no post-construction wiring.
	sm := session.NewManager()
	// Idle-session eviction: bound in-memory growth as the user base
	// scales. Evicted sessions rehydrate transparently from the
	// sessions + messages tables on the user's next request, so this
	// only frees RAM (EXTERNAL-READINESS-REVIEW.md). Sweep every
	// 10m, evict after 30m idle.
	sm.StartEviction(ctx, 10*time.Minute, 30*time.Minute)

	skillReg := skills.NewRegistry()
	defer func() {
		if err := skillReg.Close(); err != nil {
			log.Printf("[gateway] warning: skill registry close: %v", err)
		}
	}()

	var toolOrch *prefetch.Orchestrator
	var braveClient *brave.Client
	if cfg.Tools.Brave.Enabled && cfg.Tools.Brave.APIKey != "" {
		braveClient = brave.New(cfg.Tools.Brave.APIKey, cfg.Tools.Brave.MaxResults)
		toolOrch = prefetch.NewOrchestrator(braveClient)
		log.Printf("[gateway] tool orchestrator enabled: brave_search=true")
		if err := skillReg.Register(search.New(braveClient)); err != nil {
			log.Printf("[gateway] warning: register search skill: %v", err)
		}
	}

	weatherSkill := weather.New(nil, cfg.Tools.PirateWeather.APIKey)
	if err := skillReg.Register(weatherSkill); err != nil {
		log.Printf("[gateway] warning: register weather skill: %v", err)
	}
	if cfg.Tools.PirateWeather.APIKey == "" {
		log.Printf("[gateway] warning: pirate_weather api_key not configured; weather tools will return an error at call time")
	}
	if err := skillReg.Register(news.New(cfg.Skills.News, braveClient)); err != nil {
		log.Printf("[gateway] warning: register news skill: %v", err)
	}
	if err := skillReg.Register(fetchskill.New()); err != nil {
		log.Printf("[gateway] warning: register fetch skill: %v", err)
	}

	// Date/time skill — pure Go, no config. The model has no reliable
	// wall-clock of its own, so it should call this rather than guess
	// "now" from training data.
	if err := skillReg.Register(datetimeskill.New()); err != nil {
		log.Printf("[gateway] warning: register datetime skill: %v", err)
	}

	// Instance metadata skill — registered unconditionally. The skill
	// itself returns "No deployment info is configured for this
	// instance." when [instance] is empty, which is a better answer
	// than the LLM having no path to "where do I register?" at all.
	// Operators backfill [instance] in gateway.toml when they're
	// ready; the tool starts surfacing real values immediately on
	// next restart.
	if err := skillReg.Register(instanceskill.New(cfg.Instance)); err != nil {
		log.Printf("[gateway] warning: register instance skill: %v", err)
	} else if instanceskill.HasAnyField(cfg.Instance) {
		log.Printf("[skills] instance info registered (admin=%q, contact=%q)",
			cfg.Instance.AdminURL, cfg.Instance.AdminContact)
	} else {
		log.Printf("[skills] instance info registered (no [instance] config — tool will report 'no info' until backfilled)")
	}

	// 8c. Optional pgvector memory store + its dependents (user profile,
	// session, identity resolver). All share the single *db.Pool opened
	// here; schema migrations run once before any store is constructed.
	var memStore *memory.PgVectorStore
	var relStore *memory.PgRelationshipStore
	var entityVocab *memory.EntityVocab
	var profStore *userprofile.Store
	var sessStore *session.Store
	var convStore *admin.ConversationStore
	var identityResolver *identitypkg.Resolver
	var sharedPool *db.Pool
	var skillPkgStore *skillpkg.Store
	if cfg.Memory.LocalDSN != "" {
		pool, poolErr := db.Open(cfg.Memory.LocalDSN)
		if poolErr != nil {
			log.Printf("[memory] warning: pool unavailable: %v", poolErr)
		} else {
			defer pool.Close()
			sharedPool = pool

			// Imported-skills library (SKILL-PACKAGES-SPEC Phase 2).
			// The directory is the artifact; the store indexes it.
			if sps, spErr := skillpkg.NewStore(pool, cfg.Skills.Dir); spErr != nil {
				log.Printf("[skills] warning: imported-skills store: %v", spErr)
			} else {
				skillPkgStore = sps
				log.Printf("[skills] imported-skills library at %s", cfg.Skills.Dir)
			}

			migCtx, migCancel := context.WithTimeout(ctx, 5*time.Second)
			if migErr := db.Migrate(migCtx, pool); migErr != nil {
				log.Printf("[memory] warning: migrations failed: %v", migErr)
			}
			migCancel()

			// Identity seeding from operator config (replaces the old
			// hardcoded phase_c_identity_seed migration). Idempotent
			// via ON CONFLICT DO NOTHING; safe to re-run on every
			// startup so config edits propagate without manual SQL.
			if len(cfg.Identity.Seed) > 0 {
				seedCtx, seedCancel := context.WithTimeout(ctx, 5*time.Second)
				if seedErr := identitypkg.Bootstrap(seedCtx, pool, cfg.Identity.Seed); seedErr != nil {
					log.Printf("[identity] warning: seed bootstrap failed: %v", seedErr)
				} else {
					log.Printf("[identity] %d seed(s) applied", len(cfg.Identity.Seed))
				}
				seedCancel()
			}

			ms, memErr := memory.NewPgVectorStore(pool)
			if memErr != nil {
				log.Printf("[memory] warning: pgvector memory store unavailable: %v", memErr)
			} else {
				memStore = ms
				log.Printf("[memory] store connected (threshold=%.2f, max=%d)",
					cfg.Memory.RelevanceThreshold, cfg.Memory.MaxInjected)
			}

			// The engine migration: the in-process memengine
			// was constructed at boot with nil deps so Ping +
			// Identity could answer immediately. Wire the real
			// collaborators now — sm + memStore + pool exist as
			// of this branch.
			if inProcMem != nil {
				inProcMem.SetDeps(pool, memStore, sm)
				log.Printf("[gateway] memengine deps wired (pool + memStore + sessions)")

				// PR-3: kick off the sleep / consolidation cycle.
				// First tick fires after IntervalSecs so a freshly
				// booted gateway doesn't run a heavy pass during
				// warmup. Stop on ctx cancel via the shared
				// shutdown context.
				sc := memengine.NewSleepCycle(pool, agentID, cfg.Sleep)
				sc.Start(ctx)
				inProcMem.SetSleepCycle(sc)
				log.Printf("[sleep] consolidation cycle started (interval=%ds)", cfg.Sleep.IntervalSecs)
			}

			if rs, relErr := memory.NewPgRelationshipStore(pool); relErr != nil {
				log.Printf("[memory] warning: relationship store unavailable: %v", relErr)
			} else {
				relStore = rs
				log.Printf("[memory] relationship store ready (graph layer enabled)")
				// Launch the entity vocab cache so the pipeline's
				// multi-hop traversal has somewhere to look up entity
				// names found in retrieved memory contents. Scoped to
				// the configured primary user (single-tenant entry
				// point — per-user vocabs land when the relationship
				// store grows a user_id index suitable for fan-out).
				entityVocab = memory.NewEntityVocab(relStore, cfg.Admin.FirstUserID, 5*time.Minute)
				entityVocab.Start(ctx)
			}

			if ps, profErr := userprofile.NewStore(pool); profErr != nil {
				log.Printf("[memory] warning: user profile store unavailable: %v", profErr)
			} else {
				profStore = ps
				log.Printf("[memory] user profile store ready (shared pool)")
			}

			if ss, sessErr := session.NewStore(pool); sessErr != nil {
				log.Printf("[memory] warning: session store unavailable: %v", sessErr)
			} else {
				sessStore = ss
				log.Printf("[memory] session store ready (rolling summary persistence)")
			}

			// Workspace conversation store. Constructed here (not
			// inside the admin block below) so it can flow into
			// pipeline.Deps for session-turn hydration after a
			// restart — see SESSION-HYDRATION.md. The admin
			// handler still attaches it separately for its own
			// REST endpoints.
			if cs := admin.NewConversationStore(pool); cs != nil {
				convStore = cs
				log.Printf("[memory] conversation store ready (turn hydration enabled)")
			}

			identCtx, identCancel := context.WithTimeout(ctx, 5*time.Second)
			resolver, identErr := identitypkg.NewResolver(identCtx, pool)
			identCancel()
			if identErr != nil {
				log.Printf("[identity] warning: resolver unavailable: %v", identErr)
			} else {
				identityResolver = resolver
				log.Printf("[identity] resolver ready (%d mappings)", resolver.Count())
			}
		}
	}

	// 8d. Register the memory skill. It needs embedder + engine +
	// store(s); if any are missing, the skill still registers but
	// individual tools return user-facing errors on invocation.
	var memSkillOpts []memoryskill.Option
	if memStore != nil {
		memSkillOpts = append(memSkillOpts, memoryskill.WithManager(memStore))
	}
	if err := skillReg.Register(memoryskill.New(eng, memStore, memoryskill.EmbedFunc(embedder), memSkillOpts...)); err != nil {
		log.Printf("[skills] warning: register memory skill: %v", err)
	}

	// Profile skill (Phase 5) — exposes update_my_email for Slack-
	// bootstrapped users whose workspace didn't surface an email.
	// Needs the identity resolver for SetUserEmail; skipped when the
	// resolver isn't wired (no pgvector pool), in which case the
	// bootstrap welcome DM can still fall back to its no-email
	// message but the user has no way to self-link later.
	if identityResolver != nil {
		if err := skillReg.Register(profileskill.New(identityResolver)); err != nil {
			log.Printf("[skills] warning: register profile skill: %v", err)
		} else {
			log.Printf("[skills] profile (update_my_email) registered")
		}
	}

	// Notes skill (FAMILIAR-NOTES-SKILL-SPEC + KEY-PROVISIONING-SPEC).
	// adminH is hoisted to outer scope (declared just below) so the
	// wiki skill's lazy resolve closure can capture it; the admin
	// pool block assigns into the same variable. At registration
	// time adminH is nil, but the skill only dereferences it on
	// the first tool invocation — by then the pool block has run.
	//
	// The notes skill used to register here too, backed by
	// admin.NotesStore directly. Phase 1 step 6 retires it: the
	// notes table is post-migration backup only, and the wiki
	// skill (with include_personal=true on list_books) now covers
	// every tool the model used to reach via search_notes /
	// read_note / create_note / update_note / append_to_note /
	// patch_note. The notes skill package is left in the repo for
	// git history; nothing wires it.
	var adminH *admin.Handler

	// Wiki skill — same lazy-resolve pattern. The closure returns
	// nil until adminH is wired and AttachWikiStore has run, at
	// which point every tool invocation gets the live store.
	if err := skillReg.Register(wikiskill.New(func() wikiskill.WikiBackend {
		if adminH == nil {
			return nil
		}
		return adminH.WikiStore()
	})); err != nil {
		log.Printf("[skills] warning: register wiki skill: %v", err)
	} else {
		log.Printf("[skills] wiki (list_books/list_pages/search/read + create/update/append/patch) registered (Postgres-backed)")
	}

	// Imported-skill access tools (SKILL-PACKAGES-SPEC Phase 2) —
	// shard-only: the pipeline excludes them from trusted-path
	// advertisement and dispatch; the shard augmenter adds them to a
	// shard's allowlist only when it has bound skills.
	if err := skillReg.Register(skillpacks.New(func() skillpacks.Backend {
		if skillPkgStore == nil {
			return nil
		}
		return skillPkgStore
	})); err != nil {
		log.Printf("[skills] warning: register skillpacks: %v", err)
	}

	var skillRegForPipeline *skills.Registry
	if names := skillReg.SkillNames(); len(names) > 0 {
		log.Printf("[skills] registered: %s (%d tools)", strings.Join(names, ", "), len(skillReg.ToolDefinitions()))
		skillRegForPipeline = skillReg
	}

	// The preamble generator posts directly to a sidecar endpoint
	// (it's not one of the routed tasks). Prefer the legacy literal
	// router_endpoint; when that's unset — the SIDECAR-SLOT-FIXES
	// task-model path — borrow the classify task's fast endpoint.
	sidecarEndpoint := ""
	if cfg.Sidecar.Enabled {
		sidecarEndpoint = cfg.Sidecar.RouterEndpoint
		if sidecarEndpoint == "" && sc != nil {
			sidecarEndpoint = sc.TaskEndpoint(sidecar.TaskClassify)
		}
		if sidecarEndpoint != "" {
			log.Printf("[pipeline] preamble enabled via sidecar at %s", sidecarEndpoint)
		}
	}

	// Cross-encoder reranker. Off unless [rerank] enabled = true AND
	// an endpoint is configured; rerank.New returns nil for a blank
	// endpoint, and the pipeline treats a nil reranker as "skip the
	// precision pass, use hybrid-search RRF order".
	var reranker *rerank.Client
	if cfg.Rerank.Enabled {
		reranker = rerank.New(cfg.Rerank.Endpoint, cfg.Rerank.Model)
		if reranker != nil {
			log.Printf("[pipeline] cross-encoder rerank enabled at %s (pool=%d)",
				cfg.Rerank.Endpoint, cfg.Rerank.PoolSize)
		} else {
			log.Printf("[pipeline] rerank enabled but endpoint is blank — reranking disabled")
		}
	}

	// CHAT-REARCH S5: typed memory-write event bus. Default sink writes
	// to the gateway log (best-effort one-line summary per event); the
	// SSE endpoint mounted on the OpenAI adapter fans the same stream
	// out to subscribed browsers. Ring size 128 covers a healthy
	// reconnect window per session.
	memEvents := memevents.NewBus(128, func(e memevents.Event) {
		log.Printf("[memevents] sess=%s kind=%s id=%d", e.SessionID, e.Kind, e.ID)
	})

	pl := pipeline.New(pipeline.Deps{
		Engine:         eng,
		Router:         rtr,
		Sessions:       sm,
		AgentID:        agentID,
		SystemPrompt:   systemPrompt,
		ShardOnlyTools: []string{"use_skill", "read_skill_file"},
		ShardAugment: func(actx context.Context, ov *pipeline.ShardOverrides) error {
			if skillPkgStore == nil || ov == nil || ov.ShardID == "" {
				return nil
			}
			pkgs, err := skillPkgStore.ListShardSkills(actx, ov.ShardID)
			if err != nil {
				return err
			}
			if len(pkgs) == 0 {
				return nil
			}
			ps := make([]skillpkg.PromptSkill, 0, len(pkgs))
			for _, p := range pkgs {
				ps = append(ps, skillpkg.PromptSkill{Name: p.Name, Description: p.Description})
			}
			ov.SystemPrompt += skillpkg.PromptBlock(ps)
			ov.ToolAllowlist = append(ov.ToolAllowlist, "use_skill", "read_skill_file")
			return nil
		},
		// USER-SKILLS-SPEC Phase B: the trusted-path grant. Returns
		// the "## Skills" block for the user's chat-enabled personal
		// skills ("" = none = no unlock). Failures degrade to "" —
		// the turn proceeds without personal skills. Capped so a
		// pathological library can't flood the system prompt (the
		// no-silent-caps rule: the truncation is logged).
		UserSkillsAugment: func(actx context.Context, userID string) string {
			if skillPkgStore == nil || userID == "" {
				return ""
			}
			pkgs, err := skillPkgStore.ListChatEnabled(actx, userID)
			if err != nil {
				log.Printf("[skills] user-skills augment (%s): %v — continuing without personal skills", userID, err)
				return ""
			}
			if len(pkgs) == 0 {
				return ""
			}
			const maxChatSkills = 20
			if len(pkgs) > maxChatSkills {
				log.Printf("[skills] user %s has %d chat-enabled skills; advertising first %d", userID, len(pkgs), maxChatSkills)
				pkgs = pkgs[:maxChatSkills]
			}
			ps := make([]skillpkg.PromptSkill, 0, len(pkgs))
			for _, p := range pkgs {
				ps = append(ps, skillpkg.PromptSkill{Name: p.Name, Description: p.Description})
			}
			return skillpkg.PromptBlock(ps)
		},
		Embedder:    embedder,
		APIKeyFn:    apiKeyFn,
		MemoryStore: memStore,
		RelationshipStore: func() memory.RelationshipStore {
			if relStore == nil {
				return nil
			}
			return relStore
		}(),
		EntityVocab: entityVocab,
		Versioner: func() pipeline.MemoryVersioner {
			if memStore == nil {
				return nil
			}
			return memStore
		}(),
		MemoryConfig:   cfg.Memory,
		RerankConfig:   cfg.Rerank,
		Reranker:       reranker,
		PipelineConfig: cfg.Pipeline,
		ContextConfig: ctxbuild.Config{
			WindowSize:          cfg.Context.WindowSize,
			OutputReservation:   cfg.Context.OutputReservation,
			SystemPromptRatio:   cfg.Context.SystemPromptRatio,
			MemoryRatio:         cfg.Context.MemoryRatio,
			ToolResultRatio:     cfg.Context.ToolResultRatio,
			MaxToolResultTokens: cfg.Context.MaxToolResultTokens,
		},
		ProfileStore:     profStore,
		PromptStore:      promptStore,
		SidecarEndpoint:  sidecarEndpoint,
		SidecarClient:    sc,
		ToolOrchestrator: toolOrch,
		SkillRegistry:    skillRegForPipeline,
		SessionStore:     sessStore,
		Conversations:    convStore,
		IdentityResolver: identityResolver,
		EffortResolver:   classifier.ResolverFromConfig(cfg.Effort),
		Events:           memEvents,
		Maintenance:      maint,
	})
	// Detached turns (SSE clients that disconnect mid-stream still
	// finish + persist) yield to gateway shutdown via this root ctx.
	pl.SetLifetime(ctx)

	// 8e. (Retired) The config-file cron scheduler lived here until
	// SCHEDULED-ACTIONS-SPEC replaced it wholesale: scheduled actions
	// are DB-backed (per-user ownership, run ledger, breaker, shard
	// envelopes, event triggers) and wired further up alongside the
	// admin handler. A stale [scheduler] TOML block is ignored by the
	// decoder; recreate any task as a scheduled action in the console.

	// 9. Run adapters. Multiple adapters run concurrently — OpenAI
	// (for Open WebUI) and Slack side by side. CLI is the interactive
	// fallback when nothing else is configured.
	adapterErrs := make(chan error, 3)
	var adapterCount int

	// 9a. HTTP adapter + admin console. The whole console/route surface
	// (auth, memory + graph, wiki/notes, shards, media, push, scheduled
	// actions, autonomous research) is mounted inside runHTTPAdapter —
	// see httpadapter.go. It returns the admin.Handler so the wiki skill's
	// lazy resolver (registered far above) reaches the live store; the
	// adapter itself runs in a goroutine and reports its exit on adapterErrs.
	if *useHTTP {
		adapterCount++
		runHTTPAdapter(ctx, httpAdapterDeps{
			cfg:              cfg,
			pl:               pl,
			sm:               sm,
			eng:              eng,
			verbose:          *verbose,
			sharedPool:       sharedPool,
			reg:              reg,
			maint:            maint,
			startTime:        startTime,
			skillReg:         skillReg,
			skillPkgStore:    skillPkgStore,
			weatherSkill:     weatherSkill,
			memStore:         memStore,
			relStore:         relStore,
			embedder:         embedder,
			profStore:        profStore,
			convStore:        convStore,
			identityResolver: identityResolver,
			sc:               sc,
			promptStore:      promptStore,
			memEvents:        memEvents,
			adapterErrs:      adapterErrs,
		}, &adminH)
	}

	// Built-in skills (RESEARCH-SKILL-SPEC §5): sync the embedded
	// first-party packages into the instance library on every boot —
	// install once, refresh on deploy upgrades, admin toggles kept.
	// Runs after migrations (origin='builtin' needs the widened CHECK)
	// and after ALL skill registration so the advisory allowed-tools
	// mapping sees the full tool set, research workers included.
	// Non-fatal: a failed sync means a missing/stale builtin, not a
	// dead gateway.
	if skillPkgStore != nil {
		syncCtx, syncCancel := context.WithTimeout(ctx, 15*time.Second)
		installed, refreshed, skippedBuiltins, syncErr := skillPkgStore.SyncBuiltins(syncCtx, skillReg.KnownToolNames())
		syncCancel()
		if syncErr != nil {
			log.Printf("[skillpkg] warning: builtin sync: %v", syncErr)
		}
		if len(skippedBuiltins) > 0 {
			log.Printf("[skillpkg] builtins: %d installed, %d refreshed, skipped: %v", installed, refreshed, skippedBuiltins)
		} else {
			log.Printf("[skillpkg] builtins: %d installed, %d refreshed", installed, refreshed)
		}
	}

	// Auto-start Slack when tokens are configured — no flag needed.
	slackReady := cfg.Adapter.Slack.BotToken != "" && cfg.Adapter.Slack.AppToken != ""
	if *useSlack || slackReady {
		adapterCount++
		go func() {
			adapter := slackadapter.New(pl, sm, eng, cfg.Adapter.Slack, *verbose)
			if identityResolver != nil {
				adapter.SetResolver(identityResolver)
			}
			// Durable Slack conversations: persist + hydrate each DM/
			// thread so the bot remembers across turns and sees what a
			// scheduled action posted into the same conversation
			// (SLACK-CONTEXT). No-op without the conversation store.
			if convStore != nil {
				adapter.SetConversations(slackConvStore{cs: convStore})
			}
			// Phase-5 welcome DM references the admin console URL +
			// admin contact from [instance] — hand those to the
			// adapter so bootstrapped users get a useful message
			// with real links instead of placeholders.
			adapter.SetInstance(cfg.Instance)
			adapterErrs <- adapter.Run(ctx)
		}()
		log.Println("[gateway] Slack adapter starting")
	}

	// Wait for the run to end — either an adapter returns an error, or a
	// SIGINT/SIGTERM cancels ctx and the adapters unwind. We log and
	// RETURN (not log.Fatalf/os.Exit) so every deferred teardown runs:
	// stopping the sleep + sidecar goroutines and closing the DB pool,
	// instead of killing them mid-write.
	var runErr error
	if adapterCount == 0 {
		// No network adapters — fall back to interactive CLI.
		adapter := cliadapter.New(pl, sm, eng, cfg.Adapter.CLI, *verbose)
		runErr = adapter.Run(ctx)
	} else {
		// Block until an adapter exits (they normally run until ctx is
		// cancelled by a signal).
		runErr = <-adapterErrs
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		log.Printf("[gateway] adapter error: %v — shutting down", runErr)
	} else {
		log.Printf("[gateway] shutdown signal received — draining")
	}
	// Explicitly stop the consolidation cycle here, before the deferred
	// pool.Close runs, so a mid-pass sleep query can't race the pool
	// closing. Idempotent: the deferred eng.Close() calls Stop() again
	// (a sync.Once no-op).
	inProcMem.Close()
}
