#!/usr/bin/env bash
set -euo pipefail

# Slack App Setup Script for Familiar Gateway
# Guides through creating a Slack app, configuring permissions,
# enabling Socket Mode, and writing the gateway config.

CONFIG_DIR="${HOME}/.familiar"
CONFIG_FILE="${CONFIG_DIR}/gateway.toml"

BOLD='\033[1m'
DIM='\033[2m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
RED='\033[0;31m'
RESET='\033[0m'

banner() {
    echo ""
    echo -e "${CYAN}${BOLD}═══════════════════════════════════════════════════${RESET}"
    echo -e "${CYAN}${BOLD}  Familiar Gateway — Slack Setup${RESET}"
    echo -e "${CYAN}${BOLD}═══════════════════════════════════════════════════${RESET}"
    echo ""
}

step() {
    echo ""
    echo -e "${GREEN}${BOLD}[$1]${RESET} $2"
    echo -e "${DIM}────────────────────────────────────────────${RESET}"
}

info() {
    echo -e "  ${DIM}$1${RESET}"
}

warn() {
    echo -e "  ${YELLOW}⚠  $1${RESET}"
}

err() {
    echo -e "  ${RED}✗  $1${RESET}"
}

ok() {
    echo -e "  ${GREEN}✓  $1${RESET}"
}

prompt_continue() {
    echo ""
    read -rp "  Press Enter when ready to continue... "
}

prompt_value() {
    local label="$1"
    local var_name="$2"
    local secret="${3:-false}"
    echo ""
    if [[ "$secret" == "true" ]]; then
        read -rsp "  ${label}: " value
        echo ""
    else
        read -rp "  ${label}: " value
    fi
    eval "$var_name=\"\$value\""
}

validate_token() {
    local token="$1"
    local prefix="$2"
    local label="$3"
    if [[ ! "$token" =~ ^${prefix}- ]]; then
        err "${label} should start with '${prefix}-'. Got: ${token:0:10}..."
        return 1
    fi
    return 0
}

# ─── Main ────────────────────────────────────────────────────────────────────

banner

echo "This script will guide you through setting up a Slack App for"
echo "Familiar Gateway. You'll need access to a Slack workspace where"
echo "you can create apps."
echo ""
echo -e "Config will be written to: ${BOLD}${CONFIG_FILE}${RESET}"

# ─── Step 1: Create Slack App ────────────────────────────────────────────────

step "1/6" "Create a Slack App"

echo "  Open the Slack App creation page:"
echo ""
echo -e "  ${BOLD}https://api.slack.com/apps?new_app=1${RESET}"
echo ""
echo "  Select ${BOLD}\"From scratch\"${RESET}, then:"
echo "    • App Name: ${BOLD}Familiar${RESET} (or whatever you prefer)"
echo "    • Workspace: Select your workspace"
echo "    • Click ${BOLD}\"Create App\"${RESET}"

prompt_continue

# ─── Step 2: Enable Socket Mode ─────────────────────────────────────────────

step "2/6" "Enable Socket Mode"

echo "  In your app's settings page:"
echo "    1. Go to ${BOLD}\"Socket Mode\"${RESET} in the left sidebar"
echo "    2. Toggle ${BOLD}\"Enable Socket Mode\"${RESET} to ON"
echo "    3. You'll be prompted to create an App-Level Token"
echo "       • Token Name: ${BOLD}familiar-socket${RESET}"
echo "       • Scope: ${BOLD}connections:write${RESET} (should be pre-selected)"
echo "       • Click ${BOLD}\"Generate\"${RESET}"
echo "    4. Copy the token (starts with ${BOLD}xapp-${RESET})"

prompt_value "App-Level Token (xapp-...)" APP_TOKEN true

if ! validate_token "$APP_TOKEN" "xapp" "App-Level Token"; then
    warn "Token format looks wrong, but continuing anyway."
fi

ok "App token saved"

# ─── Step 3: Configure Event Subscriptions ───────────────────────────────────

step "3/6" "Configure Event Subscriptions"

echo "  In your app's settings:"
echo "    1. Go to ${BOLD}\"Event Subscriptions\"${RESET} in the left sidebar"
echo "    2. Toggle ${BOLD}\"Enable Events\"${RESET} to ON"
echo "    3. Under ${BOLD}\"Subscribe to bot events\"${RESET}, add:"
echo ""
echo -e "       ${BOLD}• message.channels${RESET}  — messages in public channels"
echo -e "       ${BOLD}• message.groups${RESET}    — messages in private channels"
echo -e "       ${BOLD}• message.im${RESET}        — direct messages to the bot"
echo -e "       ${BOLD}• app_mention${RESET}       — when someone @mentions the bot"
echo ""
echo "    4. Click ${BOLD}\"Save Changes\"${RESET}"

prompt_continue
ok "Event subscriptions configured"

# ─── Step 4: Configure OAuth Scopes ─────────────────────────────────────────

step "4/6" "Configure Bot Token Scopes"

echo "  In your app's settings:"
echo "    1. Go to ${BOLD}\"OAuth & Permissions\"${RESET} in the left sidebar"
echo "    2. Under ${BOLD}\"Scopes\" → \"Bot Token Scopes\"${RESET}, add:"
echo ""
echo -e "       ${BOLD}• app_mentions:read${RESET}   — read @mention events"
echo -e "       ${BOLD}• channels:history${RESET}    — read messages in public channels"
echo -e "       ${BOLD}• channels:read${RESET}       — list channels"
echo -e "       ${BOLD}• chat:write${RESET}          — send messages"
echo -e "       ${BOLD}• groups:history${RESET}      — read messages in private channels"
echo -e "       ${BOLD}• groups:read${RESET}         — list private channels"
echo -e "       ${BOLD}• im:history${RESET}          — read direct messages"
echo -e "       ${BOLD}• im:read${RESET}             — list DM conversations"
echo -e "       ${BOLD}• im:write${RESET}            — open DM conversations"
echo ""
echo "    3. Scroll up and click ${BOLD}\"Install to Workspace\"${RESET}"
echo "    4. Review permissions and click ${BOLD}\"Allow\"${RESET}"
echo "    5. Copy the ${BOLD}Bot User OAuth Token${RESET} (starts with ${BOLD}xoxb-${RESET})"

