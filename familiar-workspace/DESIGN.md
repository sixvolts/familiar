# Familiar — Design System & Interface Spec

The single canonical design doc for the Familiar workspace. It describes the
**shipped** interface — the tokens, type, shell, and components as they exist in
`familiar-workspace/static/` today, not an aspirational target. Where the code
and this doc disagree, the code wins; the [Known inconsistencies](#11-known-inconsistencies)
section at the end tracks the gaps worth closing.

Source of truth: `static/app.css` (`:root`, ~150 lines of tokens) and the
per-surface JS. Mobile (`static/mobile.css` / `mobile.js`) is a hand-maintained
partial mirror, not a shared import — it can and does drift.

---

## 1. Thesis

Familiar is a note-taking-and-collaboration tool built around personal AI. The
surface carries one idea: *your data, your AI, your rules* — quiet, inspectable,
trustworthy. Influences stated plainly: **Obsidian** (dense, calm, tools-for-thought),
**Sublime Text** (precise, keyboardy, minimal chrome), **Claude Desktop** (warm
restraint). Where other AI products shout, Familiar speaks at conversation volume.

Principles that the rest of this doc enforces:

- **Dark-only.** There is no light theme — no `prefers-color-scheme`, no
  `[data-theme]`, no `.light` class anywhere in `static/`. The palette is a single
  cool graphite band; never true `#000000`.
- **One accent, sparing.** Iris purple `#6A4CE0` is the only signature accent.
  Aim for one full-strength iris moment per view.
- **Color means something.** Beyond iris, color is reserved for the four+one
  category signals and semantic states (success / warn / danger / info). The body
  of a document is neutral graphite.
- **Content before chrome.** The chrome is a two-column shell that gets out of the
  way. No title bar, no status bar, no tab-of-tabs, no colored strips.
- **Type is the second voice.** Geist does the work; mono signals structure.

---

## 2. Color

### 2.1 Graphite ramp — surfaces + text (`app.css:16`)

| Token | Hex | Role |
|---|---|---|
| `--graphite-950` | `#0B0B0F` | canvas (deepest) |
| `--graphite-900` | `#101014` | default surface — sidebar, tab bars |
| `--graphite-850` | `#15151B` | raised — menus, code blocks, palette |
| `--graphite-800` | `#1B1B23` | card — elevated containers |
| `--graphite-750` | `#22222C` | hover surface |
| `--graphite-700` | `#2B2B36` | active / pressed; scrollbar thumb |
| `--graphite-600` | `#3A3A48` | subtle border in raised contexts |
| `--graphite-500` | `#52525F` | tertiary text / placeholders |
| `--graphite-400` | `#7A7A86` | secondary text / metadata |
| `--graphite-200` | `#C9C9D1` | body text |
| `--graphite-050` | `#F4F4F7` | primary text / headings |

(`-300 #A3A3AD`, `-100 #E6E6EB` fill the ramp.)

### 2.2 Iris — signature accent (`app.css:32`)

`--iris-500 #6A4CE0` is the default primary, caret, and focus ring. Ramp:
`-600 #5138B0` (pressed) · `-500 #6A4CE0` · `-400 #8A70EC` (hover) · `-300 #AC98F3`
(selected pill text, keywords) · `-200 #CEC2F8` (link hover) · `-100`/`-050` tints ·
`-900 #1C1636`/`-950 #120E22` deep grounds. The ramp is intentionally
non-contiguous — only the steps actually used are defined.

### 2.3 Supporting ramps

- **Slate** (info): `--slate-500 #4E6AAD` · `-400 #7E97D4` · `-300 #B0C0E4`
- **Sunlamp** (mark / warn): `--sunlamp-500 #C69A27` · `-400 #E8BE55` · `-300 #F1D284`
- **Moss** (success): `--moss-500 #3E9A6A` · `-400 #5CB585`
- **Teal** (Actions/Scheduled): `--teal-500 #14B8A6` · `-400 #2DD4BF`
- **Ember** (danger): `--ember-500 #D0514A` · `-400 #E37A74`
- **Tangerine** (diagram tabs): `--tangerine-500 #E8853D` · `-400 #F2A263`

### 2.4 Semantic tokens (`app.css:64`)

Style through these, not the raw ramps.

```
--bg-canvas / -surface / -raised / -card / -hover / -active   → graphite 950…700
--fg-1  #F4F4F7 (primary)   --fg-2  #C9C9D1 (body)
--fg-3  #7A7A86 (secondary) --fg-4  #52525F (tertiary/placeholder)

--accent          iris-500     --accent-hover  iris-400   --accent-press iris-600
--accent-soft-fg  iris-300     --accent-soft-bg rgba(106,76,224,0.14)
--accent-ring     rgba(106,76,224,0.35)   --selection-bg rgba(138,112,236,0.28)

--info  slate-500  · --info-soft-fg slate-300 · --info-soft-bg rgba(126,151,212,0.12)
--mark  sunlamp-400 · --warn-soft-fg sunlamp-300 · --warn-soft-bg rgba(232,190,85,0.12)
--success moss-400
--danger  ember-500 · --danger-hover ember-400 · --danger-rgb 208,81,74
```

Borders are white-alpha hairlines: `--border-subtle rgba(255,255,255,0.06)`,
`--border-default …0.10` (the standard hairline), `--border-strong …0.16`. A
strong border is *earned* via hover/focus, never drawn with a color strip.

### 2.5 Category colors — the four+one signal

Each sidebar category owns one color, used in exactly three places: the **12–14px
glyph** next to its name, the **active row tint** (`--cat-soft`, a ~12–18% alpha
of the color — background tint only, **no left bar**), and the **2px top stripe**
on every workspace tab of that category. Nowhere else.

| Category | Token | Hex | Soft tint |
|---|---|---|---|
| Chat | `--moss-500` | `#3E9A6A` | `rgba(62,154,106,0.16)` |
| Notes | `--iris-500` | `#6A4CE0` | `rgba(106,76,224,0.14)` |
| Wiki | `--slate-500` | `#4E6AAD` | `rgba(78,106,173,0.18)` |
| Shards | `--sunlamp-500` | `#C69A27` | `rgba(198,154,39,0.18)` |
| Actions | `--teal-400` | `#2DD4BF` | `rgba(45,212,191,0.14)` |

(Diagram tabs, which aren't a sidebar category, stripe in `--tangerine-500`.)

---

## 3. Typography

Two families do all the work; a third is declared but dead.

- **`--font-sans` = Geist** — UI, chrome, body, headings, brand/wordmark, and
  display. 14px base, weight 400–500 for body, 600 for headings/wordmark, tracking
  `-0.01em`. There is no separate display face — Geist 600 with negative tracking
  *is* the display treatment.
- **`--font-mono` = Geist Mono** — structure: eyebrows, metadata, counts, IDs,
  keyboard hints, code, and numeric table columns (`font-variant-numeric:
  tabular-nums` where figures align). Usually 11px (`--text-2xs`) weight 500;
  uppercase eyebrows add `text-transform: uppercase` + `0.12em`.
- **`--font-serif` = Instrument Serif** — **declared but never rendered.** No CSS
  rule or JS uses `var(--font-serif)`; the few italic uses in the app are synthetic
  italic on Geist. The token, the Google-Fonts request for it, and two stale
  comments are leftovers (see §11). The redesign dropped serif.

Type scale (`app.css:116`): `--text-2xs 11 · -xs 12 · -sm 13 · -md 14 (base) ·
-lg 16 · -xl 18 · -2xl 22 · -3xl 28 · -4xl 36`.

Fonts are **CDN-only** (Google Fonts, loaded in `index.html`); none are
self-hosted. **Mobile loads no web fonts at all** — `mobile.html` has no font
link, so it falls back to `ui-sans-serif` / `ui-monospace` unless Geist is
installed at the OS level (see §11).

---

## 4. Layout & app shell

### 4.1 The two-column grid

`.view-shell` is a single-row, two-column grid: `grid-template-columns:
var(--sidebar-w, 240px) 1fr` (sidebar | content), full viewport height. The shell
never scrolls — scrolling lives inside individual panels. At `≤768px` the sidebar
narrows to 200px; it stays a two-column height-clamped layout.

**There is no title bar and no status bar.** Both were removed: the 40px F-mark+
breadcrumb title bar duplicated the sidebar header, and the 28px Sync/Vault/Rules
status row was ambient clutter. The grid collapsed from three rows to one. A
`window.familiarStatusBar.setContext()` no-op stub remains so old call sites don't
break. Metadata the status bar used to show now lives inline where it's relevant
(e.g. the chat thinking-trace metrics line).

### 4.2 Content column

`.content` (grid col 2) is `display: flex; flex-direction: column; padding: 0;
overflow: hidden`. It's full-bleed — each panel provides its own padding. Its
direct children are the **maintenance-banner host** (a sibling *above* the panels,
so it takes its own height and the active panel flexes into the remainder — this
is why the banner can't clip the composer) followed by the stack of top-level
`.panel` "modes", all but one carrying `hidden`.

Modes (via `appSwitchPanel(name)`): `home` · `workspace` · `user` · plus reading-
column panels (`dashboard`, `system-status`, `users`, `shards`) that get a
centered `max-width: 1080px; padding: 32px 40px 56px`. Home and the User/Config
panel *replace* the workspace; they are modes, not tabs.

**Maintenance banner** — a conditional strip fed by `/auth/status` (boot + 90s
watchdog). Amber: `background: linear-gradient(rgba(232,190,85,0.16), …),
var(--graphite-950); border-bottom: 1px solid rgba(232,190,85,0.45)` with a `⚠`
icon in `--sunlamp-400`. Empty host = 0 height, so the no-banner layout is
unchanged.

### 4.3 Sidebar

`<nav class="sidebar">` is the single navigation rail — `background: var(--bg-surface)`
(`#101014`), a flex column, **no right border** (the surface→canvas color step
`#101014`→`#0B0B0F` is the boundary). Top to bottom:

1. **Header** — the Familiar `F` mark (25×29 SVG, accent path + a `--mark` dot) and
   the "Familiar" wordmark (Geist 20px/600). No search input, no environment chip.
2. **Home row** — a muted `⌂` in `--fg-3`, never tinted. Active = neutral
   `--bg-card`, no color. Home is chromatic-neutral so it doesn't compete with the
   category colors.
3. **Category rows** — the only scroll region. **Five** categories in order:
   **Chat · Notes · Wiki · Shards · Actions**. Each row is a 26px grid
   (`glyph 18px | label | count | +new | chevron`); the glyph is the only colored
   element; the count is mono/tabular in `--fg-4`; the `+` is a ghost button that
   tints to the category color on hover; the chevron rotates 90° when expanded.
   Active category = `--cat-soft` background tint (no left bar). Children indent
   under an expanded category and share one neutral style across all categories
   (label + right-aligned mono timestamp/count; pins get a `★` in sunlamp; Notes
   folders nest further).
4. **Spacer** (`flex: 1`) pins the footer down.
5. **User footer** — a bubble (`--bg-card`, hairline border, `--radius-md`) with a
   28px iris-gradient avatar + name + chevron. Opens the User/Config panel (Account
   / Memory / Admin subnav; **Sign out** lives here and nowhere else). The old
   secondary nav block (Memory, Profile, Sessions, Skills, Users, System status)
   was deleted from the sidebar and re-homed into this panel.

Default width 240px, user-resizable via a JS-revealed handle (`--sidebar-w`).

### 4.4 Workspace, tabs, splits

`#panel-workspace` is a flex column: a thin toolbar, then a split grid.

- **Toolbar** (`.ws-toolbar`, on canvas) — right-aligned: a **layout picker** of
  four ghost icon buttons (`single` · `split-2x1` "side by side" · `split-1x2`
  "stacked" · `t-top`), active state in `--accent-soft-bg`/`--accent-soft-fg`; then
  a **zoom picker** (100 / 110 / 120%). The old "Workspace" eyebrow was dropped.
- **Split grid** (`.ws-grid`) — panes separated by a 5px canvas gutter (borderless
  panes; the divider is just canvas showing through). Track fractions are custom
  props with a `MIN_TRACK_FRACTION` floor of 15%. Fresh state defaults to
  `split-2x1` with slot A active. Removed layouts (quad, t-bottom/left/right) alias
  onto survivors from old localStorage.
- **Pane** (`.ws-panel`) — borderless flex column on `--bg-surface`. The **focused
  pane** is marked by a subtle inset top hairline (`box-shadow: inset 0 1px 0 0
  var(--border-default)`), not a full ring. Clicking anywhere in a pane focuses it.
- **Tab bar** (`.ws-tab-bar`, 32px, `border-bottom: 2px solid var(--border-default)`)
  — flat tabs, no rounded corners. A tab's only color is a **2px top-edge stripe**
  in its category color (`::before`, opacity 0.35 inactive → 1.0 active); the active
  tab body steps up to `--graphite-900` with `--fg-1` text. A 12px trailing slot
  holds a `--sunlamp-400` **dirty dot** that swaps to a `×` close on hover. Tabs and
  the tab bar support drag-reorder / drag-between-panes with iris drop indicators. A
  trailing ghost `+` adds a tab; overflow chevrons appear when the strip fills.
- **New tabs always open in the left-most pane** (`slots[0]`), never the active/
  right pane. A single empty "splash" tab in the left slot is reused rather than
  stacking a second; a sidebar click never overrides a doc-bearing tab. Re-selecting
  a surface returns you to its last-open tab without resetting it.

---

## 5. Components

Standard transition idiom: `120ms ease` on background/border/color/box-shadow/transform.

**Buttons** (all 40px tall, Geist 13px/500, `-0.01em`):

- **`.btn-accent`** — primary, "one per view max". Iris fill, white text,
  `--radius-md` (10px — *not* a pill), `padding: 0 18px`. Focus = double ring
  (`0 0 0 2px var(--bg-canvas), 0 0 0 4px var(--accent-ring)`), no outline. Active
  presses to `--accent-press` + `scale(0.99)`. Disabled → `--bg-active`/`--fg-4`.
- **`.btn-ghost`** — supporting: transparent, `--fg-1`, hairline border; hover
  raises to `--bg-hover` + `--border-strong`.
- **`.btn-small`** — 32px, 12px, `padding: 0 12px`.
- **`.btn-danger`** — filled ember (destructive, detail views); `.btn-ghost.is-danger`
  is the quiet list-card variant (ember text, faint red hover).
- **Ghost icon buttons** — the recurring "no chrome until hover" idiom (sidebar
  `+new`, tab `+`, tab-scroll chevrons, row chevrons): transparent, `--fg-4`, tint
  to their context color on hover.

**The composer Send / Stop control** — the Send button is
`btn-accent btn-small chat-send-btn` forced back to 40px to match the textarea
(an iris accent button, 10px radius). While a turn streams it morphs into the
red-octagon Stop — see §6.2.

**Cards** — `.card`: `--bg-card`, `--border-default`, `--radius-xl` (18px),
`padding 20px`, `--shadow-1`; hover raises the border + `--shadow-2`. Modals/
popovers use `--radius-2xl`/`-lg` on raised/canvas grounds with `--shadow-3`.

**Inputs** — `.input`/`.field input`: `--bg-raised`, `--border-default`,
`--radius-sm` (6px), `padding 10px 12px`, 14px; focus drops the outline for
`border-color: var(--accent)` + a 2px `--accent-ring`. `.label` is *the* eyebrow:
mono 11px, `0.12em`, uppercase, `--fg-3` — the only uppercase treatment in the
system.

**Chips / badges** — tags: `--radius-xs` (4px), mono 12px, hairline. `.status-pill`:
pill, mono 11px uppercase `0.08em`, state variants (`pending` sunlamp, `approved`
moss, `denied`/`disabled` ember/grey). `.chat-model-badge` and `.chat-msg-tools`
follow the same mono-pill language in accent-soft / info-soft.

**Code / kbd** — inline `code`: mono `0.92em`, `--graphite-850` ground, hairline,
`--radius-xs`. Code blocks: `--bg-raised`, hairline, `--radius-sm`, `overflow-x: auto`.
`.kbd-hint`: mono 11px keycap (hairline border with a 2px bottom for depth).

**Radii scale**: `--radius-xs 4 · -sm 6 · -md 10 · -lg 14 · -xl 18 · -2xl 24 ·
-3xl 32 · -pill 999`. **Shadows** are low, wide, and purple-tinted
(`--shadow-1…3`, plus `--shadow-glow` = the iris focus glow).

---

## 6. The working indicator, Stop, and research card

These three chat surfaces are the most distinctive recent additions; the pixel
indicator is the through-line.

### 6.1 The pixel indicator (`.rc-px`) — the shared "working" signal

One global component, used by **both** the chat "Thinking" line and every
research-roster row. It replaced the old blinking body caret.

- **Shape** — a three-column staircase: columns of 1 · 2 · 3 stacked pixels,
  bottom-aligned. Each pixel is `3×3px` (`border-radius: 1px`), with a `3px` gap on
  both axes. Resting fill is 26% of the state color.
- **Color** — `--pc` (pixel color) is swapped per state from three RGB triples
  (declared on `:root` for iris, and on the research card for all three):
  `--rc-iris 154,124,245` (a working-purple, ≈ iris-400) · `--rc-moss 92,181,133`
  (= moss-400) · `--rc-amber 224,164,76`.
- **States** — `active` → `.rc-px-anim`, the marching pulse (the only animated
  state); `done` → `.rc-px-moss.rc-px-fill`, solid moss at 90%; `failed` →
  `.rc-px-amber`, static at rest opacity; `queued` → `.rc-px-dim`, faint 14%.
- **Motion** — `@keyframes rc-pxpulse` (1.35s): rest at 26%, a 100% bright flash
  with a soft glow at the 13% mark, back to rest by 42%. Per-column stagger (delays
  0 / 0.16 / 0.32s) makes the flash **march left→right** across the staircase.
- **Reduced motion** — `prefers-reduced-motion` kills the animation and holds
  active pixels steady at 60%.

**On the chat "Thinking" line** (`.chat-think-px`): shown immediately, before the
first token, next to a status label that swaps "Thinking" → "Searching the web" →
"Using a skill" → "Spawning research workers" (via `friendlyActivity()`). It
**drops** the instant real answer text streams ("hand the motion to the text"),
and **relights** whenever the model re-enters a working phase — a tool/search
status arrives, or reasoning resumes after a tool returns — so it stays lit
through the silent tool-execution wait instead of vanishing on a short preamble.
Real answer tokens re-drop it.

### 6.2 Stop control — Send morphs to a red octagon

While a turn streams, the Send button becomes a small solid **red octagon**
(`.chat-stop-oct`: `clip-path: polygon(30% 0,70% 0,100% 30%,100% 70%,70% 100%,
30% 100%,0 70%,0 30%)`, `background: var(--danger)` #D0514A) with a 1.7s pulse
ring. 22px in the composer, 18px in the research-card header, 20px on mobile.
Clicking cuts generation **server-side** (`POST /api/chat/stop`, which cancels the
turn and commits the partial produced so far) **and** aborts the local fetch for
instant feedback / fallback. Reduced motion drops the pulse.

### 6.3 Research progress card (`.chat-research-card`)

A card rendered in the chat while an autonomous deep-research run is active,
rebuilt from a 5s poll of the run state (`GET …/research/runs/active`).

- **Frame** — iris-outlined (`border: 1px solid var(--iris-600)` = #5138B0),
  `--radius-lg`, an iris-tinted gradient over `--bg-card`, a soft iris drop shadow.
  **Inset ~15% and centered** (`margin: 10px 7.5% 6px`) so it reads as a distinct
  panel, not edge-to-edge. Clicking the body reopens the live evidence view in the
  right pane.
- **Header** — "Researching" + the curly-quoted topic, a subtitle, and the
  red-octagon **stop** (cancels the run server-side; the next poll clears the card).
- **Roster** — one row per fan-out worker: the `.rc-px` indicator (active iris /
  done moss / failed amber / queued dim) + the worker's sub-question + a state word
  ("searching" / "done" / "no result" / "queued"). Capped ~5 rows, then scrolls.
- **Meta row** — mono `round · areas (N/total) · tokens`.
- **Synthesis collapse** — when the run enters `synthesizing`, the roster collapses
  to exactly two rows: a single **"Research workers"** row (done/green, or amber
  only if the whole fan-out came back empty) and a live **"Writing the note"** row
  (active pulse) — so the card keeps an animated signal through the write-up.
- **No-flicker updates** — a `status|round|topic|total` structure key drives an
  incremental patch (only changed rows + counters) vs a full rebuild, so the other
  rows' pulses keep marching without restarting.

---

## 7. Depth, motion, whitespace

- **Elevation** by surface step (canvas → surface → raised → card → hover → active),
  reinforced by the low/wide purple-tinted shadows — never by bright borders.
- **Motion is functional**: the 120ms component transitions, the indicator pulse,
  and the stop pulse. The single decorative flourish is one low-opacity iris glow
  behind a view's hero element — one per view, never more. Everything honors
  `prefers-reduced-motion`.
- **Whitespace** does the separating: flex/grid `gap`, per-panel padding, hairline
  dividers. There is no `--space-*` scale — spacing is literal px, kept tight
  because the product is dense.

---

## 8. Do / Don't

**Do**
- Style through semantic tokens (`--bg-*`, `--fg-*`, `--accent*`, `--border-*`).
- Keep iris to one full-strength moment per view; use `--accent-soft-*` for the rest.
- Signal category with the glyph + soft active tint + tab top-stripe — those three
  places only.
- Use mono for metadata/eyebrows/counts/code and `tabular-nums` where figures align.
- Earn strong borders via hover/focus; default to hairlines.

**Don't**
- Add a light theme, a title bar, a status bar, per-document icons, or a colored
  status strip — all deliberately removed.
- Draw a left accent bar on active sidebar rows (the soft tint carries it).
- Use ALL-CAPS except the mono eyebrow. No Title Case.
- Reach for serif — the token is dead; use Geist.
- Introduce a new accent color for a one-off; map it onto an existing semantic role.

---

## 9. Responsive & mobile

The desktop shell stays a two-column grid down to a 200px sidebar at `≤768px`.
Mobile is a separate build (`mobile.html` / `mobile.js` / `mobile.css`) with its
own bottom-tab shell. It **mirrors** the token set and the chat/research components
(the pixel indicator, stop octagon, and a full-width research *strip*) by hand —
so treat `mobile.css` as a partial copy that can drift: it abbreviates `--sunlamp`
to `--sun`, hardcodes several state colors as hex, and loads **no web fonts**
(Geist falls back to the system UI font). Keep the two in sync deliberately.

---

## 10. Token quick-reference

```css
/* Grounds */         --bg-canvas #0B0B0F  --bg-surface #101014  --bg-raised #15151B
                      --bg-card #1B1B23     --bg-hover #22222C     --bg-active #2B2B36
/* Text */            --fg-1 #F4F4F7  --fg-2 #C9C9D1  --fg-3 #7A7A86  --fg-4 #52525F
/* Accent (Iris) */   --accent #6A4CE0  --accent-hover #8A70EC  --accent-press #5138B0
                      --accent-soft-fg #AC98F3  --accent-soft-bg rgba(106,76,224,0.14)
/* Categories */      chat #3E9A6A · notes #6A4CE0 · wiki #4E6AAD · shards #C69A27 · actions #2DD4BF
/* Semantic */        --success #5CB585  --danger #D0514A  --mark #E8BE55  --info #4E6AAD
/* Indicator */       --rc-iris 154,124,245  --rc-moss 92,181,133  --rc-amber 224,164,76
/* Borders */         --border-subtle/-default/-strong  rgba(255,255,255, .06/.10/.16)
/* Radii */           --radius-xs 4 · -sm 6 · -md 10 · -lg 14 · -xl 18 · -2xl 24 · -pill 999
/* Type */            --font-sans "Geist"  --font-mono "Geist Mono"   (serif token unused)
```

---

## 11. Known inconsistencies

Reality the code carries that a future cleanup should resolve (these are *bugs /
drift*, documented so the doc stays honest):

- **Undefined radius aliases render square.** `--radius-card`, `--radius-input`,
  and `--radius-popover` are referenced (skill/dash cards, several inputs) but never
  defined and have no `var()` fallback → they resolve to `0`. Those elements have
  sharp corners despite the "soft radii" intent. Define them (≈ 6 / 6 / 14px) or
  replace with the real scale.
- **Dead serif.** `--font-serif` (Instrument Serif) is defined and the italic
  weight is still downloaded via Google Fonts, but nothing renders it. Drop the
  token, the font request, and the two stale "serif display" comments.
- **Undefined `--moss-300` / `--warn`.** Referenced by research-roster state colors
  and status pills; they resolve via inline literals `#82cfa4` / `#e0a44c`. Promote
  them to real tokens.
- **Mobile has no web fonts.** `mobile.html` loads neither Google Fonts nor
  `@font-face`, so Geist silently falls back on mobile. Add the font link (or
  self-host) if the Geist look matters on mobile.
- **Mobile token drift.** `mobile.css` is a hand-copied partial of the token set
  (`--sun-*` vs `--sunlamp-*`, hardcoded state hex). Consider sharing one tokens file.
- **Two stale code comments**: `.ws-toolbar`'s comment claims a `--bg-surface`
  background (it actually paints canvas), and `.chat-research-card`'s comment says
  "moss-tinted" (it's iris-tinted). Fix the comments, not the pixels.
