#!/bin/bash
# run-tiers.sh — deterministic tier runner for the icecube self-hosted runner.
#
# DESIGN RULE (see the build spec §1): this script is the deterministic
# half. It runs the tiers and writes structured artifacts. It never asks
# a model anything. Adjudication (tier 5) is a separate invocation that
# READS these artifacts — keep it that way, so a run stays reproducible
# without the agent.
#
# Everything the adjudicator needs to spot a false green must end up in
# the artifacts directory, above all the run manifest: without a record
# of whether FAMILIAR_TEST_DSN was actually set, a hollow-skip run and a
# real run are indistinguishable after the fact — both print "ok".
#
# Usage: ci/icecube/run-tiers.sh [--tiers 1,2,3,4] [--artifacts DIR]
#
# Exit code is the worst tier's exit code, so the workflow gate stays
# honest even though every tier runs.

set -uo pipefail
# NOTE: deliberately NOT `set -e`. Every tier must run even when an
# earlier one fails, or a tier-1 break hides everything downstream and
# the manifest lands half-empty.

# ── Configuration ───────────────────────────────────────────────────

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ARTIFACTS="${ARTIFACTS:-$REPO_ROOT/artifacts}"
TIERS="1,2,3,4"
MLX_URL="${FAMILIAR_TEST_CHAT_MODEL_URL:-http://127.0.0.1:8081}"
LOCK_DIR="${LOCK_DIR:-/tmp/icecube-tiers.lock}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --tiers)     TIERS="$2"; shift 2 ;;
        --artifacts) ARTIFACTS="$2"; shift 2 ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

wants() { [[ ",$TIERS," == *",$1,"* ]]; }

mkdir -p "$ARTIFACTS"

# ── Mutual exclusion ────────────────────────────────────────────────
# macOS has no flock(1). `mkdir` is atomic on every filesystem we care
# about, so it is the portable lock. The trap releases it even on a
# failed tier; a stale lock after a hard kill is a manual rm.

if ! mkdir "$LOCK_DIR" 2>/dev/null; then
    echo "==> another tier run holds $LOCK_DIR — refusing to run concurrently" >&2
    echo "    (Postgres and the MLX server are shared; parallel runs corrupt both)" >&2
    exit 75   # EX_TEMPFAIL