prompt_value "Bot User OAuth Token (xoxb-...)" BOT_TOKEN true

if ! validate_token "$BOT_TOKEN" "xoxb" "Bot Token"; then
    warn "Token format looks wrong, but continuing anyway."
fi

ok "Bot token saved"

# ─── Step 5: Channel Restrictions ────────────────────────────────────────────

step "5/6" "Channel Restrictions (Optional)"

echo "  You can restrict the bot to specific channels."
echo "  Leave blank to allow all channels (the bot still only"
echo "  responds when @mentioned in channels, or in DMs)."
echo ""
echo "  Enter channel IDs separated by commas, or leave blank:"
echo -e "  ${DIM}(Find channel IDs: right-click channel → View channel details → scroll to bottom)${RESET}"

prompt_value "Channel IDs (comma-separated, or blank for all)" CHANNELS

# ─── Step 6: Write Config ───────────────────────────────────────────────────

step "6/6" "Write Configuration"

mkdir -p "$CONFIG_DIR"

# Build the TOML snippet for the Slack adapter.
SLACK_TOML="[adapter.slack]
bot_token = \"\${SLACK_BOT_TOKEN}\"
app_token = \"\${SLACK_APP_TOKEN}\""

if [[ -n "$CHANNELS" ]]; then
    # Convert comma-separated to TOML array.
    IFS=',' read -ra CHANNEL_ARR <<< "$CHANNELS"
    CHANNEL_LIST=""
    for ch in "${CHANNEL_ARR[@]}"; do
        ch=$(echo "$ch" | xargs) # trim whitespace
        if [[ -n "$ch" ]]; then
            [[ -n "$CHANNEL_LIST" ]] && CHANNEL_LIST+=", "
            CHANNEL_LIST+="\"${ch}\""
        fi
    done
    SLACK_TOML+=$'\n'"channels = [${CHANNEL_LIST}]"
fi

# Check if gateway.toml already exists and has a [adapter.slack] section.
if [[ -f "$CONFIG_FILE" ]]; then
    if grep -q '^\[adapter\.slack\]' "$CONFIG_FILE"; then
        warn "Existing [adapter.slack] section found in ${CONFIG_FILE}"
        echo ""
        read -rp "  Overwrite it? (y/N): " overwrite
        if [[ "$overwrite" != "y" && "$overwrite" != "Y" ]]; then
            echo ""
            echo "  Skipping config write. Here's the config to add manually:"
            echo ""
            echo -e "${DIM}${SLACK_TOML}${RESET}"
            echo ""
        else
            # Remove existing [adapter.slack] section and everything until the next section or EOF.
            tmpfile=$(mktemp)
            awk '
                /^\[adapter\.slack\]/ { skip=1; next }
                /^\[/ && skip { skip=0 }
                !skip { print }
            ' "$CONFIG_FILE" > "$tmpfile"
            # Append new section.
            echo "" >> "$tmpfile"
            echo "$SLACK_TOML" >> "$tmpfile"
            mv "$tmpfile" "$CONFIG_FILE"
            ok "Updated ${CONFIG_FILE}"
        fi
    else
        # Append to existing config.
        echo "" >> "$CONFIG_FILE"
        echo "$SLACK_TOML" >> "$CONFIG_FILE"
        ok "Appended Slack config to ${CONFIG_FILE}"
    fi
else
    # Create a new config with defaults + slack section.
    cat > "$CONFIG_FILE" <<TOML
[engine]
socket_path = "~/.familiar/run/engine.sock"

[adapter.cli]
prompt = "> "
history_file = "~/.familiar/cli_history"

${SLACK_TOML}

[embedder]
endpoint = ""
model = "nomic-embed-text"
dimension = 768

[router]
enabled = true
fallback_model = "anthropic/claude-sonnet-4-6"
prefer_local = true

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
    ok "Created ${CONFIG_FILE}"
fi

# Write tokens to env file.
ENV_FILE="${CONFIG_DIR}/slack.env"
cat > "$ENV_FILE" <<ENV
# Familiar Gateway — Slack tokens
# Source this file or export these variables before running the gateway.
export SLACK_BOT_TOKEN="${BOT_TOKEN}"
export SLACK_APP_TOKEN="${APP_TOKEN}"
ENV
chmod 600 "$ENV_FILE"
ok "Tokens written to ${ENV_FILE} (mode 600)"

# ─── Done ────────────────────────────────────────────────────────────────────

echo ""
echo -e "${GREEN}${BOLD}═══════════════════════════════════════════════════${RESET}"
echo -e "${GREEN}${BOLD}  Setup Complete!${RESET}"
echo -e "${GREEN}${BOLD}═══════════════════════════════════════════════════${RESET}"
echo ""
echo "  To run the gateway with Slack:"
echo ""
echo -e "    ${BOLD}source ${ENV_FILE}${RESET}"
echo -e "    ${BOLD}gateway --slack${RESET}"
echo ""
echo "  Or in one line:"
echo ""
echo -e "    ${BOLD}source ${ENV_FILE} && gateway --slack${RESET}"
echo ""
echo -e "  ${DIM}Don't forget to invite the bot to channels: /invite @Familiar${RESET}"
echo ""
