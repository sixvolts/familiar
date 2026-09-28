#!/usr/bin/env bash
set -euo pipefail

REPO="${FAMILIAR_REPO:-$HOME/repos/familiar}"
GATEWAY="$REPO/familiar-gateway"
WORKSPACE="$REPO/familiar-workspace"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

step() { echo -e "${GREEN}▸ $1${NC}"; }
warn() { echo -e "${YELLOW}⚠ $1${NC}"; }
fail() { echo -e "${RED}✗ $1${NC}"; exit 1; }

# Fetch only. The live checkout is what the workspace serves static/ from,
# so it must not move until the new commit has passed the gate and built.
# (It used to be reset first: a refused deploy still shipped the new JS
# against the old gateway, and a passing one ran them mismatched for the
# whole CI wait.)
step "Fetching origin..."
cd "$REPO"
PREV_HEAD=$(git rev-parse HEAD)
git fetch origin || fail "git fetch failed"
NEW_HEAD=$(git rev-parse origin/main)

export PATH="$PATH:/usr/local/go/bin"

# Staging worktree at NEW_HEAD: the hermetic fallback tests and both builds
# happen here, never in the live tree.
STAGE=$(mktemp -d "${TMPDIR:-/tmp}/familiar-deploy.XXXXXX")
cleanup_stage() {
    git -C "$REPO" worktree remove --force "$STAGE/tree" >/dev/null 2>&1 || true
    rm -rf "$STAGE"
}
trap cleanup_stage EXIT
git worktree add --detach "$STAGE/tree" "$NEW_HEAD" >/dev/null 2>&1 \
    || fail "could not stage ${NEW_HEAD:0:8} in a worktree"

# ── Gate: verify this commit before it reaches the service ──────────────
#
# This runs ON THE PRODUCTION BOX, which shapes the whole design: there is no
# throwaway database here, and the DB-backed tests TRUNCATE whatever
# FAMILIAR_TEST_DSN points at. So we never run them here — that is CI's job,
# where an ephemeral pgvector service exists purely to be mutated.
#
# Preferred gate: ASK CI. Every push to main runs the full suite (including the
# ~161 DB-backed tests) against that disposable database. Consulting the result
# for this exact SHA is both stronger coverage and cheaper than anything we can
# do locally — no prod CPU spent, no data at risk.
#
# Fallback: if CI can't be consulted (gh missing/unauthenticated, no run for
# this SHA, or the run is still going), fall back to the hermetic suite. It
# runs with FAMILIAR_TEST_DSN removed from its environment, so it cannot
# touch a database — safe here by construction, just narrower than CI.
#
# Either way the gate runs BEFORE the live tree moves, the build, and the
# restart, so on failure the services and static files stay on the previous
# build.
ci_conclusion_for() {
    # Echoes: success | failure | pending | unknown
    local sha="$1"
    command -v gh >/dev/null 2>&1 || { echo unknown; return; }
    gh auth status >/dev/null 2>&1 || { echo unknown; return; }
    local out
    # Scope to the ONE workflow that constitutes the gate. Without this the
    # query returns every run for the SHA, so an unrelated workflow (the
    # screenshot job, say) failing would hard-abort a deploy, and an unrelated
    # workflow passing could satisfy the gate while the real suite never ran.
    out=$(gh run list --commit "$sha" --workflow "$GATE_WORKFLOW" \
            --json status,conclusion --limit 20 2>/dev/null) || { echo unknown; return; }
    [[ -n "$out" && "$out" != "[]" ]] || { echo unknown; return; }
    # Any run still going → pending. Any concluded run that isn't success or
    # skipped → failure. Otherwise success.
    if grep -q '"status":"\(queued\|in_progress\|waiting\|requested\|pending\)"' <<<"$out"; then
        echo pending; return
    fi
    if grep -q '"conclusion":"\(failure\|timed_out\|cancelled\|startup_failure\|action_required\)"' <<<"$out"; then
        echo failure; return
    fi
    grep -q '"conclusion":"success"' <<<"$out" && echo success || echo unknown
}

