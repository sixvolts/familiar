# RESEARCH-SKILL-SPEC — tiered web research with curated memory

**Status:** Phase 1 + Phase 2 built (2026-07-09).
**Builds on:** USER-SKILLS-SPEC (markdown skills, trusted-path exposure),
WIKI-SYNC (atomic appends + auto-merge), the shards envelope
(`pipeline.ShardOverrides`).
**Section numbers match the published spec artifact** — code comments
cite them (§6.2, §6.3, …); renumber both or neither.

---

## 1. Motivation

Familiar can answer from memory and do one-shot searches, but has no way
to *research*: run a directed multi-query investigation and leave the
household smarter afterward. This spec adds a `research` skill with
three effort tiers — **quick** (one turn, snippet-first), **standard**
(default; 4–6 searches across angles), **deep** (multi-round with
sub-questions).

## 2. What exists (and is reused wholesale)

| Piece | Where | Reused as-is? |
|---|---|---|
| SKILL.md machinery, user skills, `chat_enabled` trusted path | `internal/skillpkg`, `skills/skillpacks` | yes |
| `web_search` (Brave), `fetch_page` | `internal/skills/search`, `…/fetch` | yes |
| Wiki tools; personal book `personal:{userID}`; atomic `append_to_page` + diff3 merge | `internal/skills/wiki`, `admin/wiki.go` | yes — concurrent worker appends are safe by construction |
| `save_fact` → `CommitFacts` (≥0.92 near-dup collapse, idempotent) | `internal/skills/memory`, `internal/memengine` | yes |
| `wikiknowledge` page-save extraction | `internal/wikiknowledge` | yes — free lossy second recall path |
| `HandleShard` + envelope (allowlist, BookAccess, ephemeral, tier pin) | `internal/pipeline/shards.go` | extended: one field (§6.1) |
| Invoke pattern `InvokeFunc(ctx, sess, prompt, overrides)` | `internal/actions/runner.go` | pattern copied for workers |

## 3. Constraints that shaped the design

Defaults, verified against source: tool loop 10 LLM rounds/turn (grows
to search-budget+3, pipeline.go); `web_search` per trusted turn = effort
shallow 1 / deep 5 (tier fallback 1/2/5/10); **shard turns had
`web_search` hard-disabled** (`shardClassifierOutput` stamps
`SearchNone`); per-tool-result cap 2000 tokens; whole-turn tool zone =
12% × (window − 4096); 300 s turn wall clock; `fetch_page` ≤12K chars,
no PDF/JS; Brave ≤10 results, no freshness param, no client rate-limit.

Two consequences drive everything: **budgets reset per turn** (tool
messages persist and replay), and **a wiki page is an unbounded atomic
accumulator**. So evidence is externalized to a page instead of hoarded
in context, and depth scales across turns (Phase 1) or across parallel
worker turns that each bring their own budgets (Phase 2). Synthesis is
always single-threaded.

## 4. Output contract

Identical across tiers and phases — Phase 2 changes the engine, not the
artifacts. Every run produces:

