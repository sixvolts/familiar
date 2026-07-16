---
name: research
description: >-
  Web research expert. ALWAYS use this skill when the user asks to research,
  investigate, dig into, or get an overview of a topic. Effort tiers: quick,
  standard (default), deep — keyword anywhere in the request. Not for simple
  lookups answerable with a single search.
license: MIT
metadata:
  version: "1.0"
allowed-tools: web_search fetch_page list_books create_page append_to_page read_page update_page search_pages save_fact spawn_research_workers compose_research_note
---

# Research

Directed multi-query web investigation. Every run, every tier, ends with three outputs: a note, a chat summary, and a memory pass. No exceptions.

## Tier selection

Effort keyword anywhere in the request picks the tier ("quick research on X", "deep research into Y"). No keyword → standard. Decision line: if the question is answerable with a single search, don't research — just answer it.

**Deep tier: read references/deep-protocol.md first** — `read_skill_file {"skill":"research","path":"references/deep-protocol.md"}`. Only proceed after reading it. Do not load it for quick/standard.

| | quick | standard | deep |
|---|---|---|---|
| Turns | 1 | 1–2 | 1 (then it runs itself) |
| Searches | 2–3 | 5–7, across 3–4 angles (≥2 per angle) | 3–6 per sub-question |
| Page fetches | ≤1 | **3–5 — at least 3 required before writing** | as needed |
| Sub-question checklist | — | recommended, 3–5 items | required, 3–7 items |
| Evidence page | — | — | required |
| Note length | ~250–400 words | **~700–1,100 words** | **~900–1,600+ words, exhaustive** |
| Memory facts | 1–3 | 6–12 | 10–20 |

## Run checklist

Copy this into your reply and check items off as you complete them:

```
Research: <topic> — <tier>
- [ ] Scoped: question restated, tier stated, 3–4 angles named
- [ ] Tier floor met BEFORE writing (standard: ≥5 searches across ≥3 angles, ≥3 pages fetched)
- [ ] Note written to personal book — reader-facing, inline-cited, meets the length bar
- [ ] Chat summary ≤200 words
- [ ] Memory pass: facts saved
```

Don't tick the "tier floor" box on a feeling of sufficiency after a couple
of snippet reads — tick it only when the counts are actually met. An early
"I have enough now" after snippets-only is a signal to fetch, not to write.

## The loop

1. **Scope.** Restate the question in one line; state the tier. Name the 3–4 angles you'll cover up front, and give each ≥2 searches — no angle should rest on a single snippet.
2. **Search broad → narrow.** BATCH multiple `web_search` calls in one round — don't dribble them one at a time. Brave passages give you breadth and orientation. Prefer primary/official and authoritative sources; treat brand/producer marketing pages, retailer blogs, and SEO "academy/society" sites as weak — and never lean on a producer's own page for a claim it has a commercial stake in (an origin dispute, a "first/oldest" boast).
3. **Fetch, don't skim-and-stop (standard+).** Snippets are for orientation; the note's depth must come from *fetched* pages. Fetch at least 3 of the most load-bearing primary/authoritative sources before you write — do not compose a standard note from search passages alone. Expect failures — bot-blocks, PDFs, JS-only pages: fall back to the passages or an alternate source. NEVER re-fetch a failed URL. One rephrase per failed query, then move on.
4. **Stop rules.** Per angle: 2 independent sources, or 1 authoritative. Two novelty-dry searches → move on. But don't stop the whole run until the tier floor is met (standard: ≥5 searches, ≥3 angles, ≥3 fetches). If tool results start collapsing with budget notices, wrap up now and continue next turn.

Only write the note after search rounds are complete — never interleave note-writing with new searching.

## Output contract — ALWAYS, all tiers

1. **The note.** If `compose_research_note` is in your toolbox, call it — deep runs pass `evidence_page_slug`, other tiers pass your findings as `evidence` — the dedicated writer model composes the note; then do steps 2–3 yourself from your own findings. Without that tool: `create_page {"book_slug": "personal", "title": "Research: <topic>", "content": ...}`. **`book_slug` MUST be the literal `"personal"`** — it resolves to the user's personal notes. Do NOT call `list_books`, and NEVER put the note in any other book (a shared/topic wiki is wrong — the note always goes to personal notes). **The title MUST begin with `Research: ` exactly** (e.g. `Research: History of Whiskey`) — the interface keys the auto-open + in-chat note link off that exact prefix, so a reworded title silently loses the link. Structure: Summary / Key findings / Details / Sources — skeleton and citation examples in references/note-template.md. **Do NOT add an "Open questions", "Further research", "Unanswered", "Limitations", or "Next steps" section** — a trailing list of what you didn't find is noise, not a deliverable. If a genuinely load-bearing caveat matters, fold one sentence into Summary or Details; otherwise leave it out.

   **Write a deliverable, not reading notes.** The note is a finished briefing someone else will read cold — a coherent, explanatory document with a through-line, not a jotted log of "here's what I found." **This is a deliberately high-effort task, so any general guidance to "be brief", "keep it short", or "match length to complexity" does NOT apply — research IS the complex case, and thoroughness is the whole point. "Direct, no filler" means cut padding and restatement, never "be short"; err long.** Write in a clear, authoritative voice; connect facts into a narrative (cause, mechanism, consequence), don't just list them. Every claim carries an inline `[Title](URL)` citation. State each fact once, in its most specific section — **Details must add depth and nuance beyond Key findings, never restate it.** Normalize scraped titles (strip decorative brackets, "【2026】"-style year prefixes, marketing fluff) and make sure every citation is well-formed markdown.

   **Depth bar (standard tier).** ~700–1,100 words of substance (excluding Sources). Depth over breadth — develop 3–4 angles thoroughly rather than listing many one-liners:
   - **Summary** — 3–5 sentences answering the question directly *and* carrying the load-bearing specifics (key dates, names, numbers), not just the shape of the answer.
   - **Key findings** — 5–8 bullets, each a complete claim with a figure or named entity plus an inline citation. A bare topic label is not a finding.
   - **Details** — the core: 3–4 angle subsections, each 2–4 sentences (or a tight bullet cluster) that explain a mechanism, cause, or narrative. In each, at least two claims trace to a **fetched** primary/authoritative source, not a snippet.
   - **Sources** — only sources you actually used; each as `- [Title](URL) — what it supported`. Don't pad the list with search hits you never opened.
   - **No "Open questions" section.** End on Sources. Don't append a list of unresolved questions, gaps, or next steps.

   If the draft is under ~700 words, or any Details subsection is a single line, you stopped too early — run another search/fetch round before composing. Don't pad.
2. **The chat summary** — after the note exists (or the writer is dispatched). ≤200 words: the 3–5 most useful or surprising takeaways (not a compression of the note's own Summary — the user shouldn't read the same paragraph twice). Don't state where the note lives or paste a link — the interface adds a link to the note automatically. Never paste the whole note into chat.
3. **The memory pass** — last. One `save_fact` per fact:
   - `scope: "user"`, `tags: ["research", "<topic-slug>"]`
   - One atomic, self-contained sentence per fact; named entities and exact numbers/versions in the text
   - Prefer facts that carry the depth you fetched (a mechanism, a specific number, a quote) over headline dates already sitting in the summary.
   - Source URL inside the content when provenance matters: "X does Y (source: example.com/doc)"
   - "as of <month year>" on time-sensitive claims
   - Facts only — no speculation; findings the user might act on beat trivia.
