#!/usr/bin/env bash
set -euo pipefail

REPO="$HOME/repos/familiar-engine"
GATEWAY="$REPO/familiar-gateway"

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
git pull || fail "git pull failed"

# Build gateway (the engine is now in-process Go — no separate build)
step "Building gateway..."
export PATH="$PATH:/usr/local/go/bin"
cd "$GATEWAY"
go build -o familiar-gateway ./cmd/gateway/ || fail "gateway build failed"
step "Gateway built"

# Restart
step "Restarting familiar-gateway..."
sudo systemctl restart familiar-gateway
sleep 2

# Verify
step "Verifying services..."
GW_STATUS=$(systemctl is-active familiar-gateway)

if [[ "$GW_STATUS" == "active" ]]; then
    step "Gateway running"
    echo ""
    journalctl -u familiar-gateway --no-pager -n 5
else
    warn "Gateway: $GW_STATUS"
    fail "Service check failed — check logs with journalctl"
fi
