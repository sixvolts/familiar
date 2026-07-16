# Review backlog — P2 and architecture

Captured from the 2026-07 top-to-bottom review (six parallel focused
passes: security/authz, memory, skills, pipeline, frontend, data
layer). **P0 and P1 findings were fixed** in commits `b9fed79`,
`055ee58`, `2a620a1`, `54cd0fe` — this file tracks everything
deliberately deferred, plus the architecture assessment.

Severity legend matches the review: these are hygiene / scaling /
ops-debt items, not broken boundaries. Calibrated to a single-admin,
local-first, mostly-trusted deployment.

---

## Done since the review — SSE turn preservation

~~Interrupted SSE stream persists nothing.~~ **Resolved** (commit
`9ef7ee8`) by the "finish the turn" approach rather than salvaging a
partial: once generation starts, the turn runs on a detached
`turnContext` that survives client disconnect (best-effort stream) but
still yields to a 300s cap and gateway shutdown, so the *whole* answer
finishes and persists. A reconnecting client sees the full turn.
Trade-off accepted: an abandoned turn holds its inflight slot until it
finishes (compute-only on a local model).

---

## Done since the review

- ~~Backup tooling~~ — `scripts/backup-db.sh` + `familiar-backup.timer`
  (nightly `pg_dump -Fc`, retention, restore docs). Commit `0d16db0`.
- ~~`DeleteMemory` / `DeleteMemoriesBySource` self-FK~~ — all three
  delete paths detach children in-CTE. Commit `8dbcc81`.
- ~~No panic recovery in pipeline background goroutines~~ —
  `recoverBackground` on `runSummarize`/`runPostTurnExtract`. Commit
  `8dbcc81`.
- ~~Relationship graph ignores shard/book isolation~~ — extraction
  stamps `scope_tag`; `RelatedForContents`/`TraverseFrom` (the prompt
  path) exclude isolated-shard triples. Commit `3975667`.
- ~~Migrations that re-fire data UPDATEs every boot~~ (auto-approve,
  owner→admin) — gated behind `applied_data_fixes`. Commit `a01eb78`.
- ~~`NearestLiveFact`/`NearestSimilarity` no source_type filter~~ —
  exclude conversation + wiki_page. Commit `7c7fa0f`.
- ~~Preamble defeats the empty-response commit guard~~ — commit gates
  on `llmResp.Content`. Commit `7c7fa0f`.
- ~~`use_skill` body uncapped for imported skills~~ — `capSkillBody` at
  read time. Commit `ba04ea8`.
- ~~No mid-tool-loop token budgeting~~ — `budgetToolResult` caps each
  result and the turn's cumulative tool tokens. Commit `ba04ea8`.

## P2 — correctness hygiene (memory subsystem)

Most of these are the "versions/relationships legs" of the data model
being decorative rather than load-bearing (see Architecture below).
- **`NearestLiveFact` / `NearestSimilarity` have no source_type filter,**
  so the write-time conflict resolver can pick a `wiki_page` (or
  `conversation`) row as an UPDATE target; the wiki clean-replace then
  FK-fails. Add `source_type NOT IN ('conversation','wiki_page')` to
  mirror retrieval + sleep-dedup exclusions. (`internal/memory/pgvector.go`)
- **`UpdateMemoryContent` is non-transactional** (read → count → seed
  version → UPDATE → record version, all autocommit) and doesn't update
  `content_hash`, so a later commit of the *old* text collides onto the
  edited row. Wrap in a tx; recompute the hash (with `factHash`).
  (`internal/memory/pgvector_admin.go`)
- **`RecordVersion` uses `MAX(version)+1` with no unique constraint** on
  `(memory_id, version)` — concurrent edits mint duplicate version
  numbers. Add the unique index or an in-tx sequence.
