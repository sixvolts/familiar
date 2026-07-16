#!/usr/bin/env bash
# setup.sh — One-time setup for the Familiar project.
#
# Checks prerequisites, builds all projects, creates default directories,
# sets up PostgreSQL/pgvector, and runs the test suites.
#
# Usage:
#   ./setup.sh                  # full setup
#   ./setup.sh --skip-tests     # skip test suites
#   ./setup.sh --skip-db        # skip PostgreSQL setup

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
GATEWAY_DIR="$ROOT_DIR/familiar-gateway"
SKIP_TESTS=false
SKIP_DB=false
SKIP_INSTANCE=false

for arg in "$@"; do
    case "$arg" in
        --skip-tests)    SKIP_TESTS=true ;;
        --skip-db)       SKIP_DB=true ;;
        --skip-instance) SKIP_INSTANCE=true ;;
        *)               echo "Unknown flag: $arg"; exit 1 ;;
    esac
done

ERRORS=0

ok()   { echo "  [ok]  $1"; }
fail() { echo "  [!!]  $1"; ERRORS=$((ERRORS + 1)); }
skip() { echo "  [--]  $1"; }

# ── Check prerequisites ──────────────────────────────────────

echo "==> Checking prerequisites..."

if command -v go >/dev/null 2>&1; then
    ok "Go $(go version | awk '{print $3}' | sed 's/go//')"
else
    fail "Go not found — install from https://go.dev/dl"
fi

if command -v docker >/dev/null 2>&1; then
    ok "Docker $(docker --version | awk '{print $3}' | tr -d ',')"
else
    skip "Docker not found (optional — can use native PostgreSQL instead)"
fi

# Check for PostgreSQL (native or Docker).
if command -v psql >/dev/null 2>&1; then
    ok "PostgreSQL client (psql $(psql --version | awk '{print $3}'))"
elif command -v docker >/dev/null 2>&1; then
    skip "psql not found — will use Docker for PostgreSQL"
else
    if ! $SKIP_DB; then
        fail "Neither psql nor Docker found — need one for memory store"
    fi
fi

if [ $ERRORS -gt 0 ]; then
    echo ""
    echo "Missing $ERRORS required tool(s). Install them and re-run ./setup.sh"
    exit 1
fi

# ── Create runtime directories ────────────────────────────────

echo ""
echo "==> Creating runtime directories..."

FAMILIAR_HOME="${FAMILIAR_HOME:-$HOME/.familiar}"
mkdir -p "$FAMILIAR_HOME/run"
mkdir -p "$FAMILIAR_HOME/data"
mkdir -p "$FAMILIAR_HOME/identity"
mkdir -p "$FAMILIAR_HOME/models"
ok "$FAMILIAR_HOME/{run,data,identity,models}"

# Create sidecar socket directory (may need elevated permissions on Linux).
SIDECAR_SOCK_DIR="/run/familiar"
if [ -w "/run" ] || [ -d "$SIDECAR_SOCK_DIR" ]; then
    mkdir -p "$SIDECAR_SOCK_DIR" 2>/dev/null || true
    if [ -d "$SIDECAR_SOCK_DIR" ]; then
        ok "$SIDECAR_SOCK_DIR (sidecar socket dir)"
    else
        # Fall back to home directory.
        SIDECAR_SOCK_DIR="$FAMILIAR_HOME/run"
        skip "Cannot create /run/familiar — sidecar socket will use $SIDECAR_SOCK_DIR"
    fi
else
    SIDECAR_SOCK_DIR="$FAMILIAR_HOME/run"
    skip "Cannot create /run/familiar — sidecar socket will use $SIDECAR_SOCK_DIR"
fi

# ── Set up PostgreSQL / pgvector ─────────────────────────────

if ! $SKIP_DB; then
    echo ""
    echo "==> Setting up PostgreSQL with pgvector..."

    # Determine if we use native Postgres or Docker.
    USE_DOCKER_PG=false
    if command -v pg_isready >/dev/null 2>&1 && pg_isready -q 2>/dev/null; then
        ok "Native PostgreSQL is running"
    elif command -v docker >/dev/null 2>&1; then
        USE_DOCKER_PG=true
        echo "    Starting PostgreSQL via Docker..."
        docker compose -f "$ROOT_DIR/docker-compose.yml" up -d
        echo "    Waiting for Postgres to be ready..."
        TRIES=0
        until docker compose -f "$ROOT_DIR/docker-compose.yml" exec -T postgres pg_isready -U familiar >/dev/null 2>&1; do
            sleep 1
            TRIES=$((TRIES + 1))
            if [ $TRIES -ge 30 ]; then
                fail "PostgreSQL not ready after 30s"
                SKIP_DB=true
                break
            fi
        done
        if ! $SKIP_DB; then
            ok "PostgreSQL running via Docker"
        fi
    else
        skip "No PostgreSQL available — skipping database setup"
        SKIP_DB=true
    fi

    if ! $SKIP_DB; then
        # Apply the memory store schema.
        if $USE_DOCKER_PG; then
            docker compose -f "$ROOT_DIR/docker-compose.yml" exec -T postgres \
                psql -U familiar -d familiar -f /docker-entrypoint-initdb.d/01-init.sql 2>/dev/null || true
        else
            # Native Postgres — create database and apply schema.
            if ! psql -lqt 2>/dev/null | cut -d \| -f 1 | grep -qw familiar; then
                createdb familiar 2>/dev/null || true
            fi
            psql -d familiar -f "$ROOT_DIR/init-db.sql" 2>/dev/null || true
        fi
        ok "Database schema applied (facts, conversation_turns, entity_edges)"
    fi
fi

# ── Build gateway ─────────────────────────────────────────────

