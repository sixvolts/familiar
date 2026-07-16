package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/actions"
	httpAdapter "github.com/familiar/gateway/internal/adapter/native"
	"github.com/familiar/gateway/internal/adapter/shardapi"
	slackadapter "github.com/familiar/gateway/internal/adapter/slack"
	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/ctxbuild"
	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/engine"
	identitypkg "github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/maintenance"
	"github.com/familiar/gateway/internal/media"
	"github.com/familiar/gateway/internal/memevents"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/pageevents"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/push"
	"github.com/familiar/gateway/internal/router"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/shards"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/familiar/gateway/internal/skillpkg"
	"github.com/familiar/gateway/internal/skills"
	researchskill "github.com/familiar/gateway/internal/skills/research"
	scheduledskill "github.com/familiar/gateway/internal/skills/scheduled"
	"github.com/familiar/gateway/internal/skills/weather"
	"github.com/familiar/gateway/internal/userprofile"
	"github.com/familiar/gateway/internal/wikiknowledge"
)

// runHTTPAdapter builds + starts the native HTTP adapter and, when the
// admin console is enabled, mounts the whole console/route surface onto
// it: auth, memory + graph browser, wiki/notes stores, shards, media,
// web push, scheduled actions, and autonomous research. Lifted verbatim
// out of main() (GATEWAY-MAIN-DECOMP) — the body's ordering and closure
// captures are unchanged. Returns the admin.Handler (nil when the console
// is disabled or the pool is missing) so main() can hand it to the wiki
// skill's lazy resolver; the HTTP adapter runs in a background goroutine
// and reports its exit on d.adapterErrs.
func runHTTPAdapter(ctx context.Context, d httpAdapterDeps, adminHOut **admin.Handler) {
	cfg, pl, sm, eng, verbose := d.cfg, d.pl, d.sm, d.eng, d.verbose
	sharedPool, reg, maint, startTime := d.sharedPool, d.reg, d.maint, d.startTime
	skillReg, skillPkgStore, weatherSkill := d.skillReg, d.skillPkgStore, d.weatherSkill
	memStore, relStore, embedder := d.memStore, d.relStore, d.embedder
	profStore, convStore, identityResolver := d.profStore, d.convStore, d.identityResolver
	sc, promptStore, memEvents, adapterErrs := d.sc, d.promptStore, d.memEvents, d.adapterErrs

	var adminH *admin.Handler
	var researchSkill *researchskill.Skill
	var researchRuns *admin.ResearchRunStore
	var pushStore *push.Store

	adapter := httpAdapter.New(pl, sm, eng, cfg.Adapter.HTTP, verbose)

	// 9a. Admin console. Requires the shared pool (for credential and
	// session persistence) and is mounted onto the OpenAI adapter's
	// HTTP server so the whole stack lives behind a single listener.
	if cfg.Admin.Enabled {
		if sharedPool == nil {
			log.Printf("[admin] warning: admin.enabled=true but no DB pool (memory.local_dsn required)")
		} else {
			var adminErr error
			adminH, adminErr = admin.New(cfg.Admin, sharedPool)
			// Publish to the caller's adminH immediately (before the
			// adapter goroutine starts serving below) so the wiki skill's
			// lazy resolver sees the live handler exactly as it did when
			// this block was inline in main(). GATEWAY-MAIN-DECOMP.
			*adminHOut = adminH
			if adminErr != nil {
				log.Printf("[admin] warning: disabled: %v", adminErr)
			} else {
				// Reap expired admin_sessions on a timer — the table
				// grows on every login/renewal otherwise. Stops on
				// ctx cancel (shutdown).
				adminH.StartSessionGC(ctx)
				if memStore != nil {
					adminH.AttachMemoryBrowser(memStore)
					if embedder != nil {
						// Console PATCH re-embeds edited content so
						// the stored vector matches the new text.
						adminH.AttachMemoryEmbedder(embedder)
					}
					log.Printf("[admin] memory browser attached")
				}
				if identityResolver != nil {
					adminH.AttachUserManager(identityResolver)
					log.Printf("[admin] user management attached")
				}
				if sc != nil && memStore != nil && relStore != nil {
					adminH.AttachBackfill(sc, memStore, relStore)
					log.Printf("[admin] relationship backfill attached")
				}
				if relStore != nil {
					// Memory graph view (Phase D). Read-only;
					// per-user scoping inside the handlers
					// matches the rest of the memory surface.
					adminH.AttachGraphStore(relStore)
					log.Printf("[admin] memory graph attached")
				}
				if profStore != nil {
					// User profile panel. GET/PATCH the
					// per-user personality prompt; non-admin
					// writes are forced to the session user.
					adminH.AttachProfileStore(profStore)
					log.Printf("[admin] user profile attached")
				}
				// Chat sessions panel — reads the live
				// process-local session.Manager. No per-deploy
				// toggle; the manager is always constructed.
				adminH.AttachChatSessionLister(sm)
				log.Printf("[admin] chat sessions attached")
				adminH.AttachStatusProvider(&gatewayStatusProvider{
					startTime: startTime,
					registry:  reg,
					memStore:  memStore,
					resolver:  identityResolver,
					sessions:  sm,
					skills:    skillReg,
				})
				log.Printf("[admin] status provider attached")
				// Shards CRUD + tokens UI (Phase 1 Steps 8-9).
				// Requires the shared pool for the shards store;
				// reuses the skill registry so the allowlist
				// checklist stays in lockstep with what the
				// pipeline will actually dispatch.
				// Workspace conversations + messages store
				// (FAMILIAR-WORKSPACE-SPEC Phase 1a). The
				// store itself is constructed earlier so it
				// can also flow into pipeline.Deps for
				// post-restart session hydration; here we
				// just attach the same instance to the admin
				// handler for its REST endpoints.
				if convStore != nil {
					adminH.AttachConversationStore(convStore)
					log.Printf("[admin] conversation store attached")
				}
				// Workspace notes store (FAMILIAR-WORKSPACE-SPEC
				// Phase 2a). Same shared pool. The notes skill
				// switches over to this in Phase 2c.
				if notesStore := admin.NewNotesStore(sharedPool); notesStore != nil {
					adminH.AttachNotesStore(notesStore)
					log.Printf("[admin] notes store ready")
				}
				// Weather skill powers the Home weather widget
				// (/console/api/home/weather). Same instance the
				// LLM tools use, so the TTL cache is shared.
				adminH.AttachWeather(weatherSkill)
				// Public-link sharing policy. Empty PublicHosts in
				// gateway.toml leaves sharing disabled; the toggle
				// returns 503 and /p/{key} returns 404 from every
				// host. Configure [sharing] to enable.
				adminH.AttachSharing(cfg.Sharing)
				if len(cfg.Sharing.PublicHosts) == 0 {
					log.Printf("[admin] sharing disabled: [sharing] public_hosts is empty")
				} else {
					log.Printf("[admin] sharing enabled on %d host(s): %v", len(cfg.Sharing.PublicHosts), cfg.Sharing.PublicHosts)
				}
				// Instance settings — admin-editable runtime config
				// (system-prompt base override + the user-visible
				// toggle). Seeded into memory at boot; the chat hot
				// path never reads the DB for these. The admin
				// handler gets both the store and the live
				// PromptStore so a save can refresh the in-memory
				// base override without a restart.
				if isStore := admin.NewInstanceSettingsStore(sharedPool); isStore != nil {
					adminH.AttachInstanceSettings(isStore, promptStore)
					settingsCtx, settingsCancel := context.WithTimeout(ctx, 5*time.Second)
					if base, sErr := isStore.Get(settingsCtx, admin.SettingSystemPromptBase); sErr != nil {
						log.Printf("[admin] warning: load system_prompt_base: %v", sErr)
					} else if base != "" {
						promptStore.SetBaseOverride(base)
						log.Printf("[admin] system prompt: admin base override loaded (%d bytes)", len(base))
					}
					settingsCancel()
				}
				// Maintenance switch — attach AFTER instance
				// settings so the last-persisted selection +
				// enabled flag rehydrate across restarts.
				adminH.AttachMaintenance(maint)
				// Books + wiki store (BOOKS-WIKI-ARCHITECTURE
				// Phase 1a). Membership-scoped per book; no
				// default book — users create them explicitly.
				//
				// actionsWiki / actionsShards capture the live
				// (hook-installed) store instances for the
				// scheduled-actions wiring below — a second
				// NewWikiStore would miss the page-saved hook
				// and deliveries would stop firing SSE.
				var actionsWiki *admin.WikiStore
				var actionsShards *shards.PGStore
				var actionsPageBus *pageevents.Bus
				if wikiStore := admin.NewWikiStore(sharedPool); wikiStore != nil {
					actionsWiki = wikiStore
					adminH.AttachWikiStore(wikiStore)
					log.Printf("[admin] wiki store ready")
					// Always-on SSE push bus. Editors subscribe
					// per-shell to receive page-saved /
					// page-deleted events for books they're
					// members of; idle devices pick up edits
					// made on another device in near-real-time.
					pageBus := pageevents.NewBus()
					actionsPageBus = pageBus
					adminH.AttachPageEvents(pageBus)
					log.Printf("[admin] page events bus ready (SSE)")
					// Phase 1 step 6: knowledge ingestion
					// pipeline. Hooks fire in goroutines after
					// page commit, so a sidecar that's down
					// or slow doesn't stall the HTTP response.
					// Each collaborator is optional — a nil
					// engine / sidecar / store / relStore /
					// embedder degrades gracefully (skips that
					// step). New() returns nil if everything's
					// missing, in which case we don't bother
					// installing the hooks.
					kp := wikiknowledge.New(wikiknowledge.Deps{
						Engine:      eng,
						Sidecar:     sc,
						MemoryStore: memStore,
						RelStore:    relStore,
						Embedder: func(ctx context.Context, text string) ([]float32, error) {
							if embedder == nil {
								return nil, nil
							}
							return embedder(ctx, text)
						},
					})
					// resolveActorName returns the display string the
					// SSE payload puts in UpdatedBy. For shard
					// writes it's the shard's name (so idle
					// viewers see "Synced — Recipes Bot" rather
					// than the owner's user id); otherwise it's
					// the canonical user's display_name. Both
					// lookups are one-column reads; failures
					// fall back to the raw user id so the field
					// always has something renderable.
					resolveActorName := func(userID, shardID string) string {
						lookupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						defer cancel()
						if shardID != "" {
							var name string
							if err := sharedPool.QueryRowContext(lookupCtx,
								`SELECT name FROM shards WHERE id = $1::uuid`, shardID,
							).Scan(&name); err == nil && name != "" {
								return name
							}
						}
						if identityResolver != nil && userID != "" {
							if u, err := identityResolver.GetUser(lookupCtx, userID); err == nil && u != nil && u.DisplayName != "" {
								return u.DisplayName
							}
						}
						if userID != "" {
							return userID
						}
						return "someone"
					}

					// Hooks are installed unconditionally now —
					// they fan out to (a) the page-events bus
					// for SSE and (b) the knowledge pipeline when
					// present. Either side is nil-safe.
					wikiStore.SetPageSavedHook(func(page admin.WikiPage, bookSlug, userID, shardID string, links []admin.PageLink) {
						pageBus.Publish(pageevents.KindPageSaved, page.BookID, page.ID, pageevents.PageSavedPayload{
							BookSlug:  bookSlug,
							PageSlug:  page.Slug,
							Title:     page.Title,
							UpdatedAt: page.UpdatedAt,
							UpdatedBy: resolveActorName(userID, shardID),
							IsShard:   shardID != "",
						})
						if kp != nil {
							// Async: knowledge extraction is best-effort
							// enrichment and must never block or slow a page
							// write. Running it inline made a research
							// synthesis turn's update_page block on the
							// extract sidecar, pushing the turn past the 300s
							// cap. The SSE publish above stays synchronous so
							// the live evidence view still updates instantly.
							go kp.OnPageSaved(context.Background(), wikiknowledge.SaveEvent{
								BookID: page.BookID, BookSlug: bookSlug,
								PageID: page.ID, PageSlug: page.Slug,
								UserID: userID,
								Title:  page.Title, Content: page.Content,
								Links: links,
							})
						}
					})
					wikiStore.SetPageDeletedHook(func(bookID, bookSlug, pageID, pageSlug string) {
						pageBus.Publish(pageevents.KindPageDeleted, bookID, pageID, pageevents.PageDeletedPayload{
							BookSlug: bookSlug,
							PageSlug: pageSlug,
						})
						if kp != nil {
							kp.OnPageDeleted(context.Background(), wikiknowledge.DeleteEvent{
								BookID: bookID, BookSlug: bookSlug,
								PageID: pageID, PageSlug: pageSlug,
							})
						}
					})
					if kp != nil {
						log.Printf("[admin] wiki knowledge pipeline installed")
					}

					// Research workers (RESEARCH-SKILL-SPEC §6.2):
					// spawn_research_workers fans deep-research
					// sub-questions out to parallel virtual-shard
					// pipeline turns. Registered late, like the
					// scheduled skill below — the Invoke closure
					// needs the constructed pipeline, and the
					// backend is the live (hook-installed) wiki
					// store attached above, so worker appends fire
					// the same SSE / knowledge hooks interactive
					// saves do. Gated on web_search actually being
					// registered: a worker without search can only
					// re-read the evidence page, so a search-less
					// deploy skips the tool with a loud log
					// instead of shipping it broken.
					if cfg.Skills.Research.Enabled {
						// Model pins are validated against [[models]] up
						// front: a typo'd ID would otherwise surface as a
						// router error on every worker turn. Workers need
						// a tools-capable model; the writer is a pure
						// completion, so any registry model qualifies.
						workerModel := cfg.Skills.Research.WorkerModel
						if workerModel != "" {
							if m, ok := modelByID(cfg.Models, workerModel); !ok {
								log.Printf("[research] worker_model %q is not in [[models]] — falling back to worker_tier routing", workerModel)
								workerModel = ""
							} else if !slices.Contains(m.Capabilities, "tools") {
								log.Printf("[research] worker_model %q lacks the \"tools\" capability — workers can't search with it; falling back to worker_tier routing", workerModel)
								workerModel = ""
							}
						}
						writerModel := cfg.Skills.Research.WriterModel
						if writerModel != "" {
							if _, ok := modelByID(cfg.Models, writerModel); !ok {
								log.Printf("[research] writer_model %q is not in [[models]] — compose_research_note will not be offered", writerModel)
								writerModel = ""
							}
						}
						rSkill := researchskill.New(researchskill.Options{
							Invoke:             pl.HandleShard,
							Sessions:           sm,
							Backend:            wikiStore,
							MaxWorkers:         cfg.Skills.Research.MaxWorkers,
							WorkerSearchBudget: cfg.Skills.Research.WorkerSearchBudget,
							WorkerTier:         cfg.Skills.Research.WorkerTier,
							WorkerModel:        workerModel,
							WriterModel:        writerModel,
							MaxRounds:          cfg.Skills.Research.MaxRounds,
						})
						if !skillReg.KnownToolNames()["web_search"] {
							log.Printf("[research] skills.research.enabled=true but web_search is not registered ([tools.brave] off?) — workers need search; skipping registration")
						} else if err := skillReg.Register(rSkill); err != nil {
							log.Printf("[skills] warning: register research skill: %v", err)
						} else {
							researchSkill = rSkill // autonomy wired later (§6.7)
							researchRuns = admin.NewResearchRunStore(sharedPool)
							log.Printf("[skills] research registered (max_workers=%d, search_budget=%d, tier=%s, worker_model=%q, writer_model=%q, max_rounds=%d)",
								cfg.Skills.Research.MaxWorkers, cfg.Skills.Research.WorkerSearchBudget, cfg.Skills.Research.WorkerTier, workerModel, writerModel, cfg.Skills.Research.MaxRounds)

							// Evidence sweep — only once the skill is
							// actually live (gated inside the success
							// branch): deep-tier runs leave one page per
							// run in the hidden per-user research book, and
							// the books are hidden so nothing else reaps
							// them. This bounds their growth (§6.6). Tied
							// to the signal context so it exits on shutdown
							// instead of leaking on <-tick.C.
							if retH := cfg.Skills.Research.EvidenceRetentionHours; retH > 0 {
								retention := time.Duration(retH) * time.Hour
								go func() {
									tick := time.NewTicker(6 * time.Hour)
									defer tick.Stop()
									for {
										swCtx, cancelSw := context.WithTimeout(ctx, 5*time.Minute)
										if n, err := wikiStore.SweepResearchEvidence(swCtx, retention); err != nil {
											log.Printf("[research] evidence sweep: %v", err)
										} else if n > 0 {
											log.Printf("[research] evidence sweep reaped %d page(s) older than %s", n, retention)
										}
										cancelSw()
										select {
										case <-tick.C:
										case <-ctx.Done():
											return
										}
									}
								}()
							}
						}
					}
				}

				if shardStore, shardErr := shards.NewStore(sharedPool); shardErr == nil {
					actionsShards = shardStore
					adminH.AttachShardStore(shardStore)
					// SHARD-AUTH-SPEC Phase 1: shard passkey
					// store rides on the same pool, exposed
					// through the admin handler for the four
					// CRUD endpoints + the unified login path.
					if shardPasskeys := admin.NewShardPasskeyStore(sharedPool); shardPasskeys != nil {
						adminH.AttachShardPasskeyStore(shardPasskeys)
					}
					// Project the registry into the narrow
					// catalog interface the admin package
					// consumes — avoids leaking the full
					// skills.Registry type into internal/admin.
					// Three projections:
					//  - toolNames: flat list for the shard
					//    allowlist checklist
					//  - knownToolNames: lookup set for save-time
					//    validation
					//  - skillInfos: structured per-skill view
					//    for the Phase C SKILLS catalog panel
					var toolNames []string
					for _, t := range skillReg.ToolDefinitions() {
						toolNames = append(toolNames, t.Name)
					}
					var skillInfos []admin.SkillInfo
					for _, name := range skillReg.SkillNames() {
						sk, ok := skillReg.Get(name)
						if !ok {
							continue
						}
						info := admin.SkillInfo{
							Name:        sk.Name(),
							Description: sk.Description(),
							Version:     sk.Version(),
						}
						for _, t := range sk.Tools() {
							info.Tools = append(info.Tools, admin.SkillToolInfo{
								Name:        t.Name,
								Description: t.Description,
								Parameters:  t.Parameters,
							})
						}
						skillInfos = append(skillInfos, info)
					}
					adminH.AttachSkillCatalog(admin.NewSkillCatalog(toolNames, skillReg.KnownToolNames(), skillInfos))
					if skillPkgStore != nil {
						adminH.AttachSkillPackages(skillPkgStore, skillReg.KnownToolNames())
					}
					// Web Push (PWA notifications). The store backs the
					// subscribe endpoints; the Sender (built in the
					// actions-deliverer block below) signs + delivers.
					// Off unless [push] VAPID keys are configured.
					if cfg.Push.Enabled() {
						pushStore = push.NewStore(sharedPool)
						adminH.AttachPush(pushStore, cfg.Push.VAPIDPublicKey)
						log.Printf("[push] enabled (VAPID configured); subscribe endpoints live")
					} else {
						log.Printf("[push] disabled — set [push] vapid keys to enable (generate with --gen-vapid)")
					}

					// Page media (MEDIA-DIAGRAMS Phase 1): images
					// on the filesystem, metadata in page_media.
					// The sweep reaps files orphaned by page
					// deletes (rows CASCADE; bytes don't).
					if mediaStore, mErr := media.NewStore(sharedPool, cfg.Media.Dir, int64(cfg.Media.MaxUploadMB)<<20); mErr == nil {
						adminH.AttachMedia(mediaStore)
						log.Printf("[media] page-media store at %s (max %dMB)", cfg.Media.Dir, cfg.Media.MaxUploadMB)
						go func() {
							tick := time.NewTicker(24 * time.Hour)
							defer tick.Stop()
							for {
								sweepCtx, cancelSweep := context.WithTimeout(context.Background(), 5*time.Minute)
								if n, err := mediaStore.SweepOrphans(sweepCtx, 24*time.Hour); err != nil {
									log.Printf("[media] orphan sweep: %v", err)
								} else if n > 0 {
									log.Printf("[media] orphan sweep removed %d file(s)", n)
								}
								cancelSweep()
								<-tick.C
							}
						}()
					} else {
						log.Printf("[media] warning: store unavailable: %v", mErr)
					}
					log.Printf("[admin] shards CRUD attached (%d skills / %d tools in catalog)",
						len(skillInfos), len(toolNames))

					// MODEL-SELECTOR: project router.Registry into
					// the narrow ModelCatalog interface the admin
					// /console/api/models endpoint consumes. The
					// console UI's "..." → Models submenu reads
					// from this list to render the model picker.
					adminH.AttachModelCatalog(modelCatalog{reg: reg})
				} else {
					log.Printf("[admin] warning: shards CRUD disabled: %v", shardErr)
				}
				// Scheduled actions (SCHEDULED-ACTIONS-SPEC
				// Phase 1): DB-backed recurring work, shard-
				// enveloped when bound, delivering to Slack /
				// pages / the log with a run ledger. Wired only
				// when the pool exists (it does — we're inside
				// the admin block); the page deliverer reuses
				// the hook-installed wikiStore so appends fire
				// the same SSE path interactive saves do.
				if actionsStore, aErr := actions.NewStore(sharedPool); aErr == nil {
					// Recall tool: let the LLM read the user's own
					// scheduled-action run history (verbatim past
					// digests) so it can answer "what did you send
					// me?" / "same as yesterday?" across days and
					// surfaces. Registered late because it needs the
					// actions store; the pipeline reads the registry
					// at request time so this is visible. SLACK-CONTEXT.
					if err := skillReg.Register(scheduledskill.New(actionsStore)); err != nil {
						log.Printf("[skills] warning: register scheduled skill: %v", err)
					} else {
						log.Printf("[skills] scheduled (recent_scheduled_runs) registered")
					}
					deliverers := map[string]actions.DeliverFunc{
						"log": func(_ context.Context, ownerID, _ string, _ actions.Target, actionName, text string) error {
							log.Printf("[actions] %s (owner %s) report:\n%s", actionName, ownerID, text)
							return nil
						},
						// "Nowhere" — deliver nothing. The run + its output
						// are still in the ledger; this just suppresses any
						// external report (no note, no thread, no log line).
						"none": func(_ context.Context, _, _ string, _ actions.Target, _, _ string) error {
							return nil
						},
					}
					if cfg.Adapter.Slack.BotToken != "" {
						if slackSender, sErr := slackadapter.NewSender(cfg.Adapter.Slack.BotToken, cfg.Adapter.Slack.APIBaseURL); sErr == nil {
							deliverers["slack"] = func(ctx context.Context, _, _ string, t actions.Target, _, text string) error {
								return slackSender.SendProactive(ctx, t.ChannelID, text)
							}
							// slack_dm DMs the action OWNER via their
							// linked Slack identity, resolved at
							// delivery time — no channel to configure;
							// an unlinked owner gets a clear delivery
							// error instead of a silent drop.
							if identityResolver != nil {
								resolverRef := identityResolver
								convRef := convStore // may be nil — persistence is best-effort
								deliverers["slack_dm"] = func(ctx context.Context, ownerID, _ string, _ actions.Target, actionName, text string) error {
									links, err := resolverRef.ListIdentitiesForUser(ctx, ownerID)
									if err != nil {
										return fmt.Errorf("resolve slack identity: %w", err)
									}
									slackID := ""
									for _, l := range links {
										if l.Platform == "slack" {
											slackID = l.PlatformID
											break
										}
									}
									if slackID == "" {
										return fmt.Errorf("no Slack identity linked for user %s — link one to receive DMs", ownerID)
									}
									if err := slackSender.SendDM(ctx, slackID, text); err != nil {
										return err
									}
									// Mirror the digest into the owner's DM
									// conversation (same external key the adapter
									// hydrates) so a reply in Slack continues with
									// the digest in context. This conversation is
									// external_key-tagged, so it does NOT surface
									// in the workspace chat list (listConversations
									// filters those out) — context lives in Slack,
									// not the chat list. Best-effort: the DM
									// already went out, so a persistence miss must
									// not fail the delivery. SLACK-CONTEXT.
									if convRef != nil {
										conv, cerr := convRef.EnsureExternalConversation(ctx, ownerID, slackadapter.DMExternalKey(ownerID), "Slack DM")
										if cerr != nil {
											log.Printf("[actions] slack_dm: ensure DM conversation for %s (delivered, not persisted): %v", ownerID, cerr)
											return nil
										}
										if _, aerr := convRef.AppendMessage(ctx, &admin.Message{
											ConversationID: conv.ID,
											Role:           "assistant",
											Content:        text,
											Model:          "scheduled:" + actionName,
										}); aerr != nil {
											log.Printf("[actions] slack_dm: persist digest for %s (delivered, not persisted): %v", ownerID, aerr)
										}
									}
									return nil
								}
							} else {
								log.Printf("[actions] slack_dm deliverer unavailable: identity resolver not ready")
							}
						} else {
							log.Printf("[actions] warning: slack deliverer unavailable: %v", sErr)
						}
					}
					if convStore != nil {
						convRef := convStore
						actionsRef := actionsStore
						// ensureActionThread returns a live conversation id for
						// a conversation/push target, recreating the thread if
						// the user deleted it. On recreate it repoints the
						// action's stored target to the new id so subsequent
						// runs reuse it instead of spawning a thread each time.
						ensureActionThread := func(ctx context.Context, ownerID, actionID, convID, actionName string) (string, error) {
							if convID != "" {
								owns, err := convRef.OwnsConversation(ctx, convID, ownerID)
								if err != nil {
									return "", fmt.Errorf("conversation ownership: %w", err)
								}
								if owns {
									return convID, nil
								}
							}
							// Missing (deleted) or never set — mint a fresh thread.
							conv, err := convRef.Create(ctx, ownerID, "Scheduled: "+actionName, "familiar")
							if err != nil {
								return "", fmt.Errorf("recreate conversation: %w", err)
							}
							if a, gerr := actionsRef.Get(ctx, actionID, ownerID, false); gerr == nil {
								changed := false
								for i := range a.ReportTargets {
									if (a.ReportTargets[i].Kind == "conversation" || a.ReportTargets[i].Kind == "push") &&
										a.ReportTargets[i].ConversationID != conv.ID {
										a.ReportTargets[i].ConversationID = conv.ID
										changed = true
									}
								}
								if changed {
									if _, uerr := actionsRef.Update(ctx, a, false); uerr != nil {
										log.Printf("[actions] %s: repoint conversation failed (delivered to new thread %s): %v", actionID, conv.ID, uerr)
									}
								}
							}
							return conv.ID, nil
						}
						deliverers["conversation"] = func(ctx context.Context, ownerID, actionID string, t actions.Target, actionName, text string) error {
							convID, err := ensureActionThread(ctx, ownerID, actionID, t.ConversationID, actionName)
							if err != nil {
								return err
							}
							_, err = convRef.AppendMessage(ctx, &admin.Message{
								ConversationID: convID,
								Role:           "assistant",
								Content:        text,
								Model:          "scheduled:" + actionName,
							})
							return err
						}

						// push: same as conversation (append the output
						// to the action's thread) PLUS a Web Push that
						// deep-links to it, for users who get notified on
						// the PWA instead of Slack. Requires [push].
						if pushStore != nil && cfg.Push.Enabled() {
							pushSender := push.NewSender(pushStore, cfg.Push.VAPIDPublicKey, cfg.Push.VAPIDPrivateKey, cfg.Push.Subject)
							deliverers["push"] = func(ctx context.Context, ownerID, actionID string, t actions.Target, actionName, text string) error {
								convID, err := ensureActionThread(ctx, ownerID, actionID, t.ConversationID, actionName)
								if err != nil {
									return err
								}
								if _, err := convRef.AppendMessage(ctx, &admin.Message{
									ConversationID: convID,
									Role:           "assistant",
									Content:        text,
									Model:          "scheduled:" + actionName,
								}); err != nil {
									return err
								}
								// Notify is best-effort: the output is
								// already saved in the thread, so a push
								// failure (no devices, dead endpoint)
								// must NOT fail the delivery.
								n, perr := pushSender.Send(ctx, ownerID, push.Payload{
									Title: actionName,
									Body:  pushPreview(text),
									URL:   "/#chat/" + convID,
									Tag:   "action:" + convID,
								})
								if perr != nil {
									log.Printf("[actions] push: notify %s (delivered to thread, push errored): %v", ownerID, perr)
								} else if n == 0 {
									log.Printf("[actions] push: %s has no push devices (delivered to thread only)", ownerID)
								}
								return nil
							}

							// "notify": push-only modifier. The action's real
							// destination target already delivered the output;
							// this just pings the owner, deep-linking to the
							// run history (where the output always lives).
							// Best-effort — never fails the run.
							deliverers["notify"] = func(ctx context.Context, ownerID, actionID string, _ actions.Target, actionName, text string) error {
								n, perr := pushSender.Send(ctx, ownerID, push.Payload{
									Title: actionName,
									Body:  pushPreview(text),
									URL:   "/#scheduled/" + actionID,
									Tag:   "action:" + actionID,
								})
								if perr != nil {
									log.Printf("[actions] notify: push for %s errored (best-effort): %v", ownerID, perr)
								} else if n == 0 {
									log.Printf("[actions] notify: %s has no push devices", ownerID)
								}
								return nil
							}
						} else {
							log.Printf("[actions] push deliverer unavailable: push not configured")
						}
					}
					if actionsWiki != nil {
						wikiRef := actionsWiki
						deliverers["page"] = func(ctx context.Context, ownerID, _ string, t actions.Target, actionName, text string) error {
							var b *admin.Book
							var err error
							if t.BookSlug == "personal" {
								b, err = wikiRef.EnsurePersonalBook(ctx, ownerID)
							} else {
								b, err = wikiRef.GetBookBySlug(ctx, t.BookSlug, ownerID, false)
							}
							if err != nil {
								return fmt.Errorf("resolve book %q: %w", t.BookSlug, err)
							}
							cur, err := wikiRef.GetPageByID(ctx, b.ID, t.PageID)
							if err != nil {
								return fmt.Errorf("resolve page: %w", err)
							}
							section := "## " + time.Now().Format("2006-01-02 15:04") + " — " + actionName + "\n\n" + text
							newBody := section
							if cur.Content != "" {
								newBody = strings.TrimRight(cur.Content, "\n") + "\n\n" + section
							}
							actorCtx := admin.WithPageActor(ctx, admin.PageActor{UserID: ownerID})
							_, err = wikiRef.UpdatePage(actorCtx, b.ID, cur.Slug, ownerID, admin.PagePatch{Content: &newBody})
							return err
						}
					}
					runner, rErr := actions.NewRunner(actions.Deps{
						Store:    actionsStore,
						Sessions: sm,
						Invoke: func(ctx context.Context, sess *session.Session, prompt string, ov *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
							if ov != nil {
								return pl.HandleShard(ctx, sess, prompt, ov)
							}
							return pl.Handle(ctx, sess, prompt, nil)
						},
						GetShard: func(ctx context.Context, id string) (*shards.Shard, error) {
							if actionsShards == nil {
								return nil, fmt.Errorf("shards not configured")
							}
							return actionsShards.GetShard(ctx, id)
						},
						UserStatus: func(ctx context.Context, userID string) (string, error) {
							if identityResolver == nil {
								return "approved", nil
							}
							u, err := identityResolver.GetUser(ctx, userID)
							if err != nil {
								return "", err
							}
							return string(u.Status), nil
						},
						Deliverers: deliverers,
						PageEvents: actionsPageBus,
					})
					if rErr != nil {
						log.Printf("[actions] warning: runner: %v", rErr)
					} else {
						actionsStore.OnChange = func() {
							reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 10*time.Second)
							if err := runner.Reload(reloadCtx); err != nil {
								log.Printf("[actions] reload: %v", err)
							}
							reloadCancel()
						}
						if err := runner.Start(ctx); err != nil {
							log.Printf("[actions] warning: start: %v", err)
						} else {
							adminH.AttachActions(actionsStore, runner)
							defer runner.Stop()
							log.Printf("[actions] scheduled-actions runner started")
						}
					}
				} else {
					log.Printf("[actions] warning: store: %v", aErr)
				}

				// Autonomous research runs (§6.7): late-wire the skill's
				// orchestrator now that convStore / pushStore / the run
				// store all exist. Without this the deep tier still works
				// synchronously ("say continue"); with it, runs self-drive
				// to a delivered note + mobile push.
				if researchSkill != nil && researchRuns != nil && convStore != nil && adminH.WikiStore() != nil {
					var researchPush *push.Sender
					if pushStore != nil && cfg.Push.Enabled() {
						researchPush = push.NewSender(pushStore, cfg.Push.VAPIDPublicKey, cfg.Push.VAPIDPrivateKey, cfg.Push.Subject)
					}
					researchSkill.SetOrchestrator(researchRuns, makeResearchSynthesizer(
						researchSkill, pl, sm, adminH.WikiStore(), convStore, researchRuns, researchPush))
					adminH.AttachResearchRuns(researchRuns)
					adminH.AttachResearchCanceller(researchSkill.CancelRun)
					// Reconcile runs orphaned by the previous process's
					// exit: their in-memory goroutines are gone, so mark
					// them failed to unblock those conversations (§6.7).
					if n, rErr := researchRuns.FailOrphanedRuns(ctx, "interrupted by a gateway restart"); rErr != nil {
						log.Printf("[research] orphan reconcile: %v", rErr)
					} else if n > 0 {
						log.Printf("[research] reconciled %d run(s) orphaned by restart", n)
					}
					log.Printf("[research] autonomous runs wired (push=%v)", researchPush != nil)
				}

				adapter.SetAdminHandler(adminH.Mux(nil))
				adapter.SetSessionReader(adminH)
				// Gate client-supplied conversation_id on ownership
				// before it's bound to a session (EXTERNAL-READINESS
				// P0). Guarded on non-nil so a typed-nil store never
				// reaches the interface.
				if convStore != nil {
					adapter.SetConversationOwner(convStore)
					// Owner-side shard chat (SKILL-PACKAGES-SPEC
					// Phase 1): conversations bound to a shard run
					// through its envelope. Policy lives in
					// adminH.ResolveShardChat; this closure only
					// adapts the result to the adapter's type.
					adapter.SetShardChatResolver(func(ctx context.Context, conversationID, userID string) (*httpAdapter.ShardChatTarget, string, error) {
						info, refusal, err := adminH.ResolveShardChat(ctx, conversationID, userID)
						if err != nil || refusal != "" || info == nil {
							return nil, refusal, err
						}
						return &httpAdapter.ShardChatTarget{
							ShardID:   info.ShardID,
							Ephemeral: info.Ephemeral,
							Overrides: info.Overrides,
						}, "", nil
					})
				}
				log.Printf("[admin] console enabled (rp_id=%s, origins=%d)",
					cfg.Admin.RPID, len(cfg.Admin.RPOrigins))
			}
		}
	}

	// 9b. Shards HTTP invocation endpoint. Requires the shared pool
	// (for the shards + tokens tables), the pipeline, the session
	// manager, and the identity resolver. Any missing dependency
	// disables the mount — shards simply don't take HTTP invocations
	// on that deploy, which is the safe default.
	if sharedPool != nil && identityResolver != nil {
		shardStore, shardErr := shards.NewStore(sharedPool)
		if shardErr != nil {
			log.Printf("[shards] warning: disabled: %v", shardErr)
		} else {
			shardsH := shardapi.New(shardStore, pl, sm, identityResolver)
			adapter.SetShardsHandler(shardsH)
			log.Printf("[shards] invocation endpoint enabled at /v1/shards/{id}/invoke")
		}
	} else {
		log.Printf("[shards] warning: disabled (requires memory.local_dsn + identity resolver)")
	}

	adapter.SetMemEvents(memEvents)

	go func() {
		adapterErrs <- adapter.Run(ctx)
	}()
	log.Println("[gateway] OpenAI adapter starting")

}

// httpAdapterDeps bundles the collaborators runHTTPAdapter needs. It's a
// parameter object, NOT an app-struct: it exists only to keep the call
// site readable in place of ~20 positional args. main() still owns every
// field's construction and lifetime.
type httpAdapterDeps struct {
	cfg              *config.Config
	pl               *pipeline.Pipeline
	sm               *session.Manager
	eng              engine.Service
	verbose          bool
	sharedPool       *db.Pool
	reg              *router.Registry
	maint            *maintenance.Controller
	startTime        time.Time
	skillReg         *skills.Registry
	skillPkgStore    *skillpkg.Store
	weatherSkill     *weather.Skill
	memStore         *memory.PgVectorStore
	relStore         *memory.PgRelationshipStore
	embedder         pipeline.EmbedFunc
	profStore        *userprofile.Store
	convStore        *admin.ConversationStore
	identityResolver *identitypkg.Resolver
	sc               *sidecar.Client
	promptStore      *ctxbuild.PromptStore
	memEvents        *memevents.Bus
	adapterErrs      chan error
}