fi
cleanup() { rmdir "$LOCK_DIR" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# ── Provenance ──────────────────────────────────────────────────────

GIT_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
GIT_BRANCH="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
if [[ -n "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ]]; then
    GIT_DIRTY=true
else
    GIT_DIRTY=false
fi

# Whether the DSN is set is the single most important fact in the
# manifest — see the spec's hollow-skip check. Record the fact, never
# the value: the DSN carries a password.
if [[ -n "${FAMILIAR_TEST_DSN:-}" ]]; then DSN_SET=true; else DSN_SET=false; fi

# ── MLX health ──────────────────────────────────────────────────────

MLX_HEALTHY=false
MLX_MODEL_ID="null"
if curl -sf -m 5 "$MLX_URL/health" >/dev/null 2>&1; then
    MLX_HEALTHY=true
    MLX_MODEL_ID="$(curl -sf -m 5 "$MLX_URL/v1/models" 2>/dev/null \
        | /usr/bin/python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["data"][0]["id"])' 2>/dev/null \
        || echo unknown)"
fi

echo "==> icecube tier run"
echo "    sha=$GIT_SHA dirty=$GIT_DIRTY dsn_set=$DSN_SET mlx=$MLX_HEALTHY ($MLX_MODEL_ID)"

# NOT `declare -A`: macOS ships bash 3.2, which has no associative
# arrays. Tier keys are the integers 1-4, so a plain indexed array is
# both sufficient and portable. (`declare -A` silently degrades to an
# indexed array here and "works" by accident — don't rely on that.)
EXIT_CODES=()

# ── Tier 1 / 2: Go ──────────────────────────────────────────────────
# Same command twice; the ONLY difference is FAMILIAR_TEST_DSN. That is
# the point — the delta between the two skip counts is the evidence
# that the DB-backed tests actually ran.

run_go_tier() {
    local tier="$1" with_dsn="$2"
    echo "==> tier $tier: go test (dsn=$with_dsn)"
    local out="$ARTIFACTS/tier${tier}.json"
    # MUST match the Makefile's GOTEST (Makefile:29 — `go test -count=1 ./...`)
    # and its module list (test-integration runs BOTH modules).
    #
    #   -count=1 is not optional. Some tests read files OUTSIDE their module
    #   (the shipped prompts/ guards); Go's test cache does not key on those,
    #   so a prompt-only change can replay a stale PASS. Running bare
    #   `go test` here reintroduced exactly the false green this runner
    #   exists to catch.
    #
    #   familiar-workspace is not optional either — skipping it silently
    #   dropped a whole module from tiers 1 and 2.
    #
    # This does not just call `make test-integration` because that emits no
    # -json, and tier 5 needs per-test outcomes and skip reasons. The flags
    # therefore have to be kept in sync with the Makefile by hand.
    : > "$out"
    local code=0 m rc
    for m in familiar-gateway familiar-workspace; do
        (
            cd "$REPO_ROOT/$m" || exit 1
            if [[ "$with_dsn" == "false" ]]; then
                env -u FAMILIAR_TEST_DSN go test -count=1 -json ./...
            else
                go test -count=1 -json ./...
            fi
        ) >> "$out" 2>> "$ARTIFACTS/tier${tier}.err"
        rc=$?
        (( rc != 0 )) && code=$rc
    done
    EXIT_CODES[$tier]=$code
    echo "    exit=$code -> $(basename "$out")"
}

wants 1 && run_go_tier 1 false
if wants 2; then
    if [[ "$DSN_SET" != "true" ]]; then
        # Refuse rather than silently produce a run that looks identical
        # to tier 1. This is the false green the whole design targets.
        echo "==> tier 2: SKIPPED — FAMILIAR_TEST_DSN unset. Not running a" >&2
        echo "    DB tier without a DB; it would print ok and prove nothing." >&2
        EXIT_CODES[2]=78   # EX_CONFIG
    else
        run_go_tier 2 true
    fi
fi

# ── Tier 3 / 4: Playwright ──────────────────────────────────────────
# Tier 3 must NOT see a live model: point the specs at a dead port so
# the model-backed ones skip by their own gate. Tier 4 points at the
# real server. Same specs, different backend.

run_pw_tier() {
    local tier="$1" model_url="$2" workers="$3"
    echo "==> tier $tier: playwright (model=$model_url workers=$workers)"
    # Record the URL THIS tier used, not the runner's configured one.
    # Without this the manifest says mlx_healthy=true while tier 3's
    # skips say "no inference server at 127.0.0.1:1", which reads as a
    # hollow skip to anything checking the two against each other.
    echo "$model_url" > "$ARTIFACTS/tier${tier}.modelurl"
    (
        cd "$REPO_ROOT/tests/e2e" || exit 1
        PLAYWRIGHT_JSON_OUTPUT_NAME="$ARTIFACTS/tier${tier}.json" \
        FAMILIAR_TEST_CHAT_MODEL_URL="$model_url" \
        npx playwright test \
            --workers="$workers" \
            --reporter=json,line \
            --output="$ARTIFACTS/tier${tier}-results"
    ) > "$ARTIFACTS/tier${tier}.raw" 2>&1
    local code=$?
    EXIT_CODES[$tier]=$code
    echo "    exit=$code -> tier${tier}.json"
}

# 127.0.0.1:1 is closed by definition — the specs' /health probe fails
# fast and they skip, which is exactly the modelless condition.
wants 3 && run_pw_tier 3 "http://127.0.0.1:1" 1

if wants 4; then
    if [[ "$MLX_HEALTHY" != "true" ]]; then
        echo "==> tier 4: SKIPPED — no MLX server at $MLX_URL." >&2
        echo "    Running it anyway would silently repeat tier 3." >&2
        EXIT_CODES[4]=78
    else
        # workers=1, DESPITE the model tolerating concurrency.
        #
        # mlx_lm does batch: 2.44x aggregate throughput at 4 concurrent
        # requests, zero errors, +0.1 GB RSS. So the MODEL is not the
        # constraint. The SUITE is. The Playwright fixture is
        # worker-scoped, and every worker boots its own gateway+workspace
        # stack against the SAME Postgres database. At workers=4 that
        # cross-talk fails specs which are not even model-backed
        # (folders, notes-sync, skillpacks-ui all pass at workers=1 and
        # fail at 4).
        #
        # Raising this needs per-worker database isolation first — a
        # schema or database per worker index. Until then, model
        # concurrency buys nothing here.
        run_pw_tier 4 "$MLX_URL" 1
    fi
fi

# ── Silent migration failures ───────────────────────────────────────
# db.Migrate failing at gateway boot is only LOGGED, never fatal:
# cmd/gateway/main.go:355 wraps it in log.Printf("warning: migrations
# failed: %v") and carries on. The gateway then answers /api/health with
# "ok" on a database that has no tables, and specs fail later with an
# opaque 42P01 "relation does not exist" — or, worse, specs that never
# touch the missing tables pass, and the run is green on a broken DB.
#
# The migration also runs under a 5s timeout (main.go:353), so a cold or
# loaded box can trip this without anything else being wrong.
#
# The E2E fixture writes one gateway.log per booted instance into a
# mkdtemp under $TMPDIR. Scan them.
scan_migration_failures() {
    local out="$ARTIFACTS/migration-failures.txt"
    local dirs=("${TMPDIR:-/tmp}"familiar-e2e-*)
    : > "$out"
    [[ -e "${dirs[0]}" ]] || { echo "0" > "$ARTIFACTS/migration-failures.count"; return; }
    grep -h "migrations failed" "${TMPDIR:-/tmp}"familiar-e2e-*/gateway.log 2>/dev/null \
        | sort | uniq -c | sort -rn > "$out" || true
    # NOT `grep -c . || echo 0`: on an empty file grep prints 0 AND exits 1,
    # so the fallback appends a SECOND zero and n becomes "0\n0" — which is
    # not "0", so the warning fires on a clean scan and the count fails to
    # parse. wc -l cannot do that.
    local n
    n=$(wc -l < "$out" | tr -d ' []')
    echo "$n" > "$ARTIFACTS/migration-failures.count"
    if [[ "$n" != "0" ]]; then
        echo "==> WARNING: $n distinct 'migrations failed' line(s) in gateway logs" >&2
        head -3 "$out" >&2
        echo "    A gateway with failed migrations still reports healthy." >&2
    fi
}
scan_migration_failures

# ── Manifest ────────────────────────────────────────────────────────
# Written last so it can carry every tier's outcome, and written even
# when tiers failed — a run that produced no manifest is indistinguish-
# able from a run that never happened, which the adjudicator must catch.

echo "==> writing manifest"
EXIT_JSON="{"
for t in "${!EXIT_CODES[@]}"; do EXIT_JSON+="\"$t\":${EXIT_CODES[$t]},"; done
EXIT_JSON="${EXIT_JSON%,}}"
[[ "$EXIT_JSON" == "{}" ]] || true

ARTIFACTS="$ARTIFACTS" \
GIT_SHA="$GIT_SHA" GIT_BRANCH="$GIT_BRANCH" GIT_DIRTY="$GIT_DIRTY" \
DSN_SET="$DSN_SET" MLX_HEALTHY="$MLX_HEALTHY" MLX_MODEL_ID="$MLX_MODEL_ID" \
MLX_URL="$MLX_URL" TIERS="$TIERS" EXIT_JSON="$EXIT_JSON" \
/usr/bin/python3 "$REPO_ROOT/ci/icecube/manifest.py"

WORST=0
for t in "${!EXIT_CODES[@]}"; do
    c=${EXIT_CODES[$t]}
    (( c > WORST )) && WORST=$c
done
echo "==> done. worst tier exit=$WORST"
exit "$WORST"