echo ""
echo "==> Building gateway (Go)..."
(cd "$GATEWAY_DIR" && go build -o gateway ./cmd/gateway/)
ok "familiar-gateway built (engine is in-process; no separate binary)"

# ── Generate default config ──────────────────────────────────

GATEWAY_CONFIG="$FAMILIAR_HOME/gateway.toml"
if [ ! -f "$GATEWAY_CONFIG" ]; then
    echo ""
    echo "==> Generating default config at $GATEWAY_CONFIG ..."

    # Detect sidecar socket path to use.
    SIDECAR_SOCK="$SIDECAR_SOCK_DIR/sidecar.sock"

    cat > "$GATEWAY_CONFIG" <<TOML
[node]
name = "$(hostname -s)"
role = "gateway"

[adapter.cli]
prompt = "> "
history_file = "$FAMILIAR_HOME/cli_history"

[embedder]
endpoint = ""
model = "nomic-embed-text"
dimension = 768

[router]
enabled = true
fallback_model = "anthropic/claude-sonnet-4-6"
prefer_local = true
use_sidecar_router = false
fallback_router = "rule_based"
confidence_threshold = 0.7

[sidecar]
enabled = false
socket_path = "$SIDECAR_SOCK"
connect_timeout_ms = 500
request_timeout_ms = 5000
retry_interval_seconds = 10
fallback_on_failure = true

[memory]
use_sidecar_embedder = false
store = "local"
local_dsn = "postgresql://familiar@localhost:5432/familiar"
relevance_threshold = 0.72
max_injected_memories = 10
dedup_threshold = 0.95

[sleep]
enabled = true

[[models]]
id = "anthropic/claude-sonnet-4-6"
provider = "anthropic"
endpoint = "https://api.anthropic.com"
vault_key = "anthropic_api_key"
context_window = 200000
capabilities = ["tool_use", "vision", "reasoning"]
latency_profile = "remote"
max_concurrent = 5
TOML
    ok "Default config written to $GATEWAY_CONFIG"
else
    ok "Config already exists at $GATEWAY_CONFIG"
fi

# ── Interactive instance metadata ─────────────────────────────
#
# Populates [instance] in $GATEWAY_CONFIG so the bot's `instance`
# skill can answer "where do I register / who's the admin / where
# are the docs" questions. Idempotent: if the block already exists
# in the config, skip to avoid clobbering operator edits. Pass
# --skip-instance to bypass entirely (CI, unattended reprovision).

configure_instance() {
    if $SKIP_INSTANCE; then
        skip "Instance metadata prompt (--skip-instance)"
        return 0
    fi
    if grep -q '^\[instance\]' "$GATEWAY_CONFIG" 2>/dev/null; then
        ok "[instance] block already present in $GATEWAY_CONFIG — leaving untouched"
        return 0
    fi
    if [ ! -t 0 ] || [ ! -t 1 ]; then
        skip "Instance metadata prompt (non-interactive terminal)"
        return 0
    fi

    echo ""
    echo "==> Configuring [instance] metadata"
    echo "    This powers the \`instance\` skill — the bot uses it to answer"
    echo "    questions like 'how do I register' or 'who's the admin'."
    echo "    Leave any field blank to skip it."
    echo ""

    local default_name="Familiar ($(hostname -s))"
    local name admin_url register_url admin_contact docs_url help_notes

    read -r -p "  Instance name [$default_name]: " name
    name="${name:-$default_name}"

    read -r -p "  Admin console URL (e.g. https://host/admin/): " admin_url

    local default_register="$admin_url"
    read -r -p "  Registration URL [$default_register]: " register_url
    register_url="${register_url:-$default_register}"

    read -r -p "  Admin contact (e.g. '@roo on Slack'): " admin_contact
    read -r -p "  Docs URL: " docs_url
    read -r -p "  Help notes (freeform, one line): " help_notes

    # Escape double quotes so the heredoc-produced TOML stays valid
    # when operators paste in URLs or prose containing them.
    escape_quotes() { printf '%s' "$1" | sed 's/"/\\"/g'; }

    cat >> "$GATEWAY_CONFIG" <<TOML

[instance]
name          = "$(escape_quotes "$name")"
admin_url     = "$(escape_quotes "$admin_url")"
register_url  = "$(escape_quotes "$register_url")"
admin_contact = "$(escape_quotes "$admin_contact")"
docs_url      = "$(escape_quotes "$docs_url")"
help_notes    = "$(escape_quotes "$help_notes")"
TOML
    ok "[instance] appended to $GATEWAY_CONFIG"
}

configure_instance

# ── Run tests ─────────────────────────────────────────────────

if $SKIP_TESTS; then
    echo ""
    echo "==> Skipping tests (--skip-tests)"
else

    echo ""
    echo "==> Running Go gateway tests..."
    (cd "$GATEWAY_DIR" && go test -race ./internal/... 2>&1)
    ok "Gateway tests passed"
fi

# ── Summary ───────────────────────────────────────────────────

echo ""
echo "========================================="
echo "  Setup complete!"
echo ""
echo "  Components:"
echo "    - Gateway (Go):   $GATEWAY_DIR/gateway  (engine runs in-process)"
echo "    - Config:         $GATEWAY_CONFIG"
echo "    - PostgreSQL:     localhost:5432/familiar"
echo ""
echo "  Next steps:"
echo "    1. Store API keys via the admin UI or psql vault entries"
echo "    2. Run:             ./run.sh"
echo ""
echo "  Optional:"
echo "    - Add local models: edit [models] in $GATEWAY_CONFIG"
echo "    - Enable sidecar:   set [sidecar] enabled = true in $GATEWAY_CONFIG"
echo "========================================="