1. **The note (deliverable).** A new page in the user's personal book
   (slug `personal:{userID}`), titled `Research: <topic>`, structured
   Summary / Key findings / Details / Sources (no trailing "Open
   questions" section — a deliverable, not a to-do list of gaps), with
   inline `[Title](URL)` citations — the first citation convention in
   Familiar. It is written as a **finished, reader-facing briefing**
   (coherent narrative, each fact stated once, Details deepens Key
   findings) — not a reading log. Standard tier targets ~700–1,100 words
   off ≥5 searches / ≥3 fetches (a hard floor so it doesn't collapse to a
   snippet-only overview); see SKILL.md for the per-tier bars.
2. **The chat summary.** ≤200 words in-thread; the most useful/surprising
   takeaways, not a rehash of the note's Summary; never the whole note.
   It does **not** name the note location — the interface adds the link
   (below).
3. **The memory pass.** Curated `save_fact` calls (quick 1–3, standard
   6–12, deep 10–20): `scope="user"`; one atomic self-contained sentence
   per fact; entities and exact tokens in the text (the FTS retrieval
   arm matches lexically); source URL *inside the content* when
   provenance matters (`source_ref` never reaches the model at recall);
   "as of <month year>" on time-sensitive claims; tags
   `["research", "<topic-slug>"]`.
4. **Deep runs only — the evidence page.** In the shared `research`
   book: the sub-question checklist plus compact
   `- finding — [Source](url)` bullets; workers' write target and the
   synthesis input, retained for transparency.

Bonus recall path: saving any of these pages triggers `wikiknowledge`
extraction (~10–20 sidecar-chosen atomic facts + graph triples,
clean-replaced per save). The explicit memory pass is the precision
channel; extraction is the backstop.

## 5. Phase 1 — the SKILL.md

Canonical copy: `familiar-gateway/internal/skillpkg/builtin/research/`
(SKILL.md ≤ ~1,500 tokens so the `use_skill` return survives the
2,000-token result cap; deep-tier detail in
`references/deep-protocol.md`, loaded only when needed).
Ships as a **builtin skill**: embedded in the gateway binary
(`go:embed`), synced to the library at boot, enabled + chat-enabled by
default — no import step. Admins can disable it or remove it from chat
in the Skills panel; delete is refused (it re-syncs at boot).

The loop: scope (+ sub-question checklist for deep) → batched searches
broad→narrow, snippets before fetches → fetch only load-bearing URLs,
never re-fetch failures → persist learnings to the evidence page each
round (deep) → stop rules (2 independent sources per question or 1
authoritative; 2 dry searches → move on) → synthesize the note once →
chat summary → memory pass.

Deep tier without Phase 2 runs as explicit continuation: each turn
covers the next unchecked sub-questions, appends evidence, ends with
"N/M covered — say continue". Fresh budgets every turn.

## 6. Phase 2 — virtual-shard workers

Deep research fans out to parallel single-purpose pipeline turns:
`ShardOverrides` envelopes with no DB row, invoked exactly like the
actions runner invokes `HandleShard`. Gpu-host serves 6×64K slots;
workers occupy idle ones.

### 6.1 `SearchBudget`

`ShardOverrides.SearchBudget int`: a positive value lifts the
shard-path `SearchNone` stamp in `runToolLoop` and grants the envelope
N `web_search` calls. Zero keeps today's behavior — existing shards and
ephemeral action envelopes never search. Server-side only; page content
and skill text can never set it.

### 6.2 `spawn_research_workers` (skill `internal/skills/research`)

One tool: `{topic, tasks: [{question, hints?}] (1–8), page_slug?}`.
Ensures the caller's **per-user** research book, creates or reuses the
evidence page (plan checklist + findings sections), then dispatches one
worker goroutine per task and **returns immediately** — the 30 s
tool-call cap never blocks on 2-minute workers. Re-spawns pass
`page_slug` from the first spawn's result so workers append to the same
page.

The research book is **per-user and hidden** (`WikiStore.
EnsureResearchBook`, slug `research:{userID}` minted like the personal
book via `insertBookTx`): the deliverable note lives in the user's
personal notes, and this book holds only the deep-tier evidence/scratch
pages the workers write to — kept separate so a prompt-injected worker
reading hostile web content is confined by `BookAccess` and can't reach
the personal notes. It is excluded from every book listing (HTTP and
the model's `list_books`, even include-personal) via a
`NOT LIKE 'research:%'` filter, and is system-managed (no rename/
archive). Because the slug carries a colon and `CreateBook` slugifies
`:` → `-`, a user-requested slug can never collide with or be hidden as
a research book, so no reservation is needed.

An earlier cut used a single **shared** `research` book (global slug):
the first user to run a deep search owned it and every other user was
refused — the multi-user break. Per-user books remove it along with all
the membership/fork-race handling that shored it up.

### 6.3 Worker envelope

`SystemPrompt` = worker role prompt (one sub-question; compact
`- finding — [Source](url)` bullets appended to the evidence page;
final reply is a one-line status); `ToolAllowlist` =
`web_search, fetch_page, read_page, append_to_page`; `BookAccess` =
research book only; `Skip*`/`ExcludeFromHot` all true; `TierHint`
`technical` (config; `tier2` pins the sidecar model for cheap workers —
validated at construction, unknown aliases fall back loudly);
`ModelOverride` = `worker_model` when configured (explicit registry
pin — validated at startup for existence + tools capability; the tier
still shapes the thinking budget); `SearchBudget` 4 (config);
`MaxTokens` 2048. Session `research:<runID>:<n>`, identity
`("research", ownerID)`.

The worker and writer role prompts ship inside the builtin package
(`references/worker-prompt.md`, `references/writer-prompt.md`) and are
read from the binary embed at construction (`skillpkg.BuiltinFile`) —
prompt content stays in the markdown layer; compiled fallbacks only
guard against packaging regressions.

### 6.4 Run lifecycle

The `MaxWorkers` semaphore lives on the skill, **gateway-wide** —
overlapping runs (re-spawns, concurrent users) share one pool so total
in-flight workers never exceed the cap (default 3: ≤ inference
slots − 2). Detached contexts parented on the skill's root context,
300 s per worker, 10 min per run; `Close()` cancels the root so gateway
shutdown cuts workers instead of leaving them racing teardown.
Supervisor appends a completion line (`run <id> complete: n/total …`)
and per-worker failure lines; partial results are normal — re-spawn
only missing tasks.

### 6.5 `compose_research_note` — the writer model

The conversation-side coordinator is the trusted chat thread and the
trusted path has no per-turn model switch (a deliberate pipeline
invariant). What *can* run on a different model is the note
composition: when `writer_model` is set, the skill offers
`compose_research_note {topic, evidence_page_slug | evidence,
note_title?}` — tool presence is the capability signal the SKILL.md
branches on, same as spawn.

Mechanics: the evidence page is read **server-side** (the writer sees
the full log, not a 2,000-token tool-result truncation; inline
`evidence` covers quick/standard runs), a stub note is created in the
personal book immediately, and a single **no-tools completion** —
empty allowlist, `ModelOverride = writer_model`, `MaxTokens` 4096 —
fills it in. Async like spawn (a long-form completion blows the 30 s
tool cap), sharing the gateway-wide semaphore (released before page
writes; acquisition bounded by the run deadline so a starved compose
fails visibly on the stub) and root context.

Delivery never silently clobbers: the note lands via content-only
`UpdatePage` carrying the stub's dispatch-time `IfMatch` — an
untouched stub is replaced cleanly, concurrent user edits auto-merge
(WIKI-SYNC Phase 2), and a merge conflict or renamed page falls back
to the ID-addressed atomic append, so neither the user's text nor the
composed note is ever dropped. Failures replace the stub placeholder
under the same protocol. The writer model needs no tools capability —
any strong prose model in the registry qualifies. The chat summary and
memory pass stay on the trusted turn.

### 6.6 Evidence cleanup

Evidence pages are transient worker scratch, and their per-user book is
hidden from every listing — so nothing surfaces them for manual
cleanup, and without reaping, one page per deep run would accumulate
forever. A **retention sweep** bounds them: `WikiStore.
SweepResearchEvidence` runs on a 6 h ticker (tied to the signal context
so it exits on shutdown) and soft-deletes `research:%`-book pages older
than `evidence_retention_hours` (default 72). It's the single cleanup
mechanism, covering every path uniformly — model-written notes,
compose, abandoned/failed runs.

An earlier cut also deleted the evidence page eagerly at the end of a
compose; review found that raced two ways (deleting the note's only
source when the write hadn't actually landed, and deleting evidence a
concurrent re-spawn appended after the writer's snapshot), so eager
deletion was dropped in favor of the sweep alone. Race-free
end-of-run cleanup belongs to the future run-lifecycle owner (§6.7,
autonomy), which knows when a run is terminal.

Evidence pages are **excluded from `wikiknowledge` extraction**
(`OnPageSaved` skips `research:%` books): raw web scratch shouldn't seed
memory (it's low-value and possibly prompt-injected, §9), and skipping
it also means the sweep has no facts to clean up. The note cites the
original web sources, never the evidence page.

## 6.7 Autonomous runs — no "say continue"

The deep tier is a chat turn fighting a turn-based model: the workers
run in the background, but *synthesis* needs a fresh turn, so the user
is forced to type "continue" — and again each time the model decides to
spawn another batch. Autonomy removes it: a deep search becomes a
**tracked background run** that drives itself to a finished note, with a
persistent progress indicator and a completion notification.

### State — `research_runs`

One row per run: `id, user_id, conversation_id, topic, status
(researching|synthesizing|done|failed), round, workers_total,
workers_done, tokens, pages_read, workers (jsonb), evidence_book_id,
evidence_page_slug, note_book_slug, note_page_slug, error, created_at,
updated_at`. This is the source of truth the progress card restores from
(survives tab-away) and the only thing that persists in-progress state
(chat's streaming spinner is transient DOM, destroyed at turn end).

`workers` is the per-area roster the in-chat card renders — a JSONB array
of `{question, state}` (state = `queued|active|done|failed`), one entry
per sub-question in dispatch order. It's seeded queued at `Create`, and
`SetWorkerState(id, idx, state)` transitions each entry via `jsonb_set`
keyed by the task's **stable index** (0-based dispatch position) — so
gap-fill's round-local worker renumbering never scrambles it, and the
non-failed areas stay `done` across rounds. Concurrent per-index writes
serialize under the row lock; `create_if_missing=false` makes an
out-of-range index a clean no-op.

### The in-chat card — Roster + pixel indicator

The card is a purple-outlined bubble pinned in the chat (replacing the
old bottom-bar spinner and the per-tab spinners). It renders one row per
roster area — the sub-question plus a **pixel indicator** (three columns
1·2·3 tall): a purple pulse marching left→right while `active`, solid
green when `done`, faint when `queued`, amber when `failed` — over a live
meta footer (round · areas done/total · pages · tokens). The whole thing
is rebuilt from the poll payload each ~5s tick, so a reopened tab
restores it with no client memory. Desktop and mobile share the markup
and pixel CSS; the done transition still collapses to the note link
(RESEARCH-SKILL-SPEC "note auto-open"). On a run with no roster (older
rows) it degrades to the header + aggregate counters.

### Control flow — the loop is skill-driven, not model-driven

The re-entrancy hazard (a synthesis turn spawning more workers, needing
another supervisor, …) is avoided by making the **loop deterministic in
the skill** and invoking the model only to write the final note:

1. **Kickoff** (the user's chat turn): the model calls
   `spawn_research_workers(tasks)`. The skill creates the run
   (round 1, status=researching, `conversation_id` from the turn),
   dispatches workers, returns "researching in the background — I'll
   post the results here." The turn ends normally.
2. **Batch completes** (the existing supervisor goroutine): records
   `workers_done` and which task indices *failed* (worker error / empty).
   - **Gap-fill (deterministic):** if any tasks failed **and**
     `round < max_rounds` (default 2), bump the round and re-dispatch
     workers for *only the failed tasks* — a new supervisor repeats this
     step. No model call, no marker parsing; the gap signal is the
     worker error itself.
   - **Synthesize:** otherwise `status=synthesizing`, and the skill runs
     one **owner-path turn** (`pl.Handle` in a dedicated
     `research:run:{id}` session) prompted to read the evidence page,
     write the final note to the personal book, run the memory pass, and
     reply with a ≤200-word summary. `spawn_research_workers` is refused
     inside a run session, so this turn can only synthesize.
3. **Deliver:** append the summary as an assistant message to the
   *originating conversation* (`convStore.AppendMessage`, `model="research"`),
   set `status=done` + the note location, and send a **mobile Web Push**
   deep-linking to `/#chat/{convID}`. Synthesis failure → `status=failed`
   + an error message + push.

Because the loop never asks the model to spawn — the skill re-dispatches
failed tasks itself — there is no nested-run tangle and no user nudge.
The gateway-wide worker semaphore still bounds concurrency; `max_rounds`
bounds depth.

**Robustness (all from the review):**

- The synthesis session id **must** carry the `research:run:` prefix
  (`GetOrCreateWithID`, not `GetOrCreate`, which mints a random UUID) —
  otherwise the spawn-refusal guard never fires and a synthesis turn
  could spawn a nested run.
- Terminal status writes + delivery run on a **detached context**, not
  the (cancellable) synthesis-turn context, so a shutdown or transient
  error mid-synthesis still marks the run terminal instead of wedging it.
- A boot-time **`FailOrphanedRuns`** marks any run still non-terminal at
  startup failed — its driving goroutine died with the old process, so
  it can't resume; reconciling unblocks the conversation.
- One active run per conversation is enforced by a **partial-unique
  index** (`conversation_id WHERE status IN (researching,
  synthesizing)`); `Create` returns `ErrActiveRunExists` on conflict —
  the atomic backstop for the kickoff guard's check-then-create window.

### Knowledge extraction is off the critical path (RESEARCHEXTRACTDECOUPLE)

The write-up turn's job ends when the **note is written and the memory
pass is saved** — that is the deliverable, and the run is marked `done`
and the user notified at that point, unconditionally. `wikiknowledge`
extraction (the note → knowledge-graph enrichment) is a *best-effort
post-pass*, never a gate on delivery. Three properties enforce this:

1. **Success = the note landed, not the turn's exit code.** A deep
   synthesis routinely writes the note + a 20-fact memory pass, then hits
   the pipeline's 300 s turn cap before emitting a final summary. The
   synthesizer re-reads the note stub after the turn: if the placeholder
   is gone, the run is `done` regardless of `turnErr` (a delivered note
   that reports `failed` with an error push was the exact bug here).
2. **Extraction is detached.** The page-saved hook fires
   `wikiknowledge.OnPageSaved` in a goroutine on `context.Background()`,
   so it never blocks the write or runs inside the turn's 300 s budget.
   The page-saved **SSE publish stays synchronous** so the live evidence
   view still updates instantly.
3. **Large documents route to the big model.** A 5–12K-char write-up
   overruns the small extract sidecar (~8K ctx, the timeout that flipped
   runs to `failed`). Bodies over ~4K chars route to `extract_large`
   (`[sidecar].extract_large_model`, e.g. `gpu-host/qwen3.5-122b`) with a
   generous 5-minute request ceiling; unset → it falls back to the normal
   `extract_model`. The route is **additive** — setting only
   `extract_large_model` never disables the other tasks' fallback.
   Extraction failure is never fatal: it logs and the run stays `done`.

### Wiring

The skill is registered early (to advertise its tools) but its autonomy
deps are attached late via `SetOrchestrator(SynthesizeFunc)` — a closure
built in `main.go` after `convStore` + `pushSender` + the run store
exist (they're constructed after the skill today). The closure holds
`pl.Handle`, `convStore`, `pushSender`, and the run store; the skill's
supervisor calls it. No registration move, no push-ordering hazard.

### Progress surface — poll + live evidence page

There is no live chat-message channel, so the card is **poll-driven**
(precedented by the wiki/shard-run pollers): `GET
/console/api/research/runs/active?conversation_id=` returns the active
run's `{status, round, workers_done/total, tokens, pages_read,
evidence_book_slug, evidence_page_slug, note_book_slug, note_page_slug}`.
While a run is active the workspace polls every ~5 s and renders a
persistent card; on reopen it re-queries and restores it; on the
null-after-card transition (run went terminal) it refetches messages
(the summary appears) and links the note.

**Live counters.** `workers_done` is bumped per worker as each finishes
(`IncrementWorkerDone`), and each bump also adds that worker's token and
`fetch_page` tally (`RouteInfo.PagesFetched`, threaded out of the tool
loop), so the card ticks up live — e.g. *"Researching X · 4/7 areas ·
31 pages · 88k tokens"* — instead of sitting at 0/7.

**Live evidence page (desktop).** The evidence book is hidden from
listings but the owner can fetch its pages directly by slug, and every
worker `append_to_page` fires the page-saved SSE the workspace already
consumes. So on run start the desktop forces the side-by-side layout and
opens the evidence page **read-only** in the right pane, live-refreshing
on each page-saved event — you watch findings land in real time. On
done, the pane swaps to the delivered **note** (editable) and stays
open. Mobile (single-pane) gets the richer card stats but not the
right-pane view.

Desktop has no service worker, so a closed desktop tab gets no push — it
sees the done card + note on return; the mobile PWA gets the push.

### Note auto-open + in-chat link — all three write paths

The note is written by one of three paths; each gets a durable
`#note/<book>/<page>` link the workspace opens in-pane, and the inline
paths also auto-open it. The link is **built from real slugs, never
model-authored**, so it can't drift; the chat summary no longer names the
note — the link is the single reliable affordance. Detection keys off the
personal-book + `Research:` title convention (`researchNoteFrom`).

- **Inline `create_page`/`update_page`** (no writer model): the wiki
  tools stash the page location in `ToolResult.Data`; the pipeline tool
  loop records it on `RouteInfo.ResearchNote` (a value field threaded as
  `*ResearchNoteRef`, mirroring `PagesFetched`) and native.go surfaces it
  in the streaming `done` event as `research_note`.
- **Inline `compose_research_note`** (writer model set): the compose tool
  sets the same `Data` and `researchNoteFrom` accepts its tool name, so
  it rides the same `done`-event path.
- **Deep synthesis** (server-side, no SSE client): `makeResearchSynthesizer`
  appends the link to the delivered summary in Go before `deliverResearch`
  (the poll loop still swaps the right pane to the note on done).

On the client the inline paths auto-open the note editable in the right
pane (same `openDoc`/`setLayout` machinery as the deep-path swap;
idempotent via `focusExistingDoc`, skipped when a deep run owns the pane)
and append the link before the message is persisted (survives reload).
Delegated click handlers open `#note/` links in-pane (desktop) or
navigate to the note (mobile) instead of letting the raw hash navigate.

## 7. Config

```toml
[skills.research]
enabled                  = false   # requires [tools.brave] enabled AND admin.enabled (wiki store)
max_workers              = 3       # gateway-wide; ≤ inference slots − 2
worker_search_budget     = 4
worker_tier              = "technical"   # tier3 = chat model; tier2 = sidecar
worker_model             = ""            # explicit [[models]] pin for workers (needs tools)
writer_model             = ""            # dedicated note-writing model (§6.5); "" = in-turn
evidence_retention_hours = 72            # sweep hidden evidence pages after this (§6.6); 0 disables

[sidecar]
extract_large_model = "gpu-host/qwen3.5-122b"  # big-model route for the note → knowledge post-pass (§6.7); "" = use extract_model
```

Model pins are validated at startup against `[[models]]` (existence,
and tools capability for `worker_model`); invalid values fall back
loudly to default behavior. `extract_large_model` is likewise validated
(warn-only): a typo falls back to `extract_model`.

Startup logs loudly when enabled without its prerequisites (admin off →
early warning; web_search unregistered → skip with reason).

Recommended companions on the deployment host: `[context] window_size`
matched to the served slot size (65536 for 6×64K slots),
`tool_result_ratio 0.16–0.20`, `max_tool_result_tokens 2500`,
`[tools.brave] max_results 8`, `[effort.search_depth.deep]
max_searches 8`. Keep 6×64K slots: workers need <20K each and the
evidence page externalizes accumulation — 3×128K would halve
parallelism to buy context nobody holds.

## 8. Testing

- `internal/pipeline/pipeline_shard_test.go` —
  `TestShard_SearchBudgetZeroKeepsWebSearchDisabled` (default preserved),
  `TestShard_SearchBudgetGrantsWebSearch` (N allowed, N+1 exhausted).
- `internal/skills/research` — mock Invoke/Backend: envelope field
  matrix, per-spawn semaphore honored AND gateway-wide across
  overlapping spawns, immediate return, failure appends, Close() cuts
  in-flight workers, reader-role refusal, identity/argument guards,
  book-exists-not-member error, invalid-tier fallback.
- E2E (model-gated, follow-up): boot-synced builtin (chat-enabled by
  default) → "quick research…" → assert `use_skill`, note in personal
  book, `save_fact`.
- Eval set (follow-up): ~10 queries + LLM-judge rubric (accuracy,
  citations, completeness, source quality, tool efficiency).

## 9. Security notes

- Workers read hostile web content with a four-tool toolbox: no memory
  writes, no profile, no other books. A prompt-injected worker can at
  worst graffiti the evidence page; it cannot read personal notes, so
  the fetch-as-exfiltration channel is closed by `BookAccess`.
- Facts enter the memory engine only through the trusted turn's curated
  pass, after synthesis (stage public research before private data).
- Evidence pages are **excluded from `wikiknowledge` extraction**
  (`OnPageSaved` skips `research:%` books), so worker-fetched web content
  — low-value and possibly prompt-injected — never seeds memory. Only
  the curated `save_fact` pass on the trusted turn writes memory. (This
  closes what an earlier draft listed as an accepted risk.)
- `fetch_page`'s dial-time SSRF guard applies to workers unchanged.
- `SearchBudget` is envelope-level and never settable by page content
  or skill text.

## 10. Open items

- Brave plan tier on the prod host: `journalctl -u familiar-gateway |
  grep '\[brave\]'` — persistent `extra_snippets=0` = free tier (~3–5×
  less text per search).
- Push completion ping; `fetch_page` offset param; Brave freshness
  passthrough — separate follow-ups.
