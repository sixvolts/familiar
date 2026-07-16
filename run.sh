#!/usr/bin/env bash
# run.sh — Start the full Familiar stack on one box.
#
# Starts: PostgreSQL → Sidecar llama-servers → Gateway (engine runs in-process)
#
# Usage:
#   ./run.sh                    # build + run all
#   ./run.sh --no-build         # run from existing binaries
#   ./run.sh --no-sidecar       # skip sidecar llama-server instances
#   ./run.sh --no-db            # skip PostgreSQL startup check
#   ./run.sh --slack            # run Slack adapter instead of CLI
#   ./run.sh --http           # run native HTTP adapter (POST /api/chat)
#   ./run.sh --verbose          # show routing metadata after responses

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
GATEWAY_DIR="$ROOT_DIR/familiar-gateway"

FAMILIAR_HOME="${FAMILIAR_HOME:-$HOME/.familiar}"
BUILD=true
RUN_SIDECAR=true
RUN_DB=true
GATEWAY_FLAGS=""

for arg in "$@"; do
    case "$arg" in
        --no-build)    BUILD=false ;;
        --no-sidecar)  RUN_SIDECAR=false ;;
        --no-db)       RUN_DB=false ;;
        --slack)       GATEWAY_FLAGS="$GATEWAY_FLAGS --slack" ;;
        --http)      GATEWAY_FLAGS="$GATEWAY_FLAGS --http" ;;
        --verbose)     GATEWAY_FLAGS="$GATEWAY_FLAGS --verbose" ;;
        *)             echo "Unknown flag: $arg"; exit 1 ;;
    esac
done

# Ensure runtime directories exist.
mkdir -p "$FAMILIAR_HOME/run"
mkdir -p "$FAMILIAR_HOME/data"

# ── Sidecar config ────────────────────────────────────────────
# The sidecar in v1 is a pair of llama-server processes:
#   - Embedder (nomic-embed-text-v1.5) on port 8100
#   - Router (Qwen3-8B) on port 8200
#
# On host-a these run as systemd services. On dev machines they
# can be started manually or via this script.

LLAMA_SERVER="${LLAMA_SERVER:-$HOME/llama.cpp/build/bin/llama-server}"
SIDECAR_MODELS="${SIDECAR_MODELS:-$HOME/models/sidecar}"

EMBEDDER_PORT=8100
ROUTER_PORT=8200
EMBEDDER_PID=""
ROUTER_PID=""
GATEWAY_PID=""

# ── Trap for clean shutdown ───────────────────────────────────

cleanup() {
    echo ""
    echo "==> Shutting down..."
    [[ -n "${GATEWAY_PID:-}" ]]  && kill "$GATEWAY_PID" 2>/dev/null || true
    [[ -n "${ROUTER_PID:-}" ]]   && kill "$ROUTER_PID" 2>/dev/null || true
    [[ -n "${EMBEDDER_PID:-}" ]] && kill "$EMBEDDER_PID" 2>/dev/null || true
    wait 2>/dev/null
    echo "==> Done."
}
trap cleanup EXIT INT TERM

# ── 1. Ensure PostgreSQL is running ──────────────────────────

if $RUN_DB; then
    echo "==> Checking PostgreSQL..."
    if command -v pg_isready >/dev/null 2>&1 && pg_isready -q 2>/dev/null; then
        echo "    Native PostgreSQL is running."
    elif command -v docker >/dev/null 2>&1; then
        echo "    Starting PostgreSQL via Docker..."
        docker compose -f "$ROOT_DIR/docker-compose.yml" up -d
        echo "    Waiting for Postgres to be ready..."
        TRIES=0
        until docker compose -f "$ROOT_DIR/docker-compose.yml" exec -T postgres pg_isready -U familiar >/dev/null 2>&1; do
            sleep 1
            TRIES=$((TRIES + 1))
            if [ $TRIES -ge 30 ]; then
                echo "    WARNING: Postgres not ready after 30s — continuing without DB."
                break
            fi
        done
        echo "    Postgres ready."
    else
        echo "    WARNING: No PostgreSQL available. Memory store will not persist."
    fi
fi

# ── 2. Build ─────────────────────────────────────────────────

if $BUILD; then
    echo "==> Building gateway (Go)..."
    (cd "$GATEWAY_DIR" && go build -o familiar-gateway ./cmd/gateway/)
fi

# ── 3. Start sidecar llama-servers (if available) ────────────

