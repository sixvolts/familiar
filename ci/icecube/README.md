# icecube runner

The self-hosted tier runner. `icecube` is a Mac Studio (M3 Ultra, 96 GB)
that runs the Familiar test suite and publishes a verdict per commit.

## Design rule

**Deterministic execution, agentic adjudication.** `run-tiers.sh` runs
the tiers and writes structured artifacts; it never asks a model
anything. `adjudicate.sh` is a separate invocation that reads those
artifacts and judges whether a green result is genuinely green. Keeping
them apart is what lets a run stay reproducible without the agent.

The adjudicator has **veto power, never override power** — it can turn a
green run red, never a red run green. That is enforced in code, not by
prompt: `adjudicate.sh` reads the deterministic exit codes from the
manifest first and short-circuits before the model is consulted.

## Layout

| Path | What |
|---|---|
| `ci/icecube/run-tiers.sh` | Tiers 1–4. Deterministic. Writes artifacts. |
| `ci/icecube/manifest.py` | Collapses artifacts into `manifest.json`. |
| `ci/icecube/adjudicate.sh` | Tier 5. Reads artifacts, emits `verdict.json`. |
| `.github/workflows/e2e.yml` | One job, all tiers, sequential. |
| `/Users/Shared/icecube/` | venv, model, HF cache, logs (outside the repo). |

## Tiers

| Tier | What | Needs |
|---|---|---|
| 1 | `go test ./...` hermetic | Go |
| 2 | `go test ./...` with `FAMILIAR_TEST_DSN` | Postgres + pgvector |
| 3 | Playwright, modelless, incl. pixel baselines | node + chromium |
| 4 | Playwright against the MLX server | tier 3 + model |
| 5 | Adjudication | Claude Code |

Tier 3 points the specs at `127.0.0.1:1` — a closed port — so the
model-backed specs skip by their own `/health` gate. Tier 4 points at the
real server. Same specs, different backend; the difference in skip counts
is the evidence.

## The false green this exists to catch

Without `FAMILIAR_TEST_DSN`, ~160 DB-backed tests skip **and every
package still prints `ok`**. Measured on this box:

| | pass | skip | fail | exit |
|---|---|---|---|---|
| no DSN | 922 | 161 | 0 | **0** |
| DSN set | 1082 | 1 | 0 | 0 |

Both runs are green. One of them tested 160 fewer things. That delta is
recorded in the manifest as `derived.db_tests_activated_by_dsn`, and it
is the first thing the adjudicator checks.

## Running locally

```sh
export FAMILIAR_TEST_DSN="postgresql://familiar_test:familiar_test@localhost:5432/familiar_e2e?sslmode=disable"
export FAMILIAR_TEST_CHAT_MODEL_URL="http://127.0.0.1:8081"
ci/icecube/run-tiers.sh --artifacts ./artifacts
ci/icecube/adjudicate.sh --artifacts ./artifacts        # advisory
```

`--tiers 1,2` runs a subset. The script takes an exclusive lock
(`mkdir`, since macOS has no `flock`) because Postgres and the MLX
server are shared — parallel runs corrupt both.

## Machine specifics

- **launchd, not systemd.** `com.familiar.mlx-server.plist` keeps the
  MLX server up with `KeepAlive`.
- **`services:` / `container:` do not work here.** They are Linux-only.
  Postgres is a persistent launchd-managed local instance, so each run
  inherits whatever the last one left behind — survivable only because
  `32346f9` made the tests truncate their shared schemas.
- **bash is 3.2.** No associative arrays, no `flock`, no `timeout(1)`.
- The MLX server binds **127.0.0.1 only**. It has no authentication.

## Known deviations

- **Tier 4 serves `gemma-4-31b`, production serves a different variant.**
  Kept deliberately per operator decision; `config.example.toml`'s
  `gemma-4-26b-a4b` entries are stale and will be corrected separately.
- **The chat template is not production's.** The MLX repo's template
  renders string-valued `tool_calls[].function.arguments` non-fatally
  where Google's canonical template (what production serves from the QAT
  GGUF with `--jinja`) calls `raise_exception`. Since
  `internal/llm/openai.go:49` declares `Arguments string`, every tool
  call takes exactly that divergent branch. Tier 4 is therefore more
  forgiving than production on the tool-calling path. Serving with
  `--chat-template` pointed at the canonical template closes the gap.
- **Tier 4 runs `workers=1`.** The model batches fine; the suite does
  not, because workers share one database.
- **Pixel baselines are macOS-specific** and use a fallback DOM where the
  app does not expose its markdown renderer on `window`.