# Same question, asked of the CONTENT rather than the commit id.
#
# GitHub's merge button always mints a new commit, so a merge SHA never has a
# run of its own even when the branch it came from was exhaustively tested. If
# main did not move while that branch was in flight, the merge commit's TREE is
# byte-identical to the branch tip's tree — same source, different identity —
# and re-running the suite proves nothing we do not already know.
#
# If main DID move, the merge commit is the branch plus whatever main gained,
# and the trees differ. That combination genuinely has not been tested (it is
# the case the pull_request trigger used to cover), so no verdict is reused and
# the normal wait applies. The tree hash draws that line exactly.
#
# Limitation worth stating: identical trees mean identical SOURCE, not an
# identical environment. A test that depends on wall-clock time, prior database
# state, or a model's output could pass on one and fail on the other. That risk
# already exists in trusting the branch run at all; this does not add a new
# kind, only a new place it applies.
ci_conclusion_for_tree() {
    # Echoes the 40-hex sha of a commit with an identical tree that already
    # passed, or nothing. Never failure or pending — a non-success on a
    # DIFFERENT commit says nothing about this one, so the caller falls through
    # to the SHA-based ladder.
    #
    # Returns the sha on stdout rather than setting a global: this is called in
    # a command substitution, which is a subshell, so an assignment here would
    # never reach the caller.
    local sha="$1"
    command -v gh >/dev/null 2>&1 || return 1
    local want
    want=$(git rev-parse "${sha}^{tree}" 2>/dev/null) || return 1

    local heads
    heads=$(gh run list --workflow "$GATE_WORKFLOW" --status success \
              --json headSha --limit 30 2>/dev/null \
              | grep -o '"headSha":"[0-9a-f]*"' | cut -d'"' -f4) || return 1

    local h t
    for h in $heads; do
        # A run can reference a commit this clone has never fetched.
        t=$(git rev-parse "${h}^{tree}" 2>/dev/null) || continue
        if [[ "$t" == "$want" ]]; then
            echo "$h"; return
        fi
    done
    return 1
}

GATE_WORKFLOW="${FAMILIAR_DEPLOY_CI_WORKFLOW:-e2e.yml}"  # the workflow that gates
CI_WAIT="${FAMILIAR_DEPLOY_CI_WAIT:-600}"   # seconds to wait on an in-flight run
GATE_PASSED=false
step "Checking CI for ${NEW_HEAD:0:8}..."
DEADLINE=$(( $(date +%s) + CI_WAIT ))
TREE_MATCH_SHA=""
while :; do
    CI_STATE=$(ci_conclusion_for "$NEW_HEAD")
    # No verdict for this exact SHA yet? Before waiting out the timeout, check
    # whether some other commit with IDENTICAL content already passed. That is
    # the ordinary merge case: the branch was green, main had not moved, and the
    # merge commit is the same tree under a new id.
    if [[ "$CI_STATE" != "success" && "$CI_STATE" != "failure" ]]; then
        TREE_MATCH_SHA=$(ci_conclusion_for_tree "$NEW_HEAD" || true)
        if [[ "$TREE_MATCH_SHA" =~ ^[0-9a-f]{40}$ ]]; then
            step "CI green for the tree of ${NEW_HEAD:0:8} — identical content already passed as ${TREE_MATCH_SHA:0:8}"
            GATE_PASSED=true
            break
        fi
    fi
    case "$CI_STATE" in
        success)
            step "CI green for ${NEW_HEAD:0:8} (full suite incl. DB-backed tests)"
            GATE_PASSED=true
            break
            ;;
        failure)
            # Break-glass. Without it, the first red run on main leaves no way
            # to deploy anything — including the fix for whatever turned it
            # red. Loud on purpose: this is the one path that ships code CI
            # has actively rejected, so it must be a deliberate act, never a
            # default. Everything else (pending timeout, unknown) already
            # degrades to the local suite on its own.
            if [[ -n "${FAMILIAR_DEPLOY_ALLOW_FAILED_CI:-}" ]]; then
                warn "CI FAILED for ${NEW_HEAD:0:8} — OVERRIDDEN via FAMILIAR_DEPLOY_ALLOW_FAILED_CI"
                warn "Deploying code that CI rejected. Running the local suite as a floor."
                break
            fi
            fail "CI FAILED for ${NEW_HEAD:0:8} — NOT deploying (services still on the previous build). Override: FAMILIAR_DEPLOY_ALLOW_FAILED_CI=1 ./familiar-deploy.sh"
            ;;
        pending)
            if (( $(date +%s) >= DEADLINE )); then
                warn "CI still running after ${CI_WAIT}s — falling back to local tests"
                break
            fi
            echo "  CI in progress; waiting…"
            sleep 15
            ;;
        *)
            warn "CI status unavailable (gh missing/unauthenticated, or no run for this SHA)"
            break
            ;;
    esac
done

if [[ "$GATE_PASSED" != true ]]; then
    # Hermetic only. `env -u` matters: the DB-backed tests switch on
    # whenever FAMILIAR_TEST_DSN is set, and TRUNCATE tables in whatever
    # database it names — on this box, possibly production.
    warn "Falling back to the hermetic suite — narrower than CI (~161 DB-backed tests skip)"
    step "Running hermetic test suite on ${NEW_HEAD:0:8}..."
    (cd "$STAGE/tree" && env -u FAMILIAR_TEST_DSN make test) \
        || fail "tests failed — NOT deploying (services still on the previous build)"
    step "Hermetic tests passed"
fi

# Build both binaries in the staging tree (the engine is in-process Go —
# no separate build). A build failure leaves everything live untouched.
step "Building gateway..."
(cd "$STAGE/tree/familiar-gateway" && go build -o "$STAGE/familiar-gateway" ./cmd/gateway/) \
    || fail "gateway build failed — NOT deploying (services still on the previous build)"
