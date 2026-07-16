# Running the E2E suite

Phases 1–3 per [TESTING-PLAN.md](../../TESTING-PLAN.md): smoke, auth,
notes-sync (two-device SSE), shards, public sharing, the security
audit invariants, wiki, home launchpad, chat folders — plus real-model
chat specs that run wherever a local llama-server is up.

## Prerequisites

- Go (matching the gateway's `go.mod`).
- Node 20+.
- A Postgres instance reachable from the test runner. The gateway
  runs its own migrations on boot, so an empty database is fine —
  but it has to be **a database you're willing to let the tests
  mutate**. Do not point this at production.

Set the DSN:

```sh
export FAMILIAR_TEST_DSN="postgres://familiar:pw@localhost:5432/familiar_e2e?sslmode=disable"
```

## First-time setup

```sh
cd tests/e2e
npm install
npx playwright install --with-deps chromium   # browsers; cached
```

On hosts where `playwright install` has no build (e.g. Ubuntu 26.04),
use a system Chromium instead:

```sh
export PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser
```

## Run

```sh
cd tests/e2e
npm test
```

The fixture builds `familiar-gateway` and `familiar-workspace` into
`tests/e2e/.bin/` on first run (subsequent runs hit Go's build
cache and are near-instant), picks two free ports, writes minimal
TOML configs into a temp directory, and waits for both processes
to answer health probes before any spec runs.

On failure, per-process stdout/stderr is captured into the temp
directory referenced in the error message. Look there first.

## Useful flags

```sh
npm test -- --grep workspace        # run one spec by name match
npm run test:headed                 # visible browser (none used yet, but available)
npm run report                      # open the last HTML report
```

## What IS here now

- `flows/smoke.spec.ts` — gateway health + workspace serves the SPA
  (minimal, admin off).
- `flows/auth.spec.ts` — the full WebAuthn ceremony end to end:
  first-run setup → register → auto-login → dashboard, then logout →
  log back in, plus a disabled-user-cannot-log-in spec.
- `flows/notes-sync.spec.ts` — the two-device sync suite ("the one
  that would have caught the clobber"): a clean device picks up a
  remote save via SSE without a reload; a dirty device refuses the
  refresh and surfaces the 409 conflict instead of clobbering; the
  If-Match precondition at the API level.
- `flows/shards.spec.ts` — shard CRUD + token mint/revoke (plaintext
  shown once, list only carries the prefix), cross-user isolation,
  and shard passkey enrollment driven through the real `#panel-shards`
  UI with the virtual authenticator.
- `flows/share-public.spec.ts` — `/p/{key}` renders on the configured
  public host and 404s on every other Host (spoofed Host headers, no
  DNS needed); disabled/unknown keys 404; the public render is
  anonymous, sets lockdown headers, and contains no console markup.
- `flows/security.spec.ts` — black-box HTTP checks for the audit
  invariants: registration is session-gated once a credential exists,
  identity headers (X-User-Email …) never authenticate through the
  proxy, a shard bearer token is not a console session, and disabling
  a user kills their live session on the next request.
- `flows/wiki.spec.ts` — books + membership roles (owner / writer /
  reader enforcement incl. live demotion), page revisions, slug reuse
  after soft-delete (the partial-unique-index regression), and
  cross-book page-id smuggling 404s.
- `flows/home.spec.ts` — the 4-quadrant launchpad: pinned + recent
  quadrants settle with live data, the weather widget falls back to
  the share-location prompt (no stored location + headless denies
  geolocation), the New-note action card lands in a fresh editor, and
  a pinned row routes to its note.
- `flows/folders.spec.ts` — chat folders: API lifecycle (rename, move
  in/out with the "" sentinel, delete falls back to unfiled via ON
  DELETE SET NULL), real HTML5 drag-to-fold against the sidebar tree,
  the row context menu's "Move out of folder", and owner scoping.
- `flows/chat.spec.ts` — the chat pipeline against a REAL local model
  (llama-server). Raw SSE through the proxy (token / done frames,
  model attribution) and the full chat surface round trip: send →
  streamed assistant bubble → both turns persisted → replay after
  reload. **Skips cleanly when no model server is reachable** —
  CI stays green; mainframe (and any box with a llama-server on
  `FAMILIAR_TEST_CHAT_MODEL_URL`, default `http://127.0.0.1:8090`)
  runs the real thing.
- `flows/scheduled.spec.ts` — SCHEDULED-ACTIONS-SPEC Phases 1+2:
  create an action targeting a note, run-now through the real
  pipeline, the report appends with a timestamped section, an OPEN
  editor live-refreshes via page-events, the panel lists the run, and
  the API enforces shapes + ownership (cross-user page targets
  refused). Phase 2: `on_content` answers the NOTHING_TO_REPORT
  sentinel with a real model and delivers nothing (note byte-for-byte
  untouched); a conversation target auto-creates its "Scheduled: …"
  thread and the report lands as an assistant message. Phase 3: a
  page_saved action fires ON ITS OWN when a note is saved in the
  watched book (no run-now anywhere in the test) and reports into a
  different book; a webhook action fires from an unauthenticated
  POST to its secret URL (garbage token 404s, rapid refire 429s).
  Skips without a model; cron timing + budgets + loop prevention +
  burst throttling are Go-tested, not E2E-tested.
- `flows/tooluse.spec.ts` — multi-step tool use, the
  TESTING-MAINFRAME.md headline: one chat turn reads the "Pancake
  Recipe" note via wiki tools and appends its ingredients to the
  "Grocery List" note. Asserts outcomes (ingredients landed, existing
  items survived, recipe untouched, `__TOOL_EFFECT__:note_changed`
  in the stream), not the exact tool route. The UI variant holds the
  grocery note open in a notes tab and proves it live-refreshes when
  the model's tool call lands. Skips without a model, like chat.
- `fixtures/authenticator.ts` — a Chromium CDP virtual WebAuthn
  authenticator (resident-key capable for the discoverable login).
- `fixtures/user.ts` — seeded approved user + live `admin_sessions`
  row + the `familiar_admin_session` cookie on a BrowserContext.
  Every spec except auth uses this instead of the ceremony; each test
  gets its own random-suffixed identity so nothing shares data.
- `helpers/seedDb.ts` — `resetAuth()` truncates the auth tables so the
  bootstrap setup view fires; re-runnable locally.

The auth stack runs admin-enabled via `start({ admin: true })`, with
a relying party bound to the workspace origin. WebAuthn requires a
"secure context", so the browser reaches the stack over **localhost**
(not 127.0.0.1) — the fixture handles this. `rp_id` is `localhost`
(the gateway strips the port when matching).

### If the auth spec fails — it now self-diagnoses

The spec fails *loudly* at the real cause instead of on an opaque
"#view-setup is hidden":

- It waits for the SPA's `boot()` to settle (the `#view-loading` view
  clears) before asserting — so a slow first DB-backed request can't
  cause a flaky timeout.
- Before the setup view, it probes `/console/api/auth/register/status`
  directly and asserts `credentials_registered === 0`. A non-zero
  count means the DB wasn't clean; a non-200 means the admin auth
  route isn't reachable (admin disabled, or the `/console/api` proxy
  isn't wired).
- `resetAuth()` verifies the truncate actually emptied
  `webauthn_credentials` and errors with the (redacted) DSN if not —
  the usual cause is `FAMILIAR_TEST_DSN` pointing at a different DB
  than the gateway.
- On any failure the gateway log tail is attached to the report and
  printed to the console (WebAuthn / admin-wiring errors land there).

Still-finicky bits worth knowing:

- **`rp_origins` must equal the browser origin exactly** (incl. the
  dynamic port). The fixture derives it from the workspace URL.
- The credential lives in a *per-test* virtual authenticator, so a
  dirty DB (real credential, no matching key) breaks login.
- `resetAuth()` truncates `users` **CASCADE**, which also clears
  dependent rows (conversations, profiles…). Intended for an isolated
  test DB — don't point it at anything you care about.

## What's NOT here yet

Per [TESTING-PLAN.md](../../TESTING-PLAN.md):

- Phase 4 visual snapshots (deliberately deferred — flaky, OS-bound).
- Deeper agentic scenarios (multi-turn tool conversations, memory
  extraction round-trips) — tooluse.spec.ts is the template.

CI lives at `.github/workflows/e2e.yml`: a `go-test` job (gateway +
workspace `go test ./...` against pgvector Postgres) and an `e2e` job
(this suite; the chat specs self-skip there — no model in CI).
Advisory-only until it proves it isn't flaky.