- **`memory_versions` lineage is wiped by `CollapseChain` / `DeleteMemory`**
  (CASCADE) and the UPDATE path records lineage on the *old* row that
  collapse deletes. If versions are meant as an audit trail (which
  CollapseChain's own comment assumes), re-parent versions to the tip
  before delete, or record UPDATE lineage under the new row's id.
- **Relationship graph ignores shard/book isolation.** `scope_tag` is
  written on relationships but **no** read query filters it
  (`RelatedForContents`, `TraverseFrom`, `GraphAround`, `GraphTop`,
  `SearchEntities`), and chat extraction doesn't even stamp it. An
  `isolated` shard's memories are excluded from retrieval, but the
  triples extracted from those same turns flow into the top-level
  prompt via graph augmentation — the isolation boundary leaks through
  one of its two channels. Stamp `ScopeTag` in `runPostTurnExtract` and
  add the isolation predicate to the read queries.
  (`internal/pipeline/summarize.go`, `internal/memory/relationships.go`)
- **Relationship provenance is approximate.** Every triple from a turn
  gets `SourceFact = pbFacts[0].Id` (the first committed fact,
  arbitrarily), is `""` when all candidates were DUPLICATE-skipped, and
  is never re-pointed when its source fact is superseded — so
  `FactsForEntity` loses facts as chains grow. (`internal/pipeline/summarize.go`)
- **`MergeEntity` (backfill variant) aborts on a subject-rewrite unique
  collision** (contrary to its comment) and its dedup DELETE is
  table-wide (unscoped by user). Adopt `MergeEntities`' pre-delete;
  scope the dedup to the touched `(user, canonical)` rows.
  (`internal/memory/relationships.go`)
- **`ChainForMemory` diamond dedup** can emit the same memory at two
  depths (duplicate rows in the output slice). Cosmetic. (`pgvector_admin.go`)
- **`ON CONFLICT` DO UPDATE drops `supersedes`** — a correction back to a
  previously-stored value hash-collides onto the old (still-hidden)
  row and the intended supersede stamp is lost. `COALESCE(EXCLUDED.
  supersedes, memories.supersedes)`. (`internal/memengine/memengine.go`)

## P2 — pipeline / runtime

- **No mid-tool-loop token budgeting or result truncation.** Each
  iteration appends `result.Content` raw across up to `maxIters`
  rounds; `ctxbuild`'s `MaxToolResultTokens` cap applies only to
  *prefetch* results. Wiki/memory/notes results are unbounded →
  overflow becomes a hard provider 400. Apply a per-result cap at
  append time + a running-estimate early exit. (`internal/pipeline/pipeline.go`)
- **Token-budget eviction can orphan tool messages.** `fitConversation`
  and the `MaxSessionTurns` cap cut turn sequences at arbitrary
  boundaries; a kept `tool` message whose assistant-with-`tool_calls`
  partner was evicted survives `sanitizeToolHistory` and crashes the
  provider template. Make eviction tool-boundary-aware, or extend
  sanitize to drop tool messages with no preceding `tool_calls`.
  (`internal/ctxbuild/builder.go`, `internal/session/manager.go`, `internal/llm/openai.go`)
- **No panic recovery in pipeline background goroutines.** A panic in
  `runPostTurnExtract` / `runSummarize` (which parse model-shaped
  output) takes down the whole gateway; only the wiki hooks recover.
  Add a `defer recover()` wrapper matching `internal/admin/wiki.go`.
- **Preamble defeats the empty-response commit guard.** When the heavy
  model returns empty (thinking ate `max_tokens`), `responseText =
  preamble + ""` is non-empty, so a preamble-plus-`---` turn with no
  answer commits + extracts. Gate the commit on `llmResp.Content`.
  (`internal/pipeline/pipeline.go`)
- **`use_skill` body uncapped for imported skills.** `read_skill_file`
  and user-authored bodies cap at 256KB, but an imported SKILL.md can
  be up to the 5MB per-file zip cap and goes into the tool-loop verbatim.
  Cap SKILL.md at import or truncate in `use_skill`.
  (`internal/skills/skillpacks/`, `internal/skillpkg/`)
- **Registry `Close` doesn't terminate.** It calls each skill's `Close`
  but leaves the maps populated and sets no closed flag, so a late
  `Execute` during shutdown dispatches into a closed skill. Set a
  `closed` bool under the lock. (`internal/skills/skills.go`)
- **`/v1/shards/{id}/invoke` has no rate/concurrency cap** (the
  `/api/chat` path has `acquireSlot`). One valid shard token can open
  unbounded concurrent invocations against the shared local model.
  (`internal/adapter/shardapi/shardapi.go`)

## P2 — data layer / ops

- **No backup tooling at all.** The DB is the sole copy of memories,
  wiki, notes, credentials. A nightly `pg_dump -Fc` systemd timer +
  retention is ~20 lines and is arguably the biggest *operational* gap.
- **No ANN index on `memories.embedding`.** Every retrieval and the
  O(n²) sleep dedup are sequential scans (deliberate today — the
  column is dimensionless). Add optional HNSW creation keyed off the
  embedder dimension before the store grows. (`internal/db/migrate.go`)
- **`RelatedForContents` full-scans `relationships`** on the retrieval
  hot path (`WHERE position(subject IN $2) > 0` is unindexable). Route
  through the existing `EntityVocab` cache (match in Go → indexed
  `subject = ANY(...)`). (`internal/memory/relationships.go`)
- **Migrations that re-fire data UPDATEs every boot** (now that the wiki
  slug one is gated): `users_backfill` auto-approves, `add_user_roles_
  and_email` re-asserts owner→admin + resurrects cleared emails,
  `memories_user_id_nonempty` drops+revalidates a CHECK on the
  fastest-growing table each boot. Fold data fixes behind the new
  `applied_data_fixes` marker table (introduced for the slug fix); use
  `NOT VALID` for the CHECK re-add. (`internal/db/migrate.go`)
- **Unbounded-growth tables with no retention:** `scheduled_action_runs`
  (~105k rows/yr at a 5-min cron), `wiki_revisions`, `memory_versions`,
  rolling-summary `sessions` rows. Add sweeps as they matter.
- **`scheduled_action_runs`** append-only ledger wants a retention sweep.
- **Config env-expansion inconsistency:** `Embedder.Endpoint` expands
  `$ENV`, `Rerank.Endpoint` doesn't. (`internal/config`)
- **`publish-public.sh` built-in secret patterns** miss `postgres://
  user:pass@` DSNs, VAPID/base64 blobs, and non-generic API keys;
  gitleaks + `.publish-deny` are both optional-with-warning. Consider
  failing hard when the deny file is absent on non-CI runs.

## P2 — frontend

- **Document-level listener leaks.** `chat.js:368`, `notes.js:413`,
  `wiki.js:458`, and `wiki.js:1224` (per book-splash render) add
  `document.addEventListener("click", …)` inside per-shell/per-render
  builders and never remove them → detached shell DOM leaks per tab /
  per navigation. Adopt one delegated dismiss listener per module, or a
  per-shell `teardown()` contract (wiki already has one).
- **Service worker vendor-staleness.** `toastui-editor-all.min.js` /
  `mermaid.min.js` have no `?v=` and are precached cache-first forever;
  updating a vendor file without bumping `CACHE` ships fresh app JS
  against a stale lib. Put `?v=` on vendor tags or use
  stale-while-revalidate for `/vendor/*`. (`sw.js`, `mobile.html`)
- **One unescaped interpolation:** `panels/scheduled.js:207` builds an
  `<option>` with an unescaped book slug (CSP + server slug constraints
  blunt it). Build with `textContent`.
- **Mobile send-button id mismatch:** `getElementById('mob-thread-send')`
  but the element only has the *class* `mob-thread-send`, so it's always
  null and the streaming-disable is a no-op (double-send is still
  blocked by `state.streaming`). (`mobile.js:580`, `mobile.html:258`)
- **Mobile hash-route decode is uncaught:** `decodeURIComponent(p.detail)`
  throws on a malformed fragment (e.g. `#memory/50%`); wrap + fall back
  to the memory list. (`mobile.js:283`)
- **Dead code:** `familiarStatusBar.setContext` writes to a
  non-existent `#statusbar-context` (whole feature is a no-op);
  `sidebar-count-*` badges are never populated. Add the elements or
  delete the plumbing.
- **Overlay/Escape stack:** the global keydown closes memory/entity/user
  detail whenever non-hidden, so one Escape can close two layers; the
  wiki members modal has no Escape close. A tiny shared overlay stack
  (push on open, Escape pops top) fixes both.

## Known test flake (not a regression)

`TestToken_ListIncludesRevoked` (`internal/shards`) fails under
parallel `go test ./...` load but passes in isolation — the classic
"parallel packages share one test DB" truncate race the codebase notes
elsewhere. Move it to a dedicated schema like the other DB-gated
suites.

---

## Architecture assessment

Every reviewer independently praised the structure; the fixes above are
concentrated in a few code paths, mostly ones added in the recent
feature push (memory Phases A–C, user skills, view-as).

**Authorization — strong and unusually consistent.** One idiom
(`adminUserScope` → per-surface helpers, `loadScoped*` for ownership,
uniform 404-not-403, `IsAdmin()` centralizing "shards never inherit
admin"), re-enforced at the SQL layer as defense-in-depth. The single
security break (media handlers) was the one spot someone hand-rolled
`Role == "admin"` instead of the helper — fixed, and worth a lint/grep
guard banning raw `Role`-string authz comparisons.

**Pipeline — legible, documented resilience hierarchy the code honors.**
Every optional dependency is nil-safe with a bounded timeout; trusted
and shard paths share one parameterized implementation; cancellation
now works end-to-end (post-P0 shutdown fix). Strong on
degrade-and-continue; the remaining weak axis is long-tool-loop
resource bounds (P2 above).

**Skills — clean two-tier separation.** Go behavior vs inert markdown
content served through one skill behind a 4-method interface; the
trusted-vs-shard boundary is enforced at three independent layers with
the backend deliberately not trusting the pipeline wiring. The one
implicit invariant worth an assertion: "ShardID and userSkillsUnlocked
are never simultaneously active" (guaranteed only by `runTurn`'s
`overrides == nil` condition).

**Memory data model — the supersede leg is coherent; versions and
relationships legs are semi-detached.** "Superseded = pointed-at" is
applied uniformly (retrieval, admin, sleep, health, repair guard) and
the repair migration + strict-inequality dedup make cycles structurally
impossible. The **versions** leg is decorative (best-effort, outside
transactions, CASCADEs away on collapse) — it can't serve as the audit
trail CollapseChain assumes. The **relationships** leg is semi-detached
(approximate provenance, write-only `scope_tag`). Know this before
leaning on either; the P2 items above harden them if you do.

**Migrations — unusually disciplined** (advisory-lock serialized,
fresh-DB double-run test, careful introspection). The one philosophical
flaw is letting *data* statements live forever alongside DDL; the new
`applied_data_fixes` marker (added for the slug fix) is the mechanism to
retire them.

**Frontend — the no-framework approach is holding up at ~22k lines.**
One skipped escape in 253 innerHTML sinks, vendored deps + strict CSP,
a CustomEvent bus for decoupling. The strain points are framework-shaped
(lifecycle teardown, string-built HTML safety, overlay management,
manual cache-busting) and argue for ~3 small internal conventions, not a
migration.

**Recurring theme:** recent feature velocity outran the codebase's own
conventions in specific places — transaction discipline, the authz
helper, lifecycle. The conventions themselves are sound; these were
places that predated or slipped past them, and the P0/P1 fixes pulled
them back in line.
