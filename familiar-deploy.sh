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

# Build gateway (the engine is now in-process Go — no separate build)
step "Building gateway..."
export PATH="$PATH:/usr/local/go/bin"
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
# Compare the sources instead. If the range cannot be diffed (a
# force-pushed history can orphan PREV_HEAD), fall back to rebuilding.
WS_RESTART=false
WS_REASON=""
cd "$WORKSPACE"
if [[ ! -x familiar-workspace ]]; then
    WS_RESTART=true
    WS_REASON="binary missing"
elif ! git diff --quiet "$PREV_HEAD" "$NEW_HEAD" -- cmd internal go.mod go.sum 2>/dev/null; then
    WS_RESTART=true
    WS_REASON="Go sources changed"
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
    echo ""
    journalctl -u familiar-gateway --no-pager -n 5
else
    [[ "$GW_STATUS" == "active" ]] || warn "Gateway: $GW_STATUS"
    [[ "$WS_STATUS" == "active" ]] || warn "Workspace: $WS_STATUS"
    fail "Service check failed — check logs with journalctl"
fi
