# Familiar — Test Pipeline Setup

How to stand up the full Familiar test pipeline on a fresh machine — the Go
suites (`familiar-gateway` + `familiar-workspace`) and the Playwright end-to-end
suite — and run everything green. Written against the reference box (Ubuntu 26.04,
AMD Strix Halo), with host-specific steps flagged so you can adapt to other hardware.

> Keep this doc in sync when the pipeline changes. It was generated 2026-07-31 by
> inspecting the live repo + machine; version numbers are what that box actually ran.

## What "green" looks like

- **Go** — ~886 `familiar-gateway` + ~6 `familiar-workspace` unit tests (hermetic),
  plus DB-backed integration tests.
- **Playwright E2E** — **115 tests**. 94 run without a model; **21 are model-gated**
  and `test.skip` unless a live inference server is reachable.
- With the inference service up, the whole suite is **115 / 115**.

## The shape of it

| Layer | Where | Needs |
|---|---|---|
| Go unit | `familiar-gateway`, `familiar-workspace` | Go toolchain only |
| Go integration | same, `-tags`/DSN-gated | + PostgreSQL + pgvector (`FAMILIAR_TEST_DSN`) |
| Playwright E2E (94) | `tests/e2e` | + Node/npm, system Chromium, DB |
| Playwright E2E (+21) | `tests/e2e` (model-gated) | + a live OpenAI-compatible model with tool-calling |

## Fast path (toolchain already installed)

If Go, Node, PostgreSQL+pgvector, and a system Chromium are already present, this is
the whole loop (details in the sections below):

```sh
# one-time: DB (see "Database setup") + e2e deps
make e2e-setup                       # npm install in tests/e2e

# env — export for every run
export FAMILIAR_TEST_DSN="postgresql://familiar_test:familiar_test@localhost:5432/familiar_test?sslmode=disable&options=-csearch_path%3De2e_test%2Cpublic"
export PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser

# run the suites
make test              # Go unit, both modules
make test-integration  # Go + DB
make test-e2e          # Playwright (94 run; 21 skip without a model)

# add the model-gated 21:
sudo systemctl start familiar-mainframe-llama.service        # ~15s–few min to load
FAMILIAR_TEST_CHAT_MODEL_URL=http://127.0.0.1:8080 make test-e2e
```

**Setting up a brand-new machine?** Work through the sections in order.

## Sections

