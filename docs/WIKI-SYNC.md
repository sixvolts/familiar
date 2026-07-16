# Wiki page sync — robustness design

Multiple people (and the AI) edit the same wiki page from multiple
devices — the canonical case is a shared grocery list edited by two
phones at once. The recurring complaint was **silent clobbering**: one
person's save wiping the other's edit. This doc records how sync is made
robust, in two phases.

## Model

- Every page carries `updated_at`. A client sends the `updated_at` it
  last saw as an `If-Match` precondition on `PATCH`.
- `wiki_revisions` stores a full-snapshot history. Critically, a
  revision's `created_at` and the page's `updated_at` are stamped from
  the **same `transaction_timestamp()`** in one transaction, so a past
  `updated_at` (an `If-Match` value) **uniquely identifies the base
  revision** a client edited from. This identity is what makes a
  server-side three-way merge possible.

## Phase 1 — close every silent-clobber path

Optimistic concurrency, made airtight:

- **Atomic CAS** in `UpdatePage`: the `UPDATE` itself is guarded on the
  base `updated_at` (`date_trunc('microseconds', updated_at) = $base`),
  not just a pre-transaction check. Zero rows affected ⇒ someone beat us
  ⇒ `ErrPageStale`. Closes the read-modify-write (TOCTOU) window.
- **Required `If-Match`** for content edits at the HTTP layer (`428`
  Precondition Required if omitted) — no accidental last-write-wins.
- **Atomic append**: `AppendPage` concatenates in a single in-DB
  `UPDATE`, so two concurrent appends can't lose one.
- **Agent tools** thread `If-Match` and retry on stale.
- **Frontend** (desktop `wiki.js` + mobile `mobile.js`): dirty-aware
  refresh/poll/resume, poll-race guards (never move the base backwards),
  unload flush with `keepalive`, and baseline normalization to Toast
  UI's round-tripped markdown (kills phantom-dirty saves).

After Phase 1 a conflict is never silent — but a stale writer is
*rejected*, which is safe but annoying for a shared list.

## Phase 2 — auto-merge disjoint edits

Make concurrent edits *pleasant* via a server-side three-way line merge.

- **`internal/textmerge`** — a line-based diff3 (`Merge(base, mine,
  theirs) (string, bool)`). LCS-anchored stable regions; disjoint
  changes combine, same-region divergence reports a conflict. Policy
  tweak for shared lists: when both sides purely *insert* at the same
  anchor (empty base region), keep **both** instead of conflicting.
- **`UpdatePage` merge-and-retry loop**: on a CAS-stale **content-only**
  save, recover the base revision via `revisionContentAt(If-Match)`,
  `Merge(base, incoming, current)`. Clean merge ⇒ commit the merged body
  (CAS on the *current* head) and flag the response `Merged: true`.
  Conflict or unrecoverable base ⇒ `ErrPageStale`. A CAS lost to a
  writer landing mid-merge re-reads and re-merges (bounded retries).
  Title/slug edits are never auto-merged (not line-mergeable).
- **Clients reflect merges**: on a `200` whose body has `merged: true`,
  desktop and mobile swap the editor to the merged document and reseed
  the baseline — but only if the user hasn't typed since the save was
  sent (otherwise their newer edit merges again on the next flush). A
  distinct "Merged — <who>" status replaces the transient "Saved".

Net effect: two people each adding a different grocery item both land,
no reload, no lost item. Only a genuine same-line conflict falls back to
the manual keep-mine / take-theirs choice.

## Tests

- `internal/textmerge/diff3_test.go` — merge unit cases (grocery list,
  top/bottom adds, edit+add, remove+add, same-line conflict, symmetry).
- `internal/admin/wiki_concurrency_test.go` (DB-gated) —
  `AutoMergesDisjointStaleWrite`, `ConflictOnSameLineStaleWrite`,
  `TitleEditStaleRejected`, `ConcurrentAppendsBothSurvive`.
- `internal/skills/wiki/wiki_test.go` — agent-tool stale retry threads
  `If-Match`.
