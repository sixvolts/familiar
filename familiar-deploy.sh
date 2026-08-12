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

# Test BEFORE building or restarting. Until this point the running services
# still hold the previous binary, so bailing here leaves production untouched —
# a bad commit on main never reaches the service.
#
# `make test` is the hermetic suite: no DSN, so it CANNOT touch a database.
# That matters here — the DB-backed tests mutate whatever FAMILIAR_TEST_DSN
# points at (they TRUNCATE), so running them against the production DSN would
# destroy live data. They are opt-in below and only against a DSN you have
# explicitly designated as throwaway.
step "Running hermetic test suite..."
cd "$REPO"
make test || fail "tests failed — NOT deploying (services still on the previous build)"
step "Hermetic tests passed"

# Opt-in DB-backed coverage. Set FAMILIAR_DEPLOY_TEST_DSN to a THROWAWAY
# database/schema — never the production one. Unset = skipped with a notice, so
# the gap is visible rather than silent.
if [[ -n "${FAMILIAR_DEPLOY_TEST_DSN:-}" ]]; then
    step "Running DB-backed tests against FAMILIAR_DEPLOY_TEST_DSN..."
    FAMILIAR_TEST_DSN="$FAMILIAR_DEPLOY_TEST_DSN" make test-integration \
        || fail "DB-backed tests failed — NOT deploying"
    step "DB-backed tests passed"
else
    warn "FAMILIAR_DEPLOY_TEST_DSN unset — DB-backed tests skipped (~161 tests)."
    warn "Set it to a THROWAWAY database to cover them; never the production DSN."
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
