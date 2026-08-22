#!/usr/bin/env bash
set -euo pipefail

REPO="$HOME/repos/familiar"
GATEWAY="$REPO/familiar-gateway"
WORKSPACE="$REPO/familiar-workspace"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

step() { echo -e "${GREEN}▸ $1${NC}"; }
warn() { echo -e "${YELLOW}⚠ $1${NC}"; }
fail() { echo -e "${RED}✗ $1${NC}"; exit 1; }

# Pull
step "Pulling latest from origin..."
cd "$REPO"
PREV_HEAD=$(git rev-parse HEAD)
git fetch origin || fail "git fetch failed"
git reset --hard origin/main || fail "git reset to origin/main failed"
NEW_HEAD=$(git rev-parse HEAD)

export PATH="$PATH:/usr/local/go/bin"

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
# takes no DSN, so it cannot touch a database — it is safe here by construction,
# just narrower than CI.
#
# Either way the gate runs BEFORE the build and restart, so the services keep
# serving the previous binary if it fails.
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
    # Hermetic only: no DSN is passed, so this cannot reach any database.
    warn "Falling back to the hermetic suite — narrower than CI (~161 DB-backed tests skip)"
    step "Running hermetic test suite..."
    cd "$REPO"
    make test || fail "tests failed — NOT deploying (services still on the previous build)"
    step "Hermetic tests passed"
fi

# Build gateway (the engine is now in-process Go — no separate build)
step "Building gateway..."
cd "$GATEWAY"
go build -o familiar-gateway ./cmd/gateway/ || fail "gateway build failed"
step "Gateway built"

# Build the workspace only when its Go sources actually moved.
#
# Most frontend work touches familiar-workspace/static/, which is served
# off disk and needs neither a rebuild nor a restart — so restarting on
# every deploy would drop live chat streams (the workspace proxies
# /api/chat) for no reason. Comparing the binary hash would not help
# either: go stamps VCS info into it, so it changes on every commit.
# Compare mtimes instead: rebuild when any Go source is newer than
# the built binary. This catches pull-driven changes, locally committed
# ones (where reset --hard is a no-op so a commit-range diff sees
# nothing), and hand edits alike.
WS_RESTART=false
WS_REASON=""
cd "$WORKSPACE"
if [[ ! -x familiar-workspace ]]; then
    WS_RESTART=true
    WS_REASON="binary missing"
elif [[ -n "$(find cmd internal go.mod go.sum -newer familiar-workspace 2>/dev/null | head -1)" ]]; then
    WS_RESTART=true
    WS_REASON="Go sources newer than binary"
fi

if [[ "$WS_RESTART" == true ]]; then
    step "Building workspace ($WS_REASON)..."
    go build -o familiar-workspace ./cmd/workspace/ || fail "workspace build failed"
    step "Workspace built"
else
    step "Workspace Go sources unchanged — skipping build and restart"
fi

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
        fail "Gateway process is active but not answering $GW_HEALTH_URL"
    fi

    echo ""
    journalctl -u familiar-gateway --no-pager -n 5
else
    [[ "$GW_STATUS" == "active" ]] || warn "Gateway: $GW_STATUS"
    [[ "$WS_STATUS" == "active" ]] || warn "Workspace: $WS_STATUS"
    fail "Service check failed — check logs with journalctl"
fi
