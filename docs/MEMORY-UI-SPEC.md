# MEMORY-UI-SPEC — knowledge-first memory interface

**Status:** Phases A–C implemented (A: dead-wire fixes, kind-partitioned
browser, graph expand, honest mobile screen; B: entity index with
fact-count/last-seen, Entities browser mode + entity detail, entity-facts
endpoint, mobile entity drill-down, browser→graph focus handoff; C: entity
merge, edge edit, supersede-chain viewer + collapse, store-health strip).
Deviations — §4: sorting is client-side over the top-100 index rather than
a server sort param, and the entity detail is a right-hand aside (matching
the memory detail idiom) rather than a side-by-side mini-graph — the graph
link is a one-click handoff instead. §5: edits stamp `updated_at` only (no
separate provenance ledger for triples; memory edits keep full version
history as before), and the health card surfaces counts without one-click
cleanup actions.
**Companion change:** the sleep/consolidation cycle was cut down to a
maintenance job in the same review — see §6.

---

## 1. Diagnosis

The memory console grew organically alongside the engine and ended up
presenting the *storage model* instead of the *knowledge*. Concretely:

1. **One flat list of everything.** The browser listed every row in
   `memories` — extracted facts, explicit facts, wiki-derived rows, and raw
   conversation chunks — in a single table. Chunks outnumber knowledge facts
   by an order of magnitude, so the default view was mostly transcript
   fragments, and the thing the user actually cares about ("what does
   Familiar believe about me?") was buried.
2. **Dead wires.** The detail drawer's RELATIONSHIPS section called
   `/console/api/relationships?fact=…`, a route that was never registered —
   it rendered "NOT AVAILABLE" for every memory, always. The graph blurb
   advertised "double-click a node to expand" but no `dbltap` handler
   existed. The blurb also called the graph "read-only" two sentences after
   promising interaction.
3. **Stale embeddings on edit.** `PATCH /console/api/memories/{id}` updated
   `content` but left the old embedding in place, so an edited fact kept
   being retrieved by its *previous* meaning.
4. **Mobile theater.** The mobile Memory screen grouped facts under a single
   hard-coded "Entities" bucket, rendered rows that didn't respond to taps,
   and showed a static decorative SVG where the graph would be. It looked
   like a feature and functioned like a screenshot.
5. **Graph underutilized.** The entity graph is the most differentiated
   surface in the console — provenance-linked triples over pgvector facts —
   but it was a dead-end viewport: no expansion, no path from a fact to its
   triples or back.

## 2. Direction: knowledge first, chunks on request

The unit the user thinks in is the **fact** ("Drew prefers pages over
modals") and the **entity** ("Drew", "familiar-gateway"). Raw conversation
chunks are evidence, not knowledge — they should be reachable (provenance,
retention debugging) but never the default view.

Phasing:

- **Phase A (this change):** make the existing surfaces honest and partition
  by kind. No new information architecture.
- **Phase B (proposed):** entity-first navigation. An entity index (name,
  degree, fact count) as a first-class browser mode; tapping an entity shows
  its facts and its depth-1 neighborhood side by side. The graph and the
  list stop being separate worlds.
- **Phase C (proposed):** curation workbench. Merge duplicate entities,
  edit/retire triples, resolve supersede chains visually, and surface
  store-health stats (orphaned edges, superseded-chain depth, chunk volume
  vs retention window).

## 3. Phase A — delivered

### 3.1 Backend

- `MemoryFilter.Kind` on the browse query: `knowledge` →
  `COALESCE(source_type,'') <> 'conversation'`, `chunks` → the complement.
  Orthogonal to the existing `source_type` filter, which the UI still uses
  for the per-source tabs.
- Browse/detail DTO enriched: `source_ref`, `scope_tag`, `last_accessed`,
  `supersedes`, `superseded_by` (derived: the newest row pointing at me).
- `GET /console/api/memories/{id}/relationships` — the real triples
  endpoint the detail drawer always wanted: `relationships` rows whose
  `source_fact` is the memory.
- `PATCH /console/api/memories/{id}` now re-embeds edited content through
  the session embedder (10s budget; on failure the vector is cleared rather
  than left stale, so FTS still finds the row but dense search can't return
  it under its old meaning).

### 3.2 Console browser

- Kind tabs replace the Source dropdown: **Knowledge** (default) ·
  Explicit · Extracted · Wiki · Conversation log · All. Knowledge-first
  means the landing view contains zero transcript fragments.
- Detail drawer: LAST USED / SOURCE REF / SCOPE TAG meta rows; REPLACED and
  REPLACED BY are clickable links that walk the supersede chain; the
  RELATIONSHIPS section reads the real endpoint.

### 3.3 Graph

- `dbltap` on a node fetches `/console/api/memory/graph?center=<entity>&depth=1`
  and merges the neighborhood into the running view (no duplicate
  nodes/edges, layout re-run). The blurb copy now matches reality.

### 3.4 Mobile

- The fake category grouping, dead rows, and decorative graph SVG are gone.
  The screen shows real store stats and a top-entities-by-degree list —
  less pixels, all of them true. Entity → detail navigation is Phase B.

## 4. Phase B — delivered

- `GET /console/api/memory/entities` now returns `fact_count` (distinct
  source facts) and `last_seen` alongside degree; empty `q` means the
  global top-by-degree index. `GET /console/api/memory/entity/{name}/facts`
  returns the live memory rows the entity's triples were extracted from
  (superseded rows skipped).
- The browser's kind tabs gain an **Entities** mode: filterable index
  (entity / connections / facts / last seen, headers re-sort client-side),
  click → entity detail aside with meta, the fact list (each opens the
  memory detail), and a **Focus in graph** handoff that switches to the
  graph panel centered on the entity.
- Mobile entity rows are tappable: `memory/<name>` shows the entity's
  facts.

## 5. Phase C — delivered

- **Entity merge** — `POST /console/api/memory/entity/{name}/merge`
  `{into}` rewrites every user-owned triple mentioning the entity. The
  `(subject, predicate, user)` unique index makes blind rewrites collide,
  so rows whose target slot is taken are dropped in favor of the
  established row, and edges that connected the two entities (now
  self-loops) are deleted. UI: a merge control in the entity detail aside.
- **Edge edit** — `PATCH /console/api/memory/relationship/{id}` renames a
  predicate and/or adjusts confidence (409 on predicate collision).
  Inline editor in the graph panel's edge detail. Retire = the existing
  delete.
- **Chain viewer + collapse** — `GET /console/api/memories/{id}/chain`
  walks the supersede chain both directions (depth-bounded against
  hand-corrupted cycles); the memory detail shows it whenever the row is
  part of a chain, each link navigable. `POST …/chain/collapse` deletes
  the replaced rows and keeps the live tip (pointers cleared first — the
  FK is self-referential). Version history on the tip survives.
- **Store health** — `GET /console/api/memory/health`: conversation-chunk
  count + oldest age, knowledge rows without embeddings, superseded rows
  awaiting collapse, and orphan edges (provenance pointing at deleted
  memories). Rendered as a quiet stat strip under the browser.

## 6. Sleep vs post-turn (decision record)

The write path is owned by the **post-turn sidecar pass** (extraction,
ADD/UPDATE/DUPLICATE classification, supersede chains, versions). The sleep
cycle survives only as time-based hygiene that no per-turn pass can provide:

1. **Drift dedup** — near-duplicate live knowledge facts written
   independently (before either existed) collapse newer-over-older within a
   `(user_id, scope_tag)` partition; chunks and wiki rows excluded.
2. **Chunk retention** — session-scope rows idle past the archive window
   are hard-deleted.

Two legacy phases were deleted: `promote_cross_session` (its ≥2 `conv:*`
tag trigger could never fire — ON CONFLICT re-commits don't merge tags and
extraction facts are born user-scope) and `decay_stale_session` (wrote
`decay_score`, which nothing read).

The old dedup had its supersede direction **inverted** — it hid the newer
fact of each pair. Fixed in `internal/memengine/sleep.go`, with a standing
repair migration (`repair_inverted_supersedes`) that clears any pointer
older than its target on every boot: only the buggy dedup ever wrote those,
so the predicate is precise.