step "Gateway built"

# Rebuild (and restart) the workspace only when its Go sources changed.
# Most frontend work touches familiar-workspace/static/, which is served
# off disk and needs neither a rebuild nor a restart, and restarting on
# every deploy would drop live chat streams (the workspace proxies
# /api/chat) for no reason.
WS_RESTART=false
WS_REASON=""
if [[ ! -x "$WORKSPACE/familiar-workspace" ]]; then
    WS_RESTART=true
    WS_REASON="binary missing"
elif ! git -C "$REPO" diff --quiet "$PREV_HEAD" "$NEW_HEAD" -- \
        familiar-workspace/cmd familiar-workspace/internal \
        familiar-workspace/go.mod familiar-workspace/go.sum; then
    WS_RESTART=true
    WS_REASON="Go sources changed"
fi
if [[ "$WS_RESTART" == true ]]; then
    step "Building workspace ($WS_REASON)..."
    (cd "$STAGE/tree/familiar-workspace" && go build -o "$STAGE/familiar-workspace" ./cmd/workspace/) \
        || fail "workspace build failed — NOT deploying (services still on the previous build)"
    step "Workspace built"
else
    step "Workspace Go sources unchanged — skipping build and restart"
fi

# Swap in: keep the running binaries for rollback, move the live tree to
# NEW_HEAD, install the staged binaries, restart. Static files and
# binaries change within seconds of each other rather than a CI wait
# apart.
[[ -f "$GATEWAY/familiar-gateway" ]] && cp -p "$GATEWAY/familiar-gateway" "$STAGE/prev-gateway"
[[ "$WS_RESTART" == true && -f "$WORKSPACE/familiar-workspace" ]] \
    && cp -p "$WORKSPACE/familiar-workspace" "$STAGE/prev-workspace"
git -C "$REPO" reset --hard "$NEW_HEAD" >/dev/null || fail "git reset to ${NEW_HEAD:0:8} failed"
install -m 755 "$STAGE/familiar-gateway" "$GATEWAY/familiar-gateway"
[[ "$WS_RESTART" == true ]] && install -m 755 "$STAGE/familiar-workspace" "$WORKSPACE/familiar-workspace"

# rollback restores the previous tree and binaries and restarts them, then
# fails the deploy. Used when the new build doesn't come up healthy.
rollback() {
    warn "Rolling back to ${PREV_HEAD:0:8}..."
    git -C "$REPO" reset --hard "$PREV_HEAD" >/dev/null || warn "tree rollback failed"
    [[ -f "$STAGE/prev-gateway" ]] && install -m 755 "$STAGE/prev-gateway" "$GATEWAY/familiar-gateway"
    [[ -f "$STAGE/prev-workspace" ]] && install -m 755 "$STAGE/prev-workspace" "$WORKSPACE/familiar-workspace"
    sudo systemctl restart familiar-gateway || true
    [[ "$WS_RESTART" == true ]] && { sudo systemctl restart familiar-workspace || true; }
    fail "$1 — rolled back to ${PREV_HEAD:0:8}"
}

# Restart
step "Restarting familiar-gateway..."
sudo systemctl restart familiar-gateway
if [[ "$WS_RESTART" == true ]]; then
    step "Restarting familiar-workspace..."
    sudo systemctl restart familiar-workspace
fi
sleep 2

# Verify
step "Verifying services..."
GW_STATUS=$(systemctl is-active familiar-gateway || true)
WS_STATUS=$(systemctl is-active familiar-workspace || true)

if [[ "$GW_STATUS" == "active" && "$WS_STATUS" == "active" ]]; then
    step "Gateway running"
    step "Workspace running"

    # systemctl is-active only proves systemd started the process — it stays
    # "active" for a gateway that is failing every request. Probe the HTTP
    # surface too, with a few retries to cover a slow boot.
    GW_HEALTH_URL="${FAMILIAR_HEALTH_URL:-http://127.0.0.1:8000/api/health}"
    HEALTH_OK=false
    for _ in 1 2 3 4 5; do
        if curl -fsS -m 5 "$GW_HEALTH_URL" >/dev/null 2>&1; then
            HEALTH_OK=true
            break
        fi
        sleep 2
    done
    if [[ "$HEALTH_OK" == true ]]; then
        step "Gateway answering on $GW_HEALTH_URL"
    else
        journalctl -u familiar-gateway --no-pager -n 20
        rollback "Gateway process is active but not answering $GW_HEALTH_URL"
    fi

    echo ""
    journalctl -u familiar-gateway --no-pager -n 5
else
    [[ "$GW_STATUS" == "active" ]] || warn "Gateway: $GW_STATUS"
    [[ "$WS_STATUS" == "active" ]] || warn "Workspace: $WS_STATUS"
    journalctl -u familiar-gateway --no-pager -n 20 || true
    rollback "Service check failed"
fi