if $RUN_SIDECAR && [ -x "$LLAMA_SERVER" ]; then
    EMBEDDER_MODEL="$SIDECAR_MODELS/nomic-embed-text-v1.5.f16.gguf"
    ROUTER_MODEL="$SIDECAR_MODELS/Qwen3-8B-Q6_K.gguf"

    # Check if already running (systemd or otherwise).
    if curl -sf "http://127.0.0.1:$EMBEDDER_PORT/health" >/dev/null 2>&1; then
        echo "==> Embedder already running on port $EMBEDDER_PORT"
    elif [ -f "$EMBEDDER_MODEL" ]; then
        echo "==> Starting embedder llama-server on port $EMBEDDER_PORT..."
        HSA_OVERRIDE_GFX_VERSION=9.0.6 "$LLAMA_SERVER" \
            --host 127.0.0.1 --port "$EMBEDDER_PORT" \
            -m "$EMBEDDER_MODEL" \
            -ngl 99 -c 2048 --embeddings \
            > "$FAMILIAR_HOME/run/embedder.log" 2>&1 &
        EMBEDDER_PID=$!
        echo "    Embedder PID: $EMBEDDER_PID"

        # Wait for health.
        TRIES=0
        while ! curl -sf "http://127.0.0.1:$EMBEDDER_PORT/health" >/dev/null 2>&1 && [ $TRIES -lt 30 ]; do
            sleep 1
            TRIES=$((TRIES + 1))
        done
        if curl -sf "http://127.0.0.1:$EMBEDDER_PORT/health" >/dev/null 2>&1; then
            echo "    Embedder ready."
        else
            echo "    WARNING: Embedder not ready after 30s."
        fi
    else
        echo "==> Embedder model not found at $EMBEDDER_MODEL — skipping"
    fi

    if curl -sf "http://127.0.0.1:$ROUTER_PORT/health" >/dev/null 2>&1; then
        echo "==> Router already running on port $ROUTER_PORT"
    elif [ -f "$ROUTER_MODEL" ]; then
        echo "==> Starting router llama-server on port $ROUTER_PORT..."
        HSA_OVERRIDE_GFX_VERSION=9.0.6 "$LLAMA_SERVER" \
            --host 127.0.0.1 --port "$ROUTER_PORT" \
            -m "$ROUTER_MODEL" \
            -ngl 99 -c 8192 --jinja \
            > "$FAMILIAR_HOME/run/router.log" 2>&1 &
        ROUTER_PID=$!
        echo "    Router PID: $ROUTER_PID"

        TRIES=0
        while ! curl -sf "http://127.0.0.1:$ROUTER_PORT/health" >/dev/null 2>&1 && [ $TRIES -lt 60 ]; do
            sleep 1
            TRIES=$((TRIES + 1))
        done
        if curl -sf "http://127.0.0.1:$ROUTER_PORT/health" >/dev/null 2>&1; then
            echo "    Router ready."
        else
            echo "    WARNING: Router not ready after 60s."
        fi
    else
        echo "==> Router model not found at $ROUTER_MODEL — skipping"
    fi
else
    if ! $RUN_SIDECAR; then
        echo "==> Sidecar: skipped (--no-sidecar)"
    else
        echo "==> Sidecar: llama-server not found at $LLAMA_SERVER — skipping"
        echo "    (Set LLAMA_SERVER env var to override)"
    fi
fi

# ── 4. Start gateway (engine runs in-process) ────────────────

echo "==> Starting gateway..."
GATEWAY_BIN="$GATEWAY_DIR/familiar-gateway"
[ -x "$GATEWAY_BIN" ] || GATEWAY_BIN="$GATEWAY_DIR/gateway"

# shellcheck disable=SC2086
"$GATEWAY_BIN" $GATEWAY_FLAGS &
GATEWAY_PID=$!

echo ""
echo "==> Familiar is running. Press Ctrl-C to stop."
[ -n "$EMBEDDER_PID" ] && echo "    Embedder PID: $EMBEDDER_PID (port $EMBEDDER_PORT)"
[ -n "$ROUTER_PID" ]   && echo "    Router PID:   $ROUTER_PID (port $ROUTER_PORT)"
echo "    Gateway PID:  $GATEWAY_PID"
echo ""

# Wait for any child to exit.
WAIT_PIDS="$GATEWAY_PID"
[ -n "$EMBEDDER_PID" ] && WAIT_PIDS="$WAIT_PIDS $EMBEDDER_PID"
[ -n "$ROUTER_PID" ]   && WAIT_PIDS="$WAIT_PIDS $ROUTER_PID"
# shellcheck disable=SC2086
wait -n $WAIT_PIDS 2>/dev/null || true