1. [Prerequisites & system packages](#prerequisites--system-packages)
2. [Database setup (PostgreSQL + pgvector + test DB/schema)](#database-setup-postgresql--pgvector--test-dbschema)
3. [Go: build & test (gateway + workspace)](#go-build--test-gateway--workspace)
4. [Playwright E2E setup & the Chromium gotcha](#playwright-e2e-setup--the-chromium-gotcha)
5. [Make targets reference](#make-targets-reference)
6. [Local inference model service (for the 21 model-gated E2E specs)](#local-inference-model-service-for-the-21-model-gated-e2e-specs)

---

## Prerequisites & system packages

Base toolchain a fresh box needs before you touch the repo. Target: **Ubuntu 26.04 LTS (resolute), x86_64** — the same profile as the machine these versions were probed on.

### Required versions (verified on this box)

| Component | Requirement (source of truth) | This machine runs |
|---|---|---|
| Go | `go 1.25.0` directive in **both** `familiar-gateway/go.mod` and `familiar-workspace/go.mod` — a floor, anything ≥ 1.25 works | `go1.26.0` |
| Node.js | 20+ (`tests/e2e/MAKE_TESTS.md`; `@types/node ^20`, `@playwright/test ^1.49.0`) | `v22.22.1` |
| npm | ships with Node | `9.2.0` |
| PostgreSQL | server **18** + client | `18.4` |
| pgvector | `CREATE EXTENSION vector` — the only extension the migrations create (`internal/db/migrate.go:121`) | `postgresql-18-pgvector 0.8.1-2` |
| Chromium | system browser for Playwright (see Chromium note) | snap `150.0.7871.128` |
| git | any recent | `2.53.0` |

### 1. Base apt packages

```sh
sudo apt update
sudo apt install -y build-essential curl git ca-certificates
```

### 2. Go (official tarball — guarantees ≥ 1.25.0)

apt's `golang` can lag behind the go.mod floor; install upstream to match this box:

```sh
curl -fsSLO https://go.dev/dl/go1.26.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh >/dev/null
export PATH=$PATH:/usr/local/go/bin
go version   # -> go1.26.0
```

### 3. Node.js 22 + npm (NodeSource — matches this box; 20+ is the floor)

```sh
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt install -y nodejs
node --version   # -> v22.x
npm --version
```

### 4. PostgreSQL 18 + pgvector (both in the 26.04 archive — no PGDG repo needed)

```sh
sudo apt install -y postgresql postgresql-client postgresql-18-pgvector
```

- The `postgresql` metapackage pulls **major 18** on 26.04.
- `postgresql-18-pgvector` **must match the server major** — the gateway's migrations run `CREATE EXTENSION IF NOT EXISTS vector` on boot (embeddings are `vector(768)` with an HNSW index).
- **`vector` is the only extension the test pipeline needs.** UUID primary keys use core `gen_random_uuid()` (built into PostgreSQL 13+), not `uuid-ossp`. The `uuid-ossp` line in `init-db.sql` belongs to the legacy `docker-compose` bootstrap and is unused by the Go migration path.

### 5. System Chromium (Ubuntu 26.04 is snap-only — see gotchas)

```sh
sudo snap install chromium
export PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser
```

Playwright's bundled Chromium has **no 26.04 build**, so `playwright.config.ts` / `screenshot.config.ts` honor `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` to point at a system browser. `/usr/bin/chromium-browser` is a wrapper that dispatches to the snap. Persist the export in your shell profile so `npm test` picks it up.

### 6. Verify the toolchain

```sh
go version
node --version && npm --version
psql --version
dpkg -l postgresql-18-pgvector | tail -1
chromium --version
git --version
```

---

## Database setup (PostgreSQL + pgvector + test DB/schema)

The test pipeline talks to one Postgres cluster. Every DB-backed Go test (`testutil.PgTestPool` → `db.Migrate`) and the whole Playwright E2E suite read the connection string from **one** env var, `FAMILIAR_TEST_DSN`. You do **not** apply migrations by hand: the gateway runs `db.Migrate` on boot (`familiar-gateway/cmd/gateway/main.go:357`) and the Go integration harness runs it per-package (`familiar-gateway/internal/testutil/db.go:64`). Both create the app tables and issue `CREATE EXTENSION IF NOT EXISTS vector`. Your job is only to stand up the cluster, the role, the database, the extension, and the isolated schema.

### 1. Install PostgreSQL + pgvector

On this box (Ubuntu 26.04) both are in the distro repo — **no PGDG apt repo needed**. Verified here: `psql (PostgreSQL) 18.4 (Ubuntu 18.4-0ubuntu0.26.04.1)` and package `postgresql-18-pgvector 0.8.1-2`.

```bash
sudo apt update
sudo apt install -y postgresql-18 postgresql-18-pgvector
```

> Host-specific: the `18` is the major version Ubuntu 26.04 ships. On a distro whose default repo predates pgvector packaging (or a different PG major), add the PGDG repo and install `postgresql-<N>` + `postgresql-<N>-pgvector` for that same `<N>` — the pgvector package **must** match the server major or `CREATE EXTENSION vector` won't find the `.so`.

The install auto-creates and starts a cluster. Confirm it's listening on 5432:

```bash
pg_lsclusters                      # expect: 18 main 5432 online
systemctl status postgresql@18-main.service --no-pager
```

Local auth is already usable as shipped: `pg_hba.conf` has `local … peer` (so `sudo -u postgres psql` works with no password) and `host … 127.0.0.1/32 scram-sha-256` (so the app logs in over TCP with a password). No `pg_hba` edits required.

### 2. Create the role, database, extension, and schema

`CREATE EXTENSION vector` requires superuser (pgvector is not a "trusted" extension), so create it **once as `postgres`**. After that the gateway's `CREATE EXTENSION IF NOT EXISTS vector`, running as the unprivileged `familiar_test` role, is a harmless no-op.

```bash
# Role + database (superuser via peer auth)
sudo -u postgres psql <<'SQL'
CREATE ROLE familiar_test WITH LOGIN PASSWORD 'familiar_test';
CREATE DATABASE familiar_test OWNER familiar_test;
SQL

# Extension + isolated schema — must run INSIDE the target database
sudo -u postgres psql -d familiar_test <<'SQL'
CREATE EXTENSION IF NOT EXISTS vector;              -- installs the `vector` type into public
CREATE SCHEMA IF NOT EXISTS e2e_test AUTHORIZATION familiar_test;
GRANT USAGE ON SCHEMA public TO familiar_test;      -- lets familiar_test resolve public.vector
SQL
```

Only the `vector` extension is needed. The migrations use core `gen_random_uuid()` (built into PG 13+), **not** `uuid-ossp`/`pgcrypto` — the `uuid-ossp` reference in the repo's `init-db.sql` is the legacy docker-compose schema and is unused by the Go migration path.

### 3. Export the DSN

Combine the known-good credentials with the schema-isolation options. **Single-quote the whole value** so bash doesn't treat `&` as a job-control operator, and keep the percent-encoding (`%3D` = `=`, `%2C` = `,`) intact:

```bash
export FAMILIAR_TEST_DSN='postgres://familiar_test:familiar_test@localhost:5432/familiar_test?sslmode=disable&options=-csearch_path%3De2e_test%2Cpublic'
```

Put that line in your shell profile so `make test-integration` / `make test-e2e` pick it up (the `guard-dsn` Make target refuses to run without it). Use the **URL form** shown — the bare `user:pass@host` form fails `db.Open`.

### 4. Verify

```bash
psql "$FAMILIAR_TEST_DSN" -c 'SHOW search_path;'          # -> "e2e_test, public"
psql "$FAMILIAR_TEST_DSN" -c "SELECT '[1,2,3]'::vector;"  # -> proves `vector` resolves
```

Then let the gateway build the schema for you (first E2E/integration run boots it and migrates into `e2e_test`):

```bash
cd /home/sixvolts/repos/familiar && make test-integration
# spot-check the app tables landed in the e2e_test schema, not public:
psql "$FAMILIAR_TEST_DSN" -c '\dt e2e_test.*'   # users, memories, conversations, wiki_pages, ...
```

### Why this exact shape

- **`e2e_test` gets its own schema.** The E2E suite issues `TRUNCATE … users CASCADE` between specs (see `tests/e2e/flows/auth.spec.ts`, `security.spec.ts`). Nearly every app table has an FK back to `users` (`conversations`, `notes`, `books`, `wiki_pages`, `shards`, …), so that CASCADE wipes essentially the entire dataset. Isolating the E2E tables in their own schema means that reset can't reach into anything you care about (dev data, or integration fixtures) sitting in `public`.
- **`public` must stay on the `search_path`.** `CREATE EXTENSION vector` installs the `vector` type into `public` (a cluster-shared, superuser-owned extension you don't want duplicated per schema). Every migration and query references `vector` **unqualified** (e.g. `memories.embedding vector`, `$1::vector`). With `search_path = e2e_test, public`, new tables are created in `e2e_test` (first writable schema) while the unqualified `vector` type still resolves via the `public` fallback. Drop `public` from the path and the very first migration (`memories_base`, which does `CREATE EXTENSION … vector` + `embedding vector`) fails to resolve the type.

---

## Go: build & test (gateway + workspace)

Familiar is **two independent Go modules**, each with its own `go.mod` — there is no root `go.mod` or `go.work`, so a repo-root `go build ./...` covers **neither**. You must `cd` into each module:

| Path | Module | Binary (`cmd/`) |
|------|--------|-----------------|
| `familiar-gateway/` | `github.com/familiar/gateway` | `./cmd/gateway/` |
| `familiar-workspace/` | `github.com/familiar/workspace` | `./cmd/workspace/` |

Both `go.mod` files declare `go 1.25.0` as the floor. This box runs **go1.26.0**, which satisfies it.

### 1. Install the Go toolchain (fresh machine)

Ubuntu's `apt` Go often lags behind 1.25. Install from the official tarball to match this box:

```bash
GO_VER=1.26.0
curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" -o /tmp/go.tgz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile   # persist
export PATH=$PATH:/usr/local/go/bin
go version   # -> go version go1.26.0 linux/amd64
```

Any Go `>= 1.25.0` works; older toolchains are rejected by `go.mod`.

### 2. Build both modules

```bash
cd /home/sixvolts/repos/familiar
make build            # = build-gateway + build-workspace
```

`make build` runs, per the Makefile:

```bash
cd familiar-gateway   && go build -o familiar-gateway   ./cmd/gateway/
cd familiar-workspace && go build -o familiar-workspace ./cmd/workspace/
```

To compile **all** packages (not just the binary) and vet each module:

```bash
cd familiar-gateway   && go build ./... && go vet ./...
cd ../familiar-workspace && go build ./... && go vet ./...
```

### 3. Hermetic unit tests (no DB, no services, ~2s)

```bash
cd /home/sixvolts/repos/familiar
make test
```

Raw equivalent — note the `cd` into **each** module and the mandatory `-count=1`:

```bash
cd familiar-gateway   && go test -count=1 ./...
cd ../familiar-workspace && go test -count=1 ./...
```

DB-backed tests **self-skip** when `FAMILIAR_TEST_DSN` is unset, so this run is green without a database — it just silently exercises less.

### 4. DB-backed integration tests (need `FAMILIAR_TEST_DSN`)

The `go test` command is **identical** to the hermetic run — the only difference is that `FAMILIAR_TEST_DSN` is exported, which un-skips the DB tests. `make test-integration` adds a `guard-dsn` precondition that hard-fails (exit 2) if the DSN is missing, instead of skipping into a meaningless green:

```bash
export FAMILIAR_TEST_DSN='postgres://familiar_test:familiar_test@localhost:5432/familiar_test?sslmode=disable'
cd /home/sixvolts/repos/familiar
make test-integration
```

Raw equivalent (with the env var exported):

```bash
cd familiar-gateway   && go test -count=1 ./...
cd ../familiar-workspace && go test -count=1 ./...
```

Point the DSN at a **throwaway** database or schema — these tests mutate it (the E2E layer `TRUNCATE`s `users CASCADE`). The Makefile's `guard-dsn` comment gives a shared-Postgres schema-isolation recipe via `search_path`.

### 5. Why `-count=1` is non-negotiable

`GOTEST := go test -count=1 ./...` is used on every Make target deliberately. Some tests read files **outside their Go module** — the prompt/tool-name guard walks `../../prompts` — and Go's test cache does **not** key on those files. So a prompt-only edit can replay a **stale PASS** with normal caching. `-count=1` disables the cache and forces a real run every time. The whole Go suite is ~2s cold, so caching saves nothing anyway.

### 6. Test counts (sanity check)

Verified on this box:

| Module | Top-level test funcs |
|--------|----------------------|
| `familiar-gateway` | **886** |
| `familiar-workspace` | **6** |

`go test ./...` prints `ok`/`?` per package but no grand total. Reproduce the counts directly:

```bash
cd familiar-gateway      && grep -rE '^func (Test|Example)' --include='*_test.go' . | wc -l   # -> 886
cd ../familiar-workspace && grep -rE '^func (Test|Example)' --include='*_test.go' . | wc -l   # -> 6
```

(For a per-run count including subtests, use `go test -count=1 -v ./... 2>&1 | grep -c '=== RUN'`, which will read higher.)

---

## Playwright E2E setup & the Chromium gotcha

The E2E suite lives in `/home/sixvolts/repos/familiar/tests/e2e` (Playwright + TypeScript). The `gateway.ts` fixture **builds the Go binaries itself and boots a real gateway + workspace per worker** — there is no separate "start the app" step. Playwright drives a Chromium browser against them. On this Ubuntu 26.04-shape box the one thing that does *not* work out of the box is Playwright's bundled browser, so read the gotcha below before running anything.

### Verified tool versions on this box

| Tool | This machine | Required-by |
| --- | --- | --- |
| Node / npm | `v22.22.1` / `9.2.0` | `@playwright/test` |
| Go | `go1.26.0` | `go 1.25.0` in both `familiar-gateway/go.mod` and `familiar-workspace/go.mod` |
| `@playwright/test` | `1.60.0` installed (spec `^1.49.0` in `tests/e2e/package.json`) | — |
| Chromium | snap `chromium 150.0.7871.128` | see gotcha |

Go must be on `PATH` for the E2E run even though this is a "Playwright" step: the fixture (`tests/e2e/fixtures/gateway.ts`) shells out to `go build` for `./cmd/gateway` and `./cmd/workspace` into `tests/e2e/.bin`. If Go is missing, specs fail at *build* time, not at browser launch.

### 1. Install the E2E node deps

```bash
cd /home/sixvolts/repos/familiar
make e2e-setup
```

`make e2e-setup` runs `npm install` in `tests/e2e`, then *best-effort* `npx playwright install --with-deps chromium` (the recipe is prefixed with `-` so its failure never breaks the target). That second step is expected to be a no-op on this host — see the gotcha.

### 2. THE CHROMIUM GOTCHA (read this)

Playwright ships **no browser build for Ubuntu 26.04**. `npx playwright install` cannot download a working Chromium here — it only writes link metadata. You can confirm the failure mode on this box:

```bash
ls -laR ~/.cache/ms-playwright
# → only a .links/ dir with a few tiny files; NO chromium-<rev>/ build dir
find ~/.cache/ms-playwright -maxdepth 1 -iname 'chromium*'   # → prints nothing
```

So you **must** point Playwright at a system Chromium. This box has one, via snap:

```bash
ls -l   /usr/bin/chromium-browser        # -rwxr-xr-x  ~2.4 KB  (a POSIX shell script, NOT a binary)
readlink -f /usr/bin/chromium-browser    # → /usr/bin/chromium-browser  (it is not a symlink)
/usr/bin/chromium-browser --version      # → Chromium 150.0.7871.128 snap ...
```

`/usr/bin/chromium-browser` is a Canonical snap **shim** whose last line is `exec /snap/bin/chromium "$@"` (and `/snap/bin/chromium` → `/usr/bin/snap`). The real browser is the `chromium` snap. Playwright launches it fine through the shim because the config only overrides `executablePath`:

```ts
// playwright.config.ts — both the chromium and mobile projects:
launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
    ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
    : {}
```

On a **truly fresh** machine that shim won't exist yet. Install the snap first (this creates `/snap/bin/chromium`; the `chromium-browser` transitional apt package provides the `/usr/bin/chromium-browser` shim and also pulls the snap):

```bash
sudo snap install chromium          # or: sudo apt install -y chromium-browser
command -v chromium-browser || command -v chromium   # confirm a path exists
```

> Host-specific note: this is the **snap** Chromium. On a distro that ships a real `.deb`/binary Chromium, point `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` at that binary instead (e.g. `/usr/bin/chromium` or `/usr/bin/google-chrome`). Snap confinement is fine here because the browser only talks to `localhost` — it never needs to read the repo — but if you hit a sandbox error, that confinement is the first thing to suspect.

### 3. Environment needed to run

Two variables. `FAMILIAR_TEST_DSN` is consumed by the fixture (`start()` throws without it) and must point at a **throwaway** DB/schema — the suite TRUNCATEs auth tables between specs (see the DSN / Postgres section). `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` is the browser override above.

```bash
export FAMILIAR_TEST_DSN='postgres://familiar_test:familiar_test@localhost:5432/familiar_test?sslmode=disable'
export PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser
```

`make test-e2e` *tries* to auto-detect Chromium (`CHROMIUM := $(shell command -v chromium-browser || command -v chromium)`), **but the auto-detect is defeated on this exact box** — see gotcha #2 below. Exporting `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` yourself is the reliable path and always wins.

### 4. Run the full suite

```bash
cd /home/sixvolts/repos/familiar
make test-e2e
# equivalently, straight through Playwright:
cd /home/sixvolts/repos/familiar/tests/e2e && npx playwright test
```

First run builds `gateway` + `workspace` into `tests/e2e/.bin` (a few seconds); **reruns are near-instant** thanks to Go's build cache. The workspace is served with `static_dir` pointed at the **live** `familiar-workspace/static` directory, so **front-end HTML/CSS/JS edits are picked up on the next run with no rebuild**. (Go source changes are recompiled automatically each run, cache-fast, since the fixture always re-runs `go build`.)

### 5. Run a single spec file

```bash
cd /home/sixvolts/repos/familiar/tests/e2e
npx playwright test flows/smoke.spec.ts          # one file
npx playwright test flows/wiki.spec.ts -g "rename" # one test by title

# through make — E2E_ARGS is forwarded verbatim to `npx playwright test`:
make test-e2e E2E_ARGS='flows/smoke.spec.ts'
```

Spec files live in `tests/e2e/flows/` (e.g. `smoke.spec.ts`, `auth.spec.ts`, `wiki.spec.ts`, `chat.spec.ts`, `mobile.spec.ts`). `mobile.spec.ts` runs only under the `mobile` (Pixel 7) project; everything else runs under `chromium`.

### 6. Model-gated specs (expected skips)

About **21 specs are model-gated**: they `test.skip(...)` when no live inference server answers a `/health` probe at `FAMILIAR_TEST_CHAT_MODEL_URL` (default `http://127.0.0.1:8090`) — `chat.spec.ts`, `tooluse.spec.ts`, `scheduled*.spec.ts`, `wiki-prompts.spec.ts`, `skillpacks*.spec.ts`, etc. A clean run with no model shows these as **skipped, not failed** — that is correct. To actually exercise them, stand up the inference server and set that variable per the **model-service section**.

---

## Make targets reference

All targets live in the repo-root `Makefile` (`/home/sixvolts/repos/familiar/Makefile`) and run from that directory. The suite is layered: plain `make test` needs nothing, `make test-integration` and `make test-e2e` both need a database DSN, and `make test-all` runs the lot.

Discover targets on any checkout with the self-documenting help target:

```bash
cd /home/sixvolts/repos/familiar
make help
```

`make help` greps the `## ` doc-comments out of the Makefile and prints them, so it stays in sync with whatever targets exist.

### Targets

| Target | What it does | Requires | Rough time |
|---|---|---|---|
| `help` | Prints the `## `-documented targets (self-documenting). | nothing | instant |
| `build` | Runs `build-gateway` + `build-workspace`. | Go toolchain (`go 1.25.0` per both `go.mod`; this box has `go1.26.0`) | seconds |
| `build-gateway` | `cd familiar-gateway && go build -o familiar-gateway ./cmd/gateway/` | Go | seconds |
| `build-workspace` | `cd familiar-workspace && go build -o familiar-workspace ./cmd/workspace/` | Go | seconds |
| `test` | Go tests for **both** modules, `go test -count=1 ./...` each. DB-gated tests silently self-skip here. | Go only — **no DB, no services** | **~2s** cold (per Makefile header) |
| `test-integration` | Same `go test -count=1 ./...` for both modules, but depends on `guard-dsn` first so DB-backed tests actually run instead of skipping. | `guard-dsn` → **`FAMILIAR_TEST_DSN` set**; a reachable Postgres (pgvector `vector` type must resolve) | seconds (Go suite + DB round-trips) |
| `e2e-setup` | `cd tests/e2e && npm install`, then a **best-effort** `npx playwright install --with-deps chromium` (the `-` prefix means its failure does not fail the target). | Node + npm (this box: `node v22.22.1`, `npm 9.2.0`); `@playwright/test ^1.49.0` per `tests/e2e/package.json` | 1–3 min first run (npm install + optional browser download) |
| `test-e2e` | Runs Playwright against a real gateway: `npx playwright test $(E2E_ARGS)`. Pre-flight checks (a) `tests/e2e/node_modules` exists — else exits 2 telling you to run `make e2e-setup`; (b) a usable Chromium exists. | `guard-dsn` → **`FAMILIAR_TEST_DSN`**; e2e deps installed; **a browser**: a Playwright cache (`~/.cache/ms-playwright`), a system Chromium (`CHROMIUM` var), or `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` | minutes (full browser suite) |
| `test-all` | `test-integration` + `test-e2e` — Go + DB-backed + E2E. | union of both above (`FAMILIAR_TEST_DSN`, Go, Node, browser) | minutes |
| `guard-dsn` | Not run directly; a prerequisite that **fails loudly (exit 2)** if `FAMILIAR_TEST_DSN` is empty, instead of letting DB tests skip into a hollow green run. | — | instant |

### Variables you can override

- **`FAMILIAR_TEST_DSN`** — required by every DB/E2E target via `guard-dsn`. Point it at a **throwaway** database or schema: the E2E suite `TRUNCATE`s the auth tables (`users CASCADE`) between specs. The Makefile's recommended isolation is a dedicated schema on a shared Postgres:
  ```bash
  psql -c 'CREATE SCHEMA IF NOT EXISTS e2e_test'
  export FAMILIAR_TEST_DSN='postgresql://user:pw@localhost:5432/db?sslmode=disable&options=-csearch_path%3De2e_test%2Cpublic'
  ```
  Keep `public` on the search_path so the pgvector `vector` type resolves.
- **`CHROMIUM`** — auto-detected as `chromium-browser` then `chromium` on `PATH`; only consulted when Playwright has no browser of its own. This box resolves it to `/usr/bin/chromium-browser`. Override to pin a binary.
- **`PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH`** — explicit browser path; wins over `CHROMIUM`.
- **`E2E_ARGS`** — passed straight through to `playwright test` (e.g. `make test-e2e E2E_ARGS='some.spec.ts --headed'`).

### Host-specific note (target machine = Ubuntu 26.04-ish)

`e2e-setup`'s `playwright install` step is deliberately non-fatal because **Playwright ships no browser build for Ubuntu 26.04**. On this box both a Playwright cache (`~/.cache/ms-playwright`) and a system Chromium (`/usr/bin/chromium-browser`) are present, so either path works — but on a genuinely fresh 26.04 machine expect the download to no-op and `test-e2e` to fall back to system Chromium. That makes the fresh-machine prerequisite `apt install chromium-browser` (or set `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH`); otherwise `test-e2e` exits 2 with "No Playwright browser and no system Chromium found." A machine that *is* a Playwright-supported distro can instead rely on the downloaded browser and skip the system Chromium install.

---

## Local inference model service (for the 21 model-gated E2E specs)

Six E2E spec files gate themselves on a live, tool-calling, OpenAI-compatible model server. If no server answers, they **skip** (green), not fail — so this is the one piece of the pipeline that silently does nothing when misconfigured. On this box the server is a `llama.cpp` build running a 122B GGUF on the AMD Strix Halo iGPU via Vulkan, wrapped in a systemd unit.

> **This step is hardware-specific.** The build, model, and flags below assume **AMD Strix Halo `gfx1151`, llama.cpp Vulkan/RADV backend (Mesa 26.x), ~122 GB unified memory** (the model alone loads ~76 GB into memory). A different GPU/box needs a *different* llama.cpp build and *different* flags — see the **Portable alternative** at the end. The specs don't care *what* serves the model, only that something OpenAI-compatible with `/health` and tool-calling answers on a port.

### The 6 gated spec files (21 tests total)

Each defines `MODEL_URL = process.env.FAMILIAR_TEST_CHAT_MODEL_URL || "http://127.0.0.1:8090"` and a `modelIsUp()` that does `GET ${MODEL_URL}/health` (2 s timeout, treats any `resp.ok` as up), then `test.skip(!modelIsUp())` in `beforeEach`:

| Spec (`tests/e2e/flows/…`) | tests |
|---|---|
| `chat.spec.ts` | 7 |
| `scheduled.spec.ts` | 8 |
| `tooluse.spec.ts` | 2 |
| `skillpacks.spec.ts` | 2 |
| `scheduled-wiki.spec.ts` | 1 |
| `wiki-prompts.spec.ts` | 1 |

### What runs on THIS machine (verified)

- **llama.cpp build:** `b10066` (commit `86a9c79f8`), Vulkan backend, x86_64. Lives at `$HOME/llama-b10066-vulkan/llama-b10066/` (server binary + `libggml-vulkan.so` etc.). Probe: `llama-server --version` → `version: 10066 (86a9c79f8)`.
- **Model GGUFs** in `$HOME/models/qwen-3.5-122B-opus-mtp/`:
  - `Qwen3.5-122B-A10B-Opus-Reasoning-Q4_K_XL.gguf` — **71 GiB** (main)
  - `mtp-opus-q4kxl-draft.gguf` — **3.2 GiB** (MTP draft for speculative decode)
- **Launch script** `$HOME/llama-b10066-vulkan/run-opus.sh` (6 slots × 64k ctx, Vulkan full-offload, `--jinja` for tool-calling, `--host 0.0.0.0 --port 8080`).
- **systemd unit** `/etc/systemd/system/familiar-mainframe-llama.service` (User `sixvolts`, `TimeoutStartSec=300` because the 76 GB load takes minutes).

### 1. Get the llama.cpp Vulkan build

Obtain the **`b10066` Vulkan release** of llama.cpp for `x86_64` (from the official `ggml-org/llama.cpp` GitHub releases — pick the `b10066` tag, Vulkan/`ubuntu-vulkan` asset) and unpack it so the layout matches:

```bash
mkdir -p "$HOME/llama-b10066-vulkan"
# unpack the release so this path exists and is executable:
#   $HOME/llama-b10066-vulkan/llama-b10066/llama-server
"$HOME/llama-b10066-vulkan/llama-b10066/llama-server" --version   # expect: version: 10066
```

Vulkan runtime prerequisites for `gfx1151` (RADV): a recent Mesa (26.x on this box) plus the Vulkan loader:

```bash
sudo apt update
sudo apt install -y mesa-vulkan-drivers vulkan-tools libvulkan1
vulkaninfo --summary | grep -i -E 'deviceName|driverName'   # should show Radeon / RADV gfx1151
```

> Any other GPU (NVIDIA/CUDA, Apple/Metal, CPU-only) means a **different** llama.cpp build entirely and different offload flags (`-ngl`, backend libs). Match the build to the hardware; don't reuse this Vulkan tree.

### 2. Place the model GGUFs

I can't verify a download source for these specific quantized files, so **place the GGUFs at these exact paths** (the launch script references them by name):

```bash
mkdir -p "$HOME/models/qwen-3.5-122B-opus-mtp"
# copy into place:
#   $HOME/models/qwen-3.5-122B-opus-mtp/Qwen3.5-122B-A10B-Opus-Reasoning-Q4_K_XL.gguf   (~71 GiB)
#   $HOME/models/qwen-3.5-122B-opus-mtp/mtp-opus-q4kxl-draft.gguf                        (~3.2 GiB)
ls -lh "$HOME/models/qwen-3.5-122B-opus-mtp/"
```

A smaller model is fine (see Portable alternative) — but with this build/flags you need ~76 GB of the ~122 GB unified memory free for the load, plus KV cache headroom.

### 3. Create the launch script

`$HOME/llama-b10066-vulkan/run-opus.sh` (verbatim from this machine — `chmod +x` it):

```bash
#!/usr/bin/env bash
# Qwen3.5-122B-A10B Opus-Reasoning, Q4_K_XL, + MTP draft for speculative decode.
# Stock llama.cpp b10066, Vulkan/RADV backend — recommended for gfx1151.
set -euo pipefail

BIN="$HOME/llama-b10066-vulkan/llama-b10066/llama-server"
MODEL_DIR="$HOME/models/qwen-3.5-122B-opus-mtp"
MAIN="$MODEL_DIR/Qwen3.5-122B-A10B-Opus-Reasoning-Q4_K_XL.gguf"
DRAFT="$MODEL_DIR/mtp-opus-q4kxl-draft.gguf"

CTX_PER_SLOT="${CTX_PER_SLOT:-65536}"   # 65536=64k, 49152=48k fallback if 64k won't fit
SLOTS="${SLOTS:-6}"                     # 6 x 64k fits in ~85G of 122G
TOTAL_CTX=$(( CTX_PER_SLOT * SLOTS ))

echo "[run-opus] $SLOTS slots x $((CTX_PER_SLOT/1024))k  (total ctx $TOTAL_CTX)"

exec "$BIN" \
  -m  "$MAIN" \
  -md "$DRAFT" \
  --spec-type draft-mtp --spec-draft-n-max 2 --spec-draft-n-min 1 \
  -ngl 99 -fa on --no-mmap \
  --parallel "$SLOTS" -c "$TOTAL_CTX" --no-kv-unified \
  -ctk q8_0 -ctv q8_0 -ctkd q8_0 -ctvd q8_0 \
  -t 12 -b 2048 -ub 512 \
  --jinja \
  --host 0.0.0.0 --port 8080 \
  --alias qwen3.5-122b-opus
```

Key flags: `-ngl 99` (all layers on GPU), `--no-mmap` (reads the full 76 GB into memory up front), `--jinja` (**required** — enables the chat template's tool-calling the 21 specs depend on), `--port 8080`. If 64k×6 doesn't fit, drop context: `CTX_PER_SLOT=49152 ./run-opus.sh`.

### 4. Install and enable the service

Create `/etc/systemd/system/familiar-mainframe-llama.service` (verbatim; change `User=` for a different account):

```ini
[Unit]
Description=Familiar temp inference — Qwen3.5-122B Opus (llama.cpp Vulkan, Strix Halo gfx1151)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=sixvolts
# run-opus.sh defaults to 6 slots x 64k; override per-slot ctx here if needed.
# Environment=CTX_PER_SLOT=49152
# Environment=SLOTS=6
ExecStart=/home/sixvolts/llama-b10066-vulkan/run-opus.sh
Restart=on-failure
RestartSec=5
# Model load reads 76GB; give it room before systemd calls it failed.
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp familiar-mainframe-llama.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now familiar-mainframe-llama.service
```

The server listens on **port 8080** — note this is **NOT** the specs' `8090` default, so you must override the URL when running them (next step).

### 5. Run the gated specs

Start the service (if not already), then **wait for `/health` to return `{"status":"ok"}`** — while the 76 GB model loads, `/health` returns **HTTP 503**, and `modelIsUp()` treats that as "down" and **skips** the tests instead of failing them. Don't run Playwright until you see `ok`:

```bash
sudo systemctl start familiar-mainframe-llama.service

# poll until loaded (503 while loading, 200 {"status":"ok"} when ready):
until curl -fsS -m 3 http://127.0.0.1:8080/health 2>/dev/null | grep -q '"status":"ok"'; do
  echo "waiting for model load…"; sleep 5
done
echo "model up"
```

Then run the E2E suite pointed at :8080 (needs `FAMILIAR_TEST_DSN` set per the DB section; e2e deps + Chromium installed per the E2E section):

```bash
cd /home/sixvolts/repos/familiar
# whole suite (gated specs now execute instead of skip):
FAMILIAR_TEST_CHAT_MODEL_URL=http://127.0.0.1:8080 make test-e2e

# or just the 6 model-gated files:
cd tests/e2e
FAMILIAR_TEST_CHAT_MODEL_URL=http://127.0.0.1:8080 \
  npx playwright test flows/chat.spec.ts flows/scheduled.spec.ts \
    flows/tooluse.spec.ts flows/skillpacks.spec.ts \
    flows/scheduled-wiki.spec.ts flows/wiki-prompts.spec.ts
```

Toolchain on this box (for reference): Node `v22.22.1`, Playwright `1.60.0` (`@playwright/test ^1.49.0`).

### Portable alternative (no Strix Halo / no big GPU)

The specs only require an **OpenAI-compatible server with a `/health` endpoint and working tool-calling**. On any other machine, run a smaller model on any port with **`--jinja`** (tool-calling is mandatory — the 21 tests exercise tool-use, skillpacks, and scheduled actions that call tools), then point the env var at it:

```bash
# e.g. a small tool-capable model on CPU or a modest GPU:
llama-server -m ./some-small-tool-capable.gguf --jinja --host 127.0.0.1 --port 9001
# (add your backend's offload flags: -ngl N for GPU, or omit for CPU)

FAMILIAR_TEST_CHAT_MODEL_URL=http://127.0.0.1:9001 make test-e2e
```

Any framework works (llama.cpp, vLLM, an Ollama OpenAI shim, etc.) as long as `/health` returns `ok` and the model actually emits tool calls — a model without reliable tool-calling will let the specs run but fail their assertions.

---

## Gotchas & troubleshooting

### Prerequisites & system packages

- The `go 1.25.0` directive in both go.mod files is a MINIMUM, not a pin — this box runs go1.26.0 and builds fine. Don't chase an exact 1.25.0; any Go >= 1.25 works. If you install a Go older than 1.25 with GOTOOLCHAIN=auto it will silently download the 1.25 toolchain on first build.
- Chromium on Ubuntu 26.04 is snap-only. The apt `chromium-browser` package (2:1snap1-0ubuntu4) is just a transitional wrapper: /usr/bin/chromium-browser is a shell script that refuses to run unless the chromium snap is installed and execs /snap/bin/chromium. There is no non-snap Chromium in the archive. `npx playwright install chromium` has no 26.04 build, so you MUST set PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser (which resolves to the snap).
- pgvector's apt package is major-locked: you need postgresql-18-pgvector for a PG 18 server. Installing the wrong major (or a PG 18 pgvector against a different server major) makes `CREATE EXTENSION vector` fail at gateway boot, which looks like a migration error, not a package error.
- The test pipeline needs only the `vector` extension. `uuid-ossp` appears in the legacy `init-db.sql` (docker-compose) bootstrap but is NOT used by the Go migration path, which uses core `gen_random_uuid()` — don't install `postgresql-contrib` on the pipeline's account.
- The snap Chromium is AppArmor-confined and can be blocked from launching when the working directory is outside $HOME (e.g. /tmp). Run the E2E suite from under your home directory to avoid opaque browser-launch failures.
- npm on this box is 9.2.0, which is older than the npm 10 that Node 22 normally bundles (npm was installed/pinned separately). Installing Node via NodeSource gives you npm 10 instead — harmless for this suite, just don't be alarmed by the version drift.

### Database setup (PostgreSQL + pgvector + test DB/schema)

- Migration failure at gateway boot is only logged, not fatal. main.go wraps db.Migrate as `log.Printf("[memory] warning: migrations failed: %v", ...)` and keeps booting. So a broken DB (missing pgvector, wrong search_path, non-superuser trying CREATE EXTENSION) produces a gateway that comes up healthy, then tests fail later with 42P01 'relation does not exist'. Grep the gateway.log (E2E writes one per instance under the temp dir) for 'migrations failed' when tables are missing.
- `CREATE EXTENSION vector` needs superuser — do it as `postgres`, not as `familiar_test`. pgvector is not a trusted extension, so the app role cannot create it; the gateway's `CREATE EXTENSION IF NOT EXISTS vector` only no-ops cleanly because you pre-created it in step 2.
- The migration timeout is 5s (`context.WithTimeout(ctx, 5*time.Second)` in main.go). The full migration set is large (30+ steps) but idempotent and cheap on a warm box; on a slow/cold first run it can time out and log the warning above. Re-run — the advisory lock (key 0x46414D494C494152) serializes concurrent boots so parallel E2E gateways queue instead of racing.
- Keep the DSN in URL form and single-quoted. The bare `user:pass@host` form fails `db.Open`, and an unquoted value lets bash background on the `&` before `options=`. The `options=-csearch_path%3De2e_test%2Cpublic` percent-encoding is load-bearing — `%3D`/`%2C` decode to `=`/`,` for libpq.
- Do not drop `public` from search_path 'to be tidy'. The `vector` type lives only in public; the first migration (memories_base) references it unqualified and will fail to resolve, cascading to every later migration that touches `memories`.
- The pgvector package major must match the server major (here both 18). A mismatched `postgresql-<N>-pgvector` installs a `.so` the running server can't load, so `CREATE EXTENSION vector` errors even though the package looks installed.
- init-db.sql (repo root) is the legacy docker-compose bootstrap (`facts` table, uuid-ossp, hnsw index) and is NOT what the Go migration path uses. Don't seed from it — the live schema comes from familiar-gateway/internal/db/migrate.go, which only needs the `vector` extension.

### Go: build & test (gateway + workspace)

- Two separate modules with no root go.mod / go.work: a repo-root `go build ./...` or `go test ./...` covers NEITHER. You must cd into familiar-gateway and familiar-workspace individually — every Make target does exactly this.
- -count=1 is mandatory, not cosmetic. The prompt/tool-name guard reads files under ../../prompts (outside the module), which Go's test cache does not track. Without -count=1 a prompt-only edit replays a stale PASS and hides real breakage.
- `make test` is green even with no database because the DB-backed tests self-skip when FAMILIAR_TEST_DSN is unset — it proves less than it looks. Use `make test-integration` (guard-dsn) to force them; it exits 2 rather than skip quietly.
- The raw `go test -count=1 ./...` command is identical for the hermetic and integration runs; the ONLY difference is whether FAMILIAR_TEST_DSN is exported. There is no build tag or separate command.
- go.mod pins a floor of go 1.25.0 for both modules; Ubuntu apt's Go may be older and will be rejected. Install 1.26.0 (this box) from the go.dev tarball rather than apt.
- FAMILIAR_TEST_DSN points at a database these tests MUTATE (E2E TRUNCATEs users CASCADE). Never aim it at anything you care about — use a throwaway DB or an isolated schema via search_path.

### Playwright E2E setup & the Chromium gotcha

- Playwright ships NO browser for Ubuntu 26.04: `npx playwright install` writes only `~/.cache/ms-playwright/.links/` metadata and no `chromium-<rev>/` build dir. You MUST use a system Chromium via PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH.
- The Makefile auto-detect is silently defeated on this box. `make test-e2e` only injects the system Chromium when `~/.cache/ms-playwright` is ABSENT (line 67: `... || echo $(CHROMIUM)`). Here that dir EXISTS (empty, leftover from the failed `playwright install`), so the auto-detect skips and Playwright tries to launch a bundled browser that isn't there. Fix: always `export PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser` yourself, or `rm -rf ~/.cache/ms-playwright` to let auto-detect fire.
- `/usr/bin/chromium-browser` is a ~2.4 KB POSIX shell shim (not a binary, not a symlink — `readlink -f` returns itself); it `exec`s `/snap/bin/chromium`. The real browser is the `chromium` snap (150.0.7871.128). On a truly fresh machine the path won't exist until you `snap install chromium`.
- FAMILIAR_TEST_DSN is mandatory (the fixture throws without it) and points at a database the suite MUTATES — it TRUNCATEs users CASCADE between specs. Use a throwaway DB/schema, never a real one.
- Go must be installed and on PATH for the 'Playwright' step: the fixture runs `go build` for both modules into tests/e2e/.bin. go.mod requires Go 1.25.0 (this box has 1.26.0). Missing Go fails the run at build time, which can look like a browser/harness error.
- ~21 specs skip (not fail) without a live inference server at FAMILIAR_TEST_CHAT_MODEL_URL (default http://127.0.0.1:8090). A green run with those skipped is expected; enabling them is covered in the model-service section.

### Make targets reference

- `make test` silently under-covers: the DB-gated tests self-skip when FAMILIAR_TEST_DSN is unset, so a green `make test` proves less than it looks. Use `test-integration` (which routes through guard-dsn) when you actually want DB coverage.
- Every Go test target hard-codes `-count=1` (no test cache) on purpose: some tests read prompt/tool-name files OUTSIDE their Go module (../../prompts) that the cache does not key on, so a prompt-only edit could otherwise replay a stale PASS.
- FAMILIAR_TEST_DSN must point at a database/schema you are willing to have mutated — the E2E suite TRUNCATEs auth tables (users CASCADE) between specs. Use a dedicated schema, and keep `public` on the search_path or the pgvector `vector` type fails to resolve.
- `test-e2e` will exit 2 (not skip) if `tests/e2e/node_modules` is missing — run `make e2e-setup` first — or if no browser is found on a fresh box.
- go.mod declares `go 1.25.0` for both modules, but this machine runs go1.26.0; a fresh machine needs Go >= 1.25.
- npm here is 9.2.0 with node v22.22.1 — an old npm on a new node; if you install a newer node on the fresh box its bundled npm will differ, which is usually fine but worth noting for reproducibility.

### Local inference model service (for the 21 model-gated E2E specs)

- The service listens on port 8080, but every gated spec defaults FAMILIAR_TEST_CHAT_MODEL_URL to http://127.0.0.1:8090. You MUST pass FAMILIAR_TEST_CHAT_MODEL_URL=http://127.0.0.1:8080 or all 21 tests silently skip.
- While the ~76 GB model loads, GET /health returns HTTP 503. modelIsUp() treats non-2xx as 'down' and calls test.skip() — so a not-yet-loaded model makes the tests pass-by-skipping, not fail. Always poll for {"status":"ok"} before running Playwright.
- Load is slow and memory-heavy: --no-mmap reads the full 76 GB into memory at startup (hence TimeoutStartSec=300). On a box with <~100 GB free unified/system memory this OOMs or thrashes; drop context with CTX_PER_SLOT=49152 or use a smaller model.
- The run-opus.sh header comment says '4 slots' but SLOTS actually defaults to 6 (stale comment) — trust the code, not the comment.
- This is an AMD Strix Halo gfx1151 + Vulkan/RADV build. On any other GPU the llama.cpp binary AND the offload flags (-ngl, backend .so files) are different — do not copy this build tree to a non-gfx1151 box.
- --jinja is load-bearing: it enables the chat template's tool-calling. Without it the specs run but fail, since they exercise tool-use / skillpacks / scheduled-action tools.
- I could not verify a download URL for the specific quantized GGUFs (Qwen3.5-122B Q4_K_XL + MTP draft). They must be placed by hand at $HOME/models/qwen-3.5-122B-opus-mtp/ with the exact filenames the script references.
