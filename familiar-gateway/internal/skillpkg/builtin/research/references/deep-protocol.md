# Deep-tier protocol

Multi-round investigation over 3–7 sub-questions, with evidence externalized to a wiki page. The SKILL.md output contract is unchanged: note + chat summary + memory pass (10–20 facts). Search loop and stop rules are the SKILL.md ones, applied per sub-question.

## Evidence page

One page per run, titled `Research: <topic>`, opening with the sub-question checklist under `## Plan`, then compact learning bullets accumulate under `## Findings` — one line per finding:

```
- <finding> — [Source](url)
```

Who creates it depends on the branch below. Never create it yourself before calling the spawn tool — the tool creates and seeds it.

## Choose the branch

`spawn_research_workers` in your toolbox → **worker fan-out** (the normal case). Not there → **turn-by-turn continuation**.

### Worker fan-out — the run finishes itself

1. Decompose the topic into 3–7 sub-questions; write them into your reply.
2. Call `spawn_research_workers {"topic": ..., "tasks": [{"question": ..., "hints": ...}]}` — one task per sub-question, NO page_slug on the first call. It creates + seeds the evidence page and returns immediately.
3. **Read the tool's result — it tells you what happens next.** Normally the run is autonomous: the result says it's researching in the background and will post the write-up automatically. In that case **your turn is DONE**:
   - Tell the user, warmly and briefly, that you've started researching and will post the findings here when it's ready.
   - **Do NOT tell them to "continue", "say continue", "check back", or "let me know when you're ready".** You are NOT synthesizing on a later turn — the system runs the gap-fill, synthesis, note, chat summary, and memory pass itself and delivers them. There is nothing for the user to do. STOP here; do not call `read_page`, do not write a note.
   - Only if the tool's result explicitly tells you to read the page next turn and synthesize yourself (an older gateway without autonomy) do you fall through to **Synthesis** below on the user's next turn.

### Turn-by-turn continuation (no spawn tool)

First turn: create the evidence page yourself with `create_page {"book_slug":"personal", ...}` (the literal `"personal"` resolves to the user's personal notes — don't list books), titled `Research: <topic>` with the `## Plan` checklist and an empty `## Findings` section.

Each turn:

1. `read_page` the evidence page; take the next 1–2 unchecked sub-questions.
2. Run 3–6 searches plus selective fetches for those sub-questions.
3. `append_to_page` the compact learning bullets with source links; tick the covered items in the checklist ON THE PAGE.
4. End the turn with: "N/M sub-questions covered — say continue." The search budget is fresh every turn.

Only move to Synthesis when every sub-question is ticked (or the user says to wrap up).

## Synthesis — only when the spawn tool told you to (or there's no spawn tool)

Autonomous runs do this for you — do NOT do it after an autonomous spawn. A single pass, never interleaved with searching:

1. If `compose_research_note` is in your toolbox: call it with `{topic, evidence_page_slug}` — the writer model reads the FULL evidence page server-side and composes the note. Then go straight to step 3.
2. Otherwise: `read_page` the evidence page — it is the sole input; do not run new searches while writing. Write the note per the SKILL.md output contract (deep notes run ~900–1,600+ words, exhaustive — err long; brevity/length-matching guidance does not apply to research). Cite the original web sources; do NOT link the evidence page (transient scratch, reaped after).
3. Chat summary ≤200 words.
4. Memory pass: 10–20 `save_fact` calls per the SKILL.md rules.
