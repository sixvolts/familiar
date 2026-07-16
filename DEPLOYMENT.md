# Familiar — Deployment & New-Instance Setup

This guide gets a **fresh Familiar instance** running on a new machine, and
covers **updating** an existing one. It is written to be handed to a fresh
operator (human or a fresh Claude) with no prior context.

It reflects the **current** codebase.

---

## 1. What you're deploying

Three first-party processes plus Postgres and one or more model servers:

```
Browser ──HTTPS──▶ familiar-workspace (Go)          ← the ONLY public entry point
                     • serves the web UI (static files, UA-sniffs mobile vs desktop)
                     • reverse-proxies /console/api, /api, /v1, /events, /p  ──▶ gateway
                          │
                          ▼
                   familiar-gateway (Go, :8000 loopback)
                     • admin console + auth (WebAuthn passkeys)
                     • chat pipeline, memory, scheduled actions, skills
                     • engine runs IN-PROCESS (Go) — there is NO separate engine binary
                          │
            ┌─────────────┼───────────────┬──────────────────┐
            ▼             ▼               ▼                  ▼
     PostgreSQL      chat model      sidecar model        embedder
     + pgvector      (llama-server   (llama-server,       (llama-server
     (:5432)          or remote      optional)            --embeddings,
                      OpenAI-compat)  classify/extract     optional)
```

- **familiar-gateway** — the brain. Listens on `127.0.0.1:8000` (loopback). Runs
  DB migrations on boot. The HTTP API + admin console only start when launched
  with `--http`.
- **familiar-workspace** — the front door. Serves the SPA and proxies API calls
  to the gateway, **preserving the inbound `Host` header** (WebAuthn depends on
  this). This is the URL users actually open.
- **Model servers** — the gateway calls OpenAI-compatible HTTP endpoints
  (`llama.cpp`'s `llama-server`, vLLM, Ollama, or a hosted OpenAI/Anthropic API).
  None of these are part of this repo.

> The engine is **in-process Go** — memory and the sleep/consolidation cycle
> run inside the gateway against pgvector; there is no separate engine to run.

---

## 2. Prerequisites

On the target machine (Linux assumed):

- **Go** ≥ 1.25 (`go version`) — install from <https://go.dev/dl>.
- **PostgreSQL 14+ with the `pgvector` extension.** Easiest: Docker (the repo's
  `docker-compose.yml` uses the `pgvector/pgvector:pg17` image, which bundles it).
  For a native Postgres you must install `pgvector` separately.
- **A model backend** — one of:
  - a reachable OpenAI-compatible endpoint (a remote `llama-server`, vLLM, or a
    hosted API), **or**
  - a local `llama.cpp` `llama-server` (CPU is fine for testing — correctness
    doesn't need speed).
- **git**, and a way to reach this repo.
- Optional: **Docker** (for Postgres). To expose the instance beyond
  localhost you need TLS — either let the **workspace terminate it itself**
  (a `[tls]` cert/key block — see §4) or front it with a terminator
  (Caddy / nginx / Tailscale Serve).

A GPU is **not required** for a test instance — point the chat model at a remote
endpoint, or run a small model on CPU. (The `HSA_OVERRIDE_GFX_VERSION` env in the
systemd units is AMD-ROCm-specific and only matters if you run local AMD-GPU
model servers.)

---

## 3. Fresh instance — step by step

### 3.1 Clone

```sh
git clone <repo-url> ~/repos/familiar-engine
cd ~/repos/familiar-engine
```

Set a couple of shell vars to follow along (adjust as needed):

```sh
export REPO="$HOME/repos/familiar-engine"
export FAMILIAR_HOME="$HOME/.familiar"
mkdir -p "$FAMILIAR_HOME"/{skills,media}
```

### 3.2 PostgreSQL + pgvector

**Option A — Docker (recommended for a test box):**

```sh
cd "$REPO"
docker compose up -d        # starts pgvector/pgvector:pg17 on :5432
# user=familiar  password=familiar_dev  db=familiar  (see docker-compose.yml)
```

DSN: `postgresql://familiar:familiar_dev@localhost:5432/familiar`

**Option B — native Postgres:** create a database and ensure `pgvector` is
installed, e.g.:

```sh
createdb familiar
psql -d familiar -c 'CREATE EXTENSION IF NOT EXISTS vector;'
```

You do **not** need to run `init-db.sql` by hand. The gateway runs all its own
migrations on boot (and creates the `vector` extension itself if the DB role is
allowed to). `init-db.sql` is a legacy bootstrap kept only for the Docker image's
first-run hook.

### 3.3 Bring up a model backend

The gateway needs at least **one chat model**. Pick the simplest path for your box:

**Path 1 — point at an existing/remote endpoint (no local GPU).**
Use any OpenAI-compatible server you can reach (your "gpu-host", a colleague's
`llama-server`, vLLM, or a hosted API). You'll reference it in `[[models]]` in §3.5.

**Path 2 — run one small model locally with llama.cpp.**

```sh
# build llama.cpp once (see its README); then, e.g. CPU-only:
~/llama.cpp/build/bin/llama-server \
    --host 127.0.0.1 --port 8080 \
    -m ~/models/<some-instruct-model>.gguf \
    --ctx-size 32768 --parallel 2 --threads $(nproc) --jinja \
    --alias local-chat
```

**Optional — embedder** (enables semantic memory recall; the instance boots fine
without it, recall is just skipped):

```sh
~/llama.cpp/build/bin/llama-server \
    --host 127.0.0.1 --port 8100 \
    -m ~/models/nomic-embed-text-v1.5.f16.gguf \
    -c 2048 --embeddings
```

**Optional — sidecar** (a small fast model for classification / fact extraction
so the big model isn't used for cheap tasks). Skip it for a first bring-up:
set `[sidecar] enabled = false` and everything routes to the chat model.

> Full production topology for reference: chat big model on `:8080`/`:8085`,
> sidecar on `:8200`, embedder on `:8100`. See `systemd/familiar-*.service`.

### 3.4 Build the two binaries

```sh
# Gateway (engine is in-process; nothing else to build)
cd "$REPO/familiar-gateway"
go build -o familiar-gateway ./cmd/gateway/

# Workspace (web UI + reverse proxy)
cd "$REPO/familiar-workspace"
make build          # → ./familiar-workspace  (equivalent: go build -o familiar-workspace ./cmd/workspace/)
```

### 3.5 Write the gateway config

The gateway looks for its config at, in order: the `--config` path, then
`~/.familiar/gateway.toml`, then `./gateway.toml`. Create
`~/.familiar/gateway.toml`. Here is a **minimal, known-good test config** —
adjust the model `endpoint` and the `rp_origins`/host to your machine:

```toml
# ── HTTP API + admin console (REQUIRED; note the key is adapter.http) ──
[adapter.http]
listen_addr = "127.0.0.1:8000"     # keep on loopback; the workspace proxies to it

# ── Memory store (Postgres + pgvector) ──
[memory]
local_dsn = "postgresql://familiar:familiar_dev@localhost:5432/familiar"
relevance_threshold = 0.55
max_injected_memories = 5
dedup_threshold = 0.95

# ── System prompt: tiered dir (base.md + tier_*.md + tool_policy.md) ──
# The key is `prompt_dir` (NOT "dir"), and it points at the tiers/
# subdir — that's the folder that actually holds base.md. A bare
# `.../prompts` would be silently ignored (no base.md there).
[system_prompt]
prompt_dir = "/home/youruser/repos/familiar-engine/prompts/tiers"
# Legacy single-file alternative (used only when prompt_dir is unset/missing):
#   file = "/home/youruser/repos/familiar-engine/prompts/system_prompt.md"

# ── Chat model (at least one [[models]] is REQUIRED) ──
# capabilities MUST include "tools" for tool-calling to work.
[[models]]
id              = "local/chat"
provider        = "llama-server"             # llama-server | openai | ollama | vllm | llama-completion
endpoint        = "http://127.0.0.1:8080"    # ← your model server
context_window  = 32768
capabilities    = ["tools", "deep_reasoning", "conversation"]
latency_profile = "local"
max_concurrent  = 2
display_name    = "Local Chat"
# For a hosted API instead, e.g.:
#   provider = "openai"      # valid: llama-server | openai | ollama | vllm | llama-completion
#   endpoint = "https://api.openai.com/v1"   # any OpenAI-compatible base URL
#   api_key  = "sk-..."      # inline (test only) — or use vault_key for the encrypted vault
# (There is no dedicated "anthropic" provider — reach Anthropic via an
#  OpenAI-compatible endpoint under provider = "openai".)

# ── Sidecar: off for a first bring-up (everything routes to the chat model) ──
[sidecar]
enabled = false

# ── Embedder (OPTIONAL — omit to skip semantic recall) ──
[embedder]
endpoint  = "http://127.0.0.1:8100"
model     = "nomic-embed-text"
dimension = 768                              # must match the model's output dim

# ── Router (defaults are fine) ──
[router]
enabled = true
prefer_local = true

# ── Admin console + WebAuthn (REQUIRED to use the web UI) ──
[admin]
enabled          = true
first_user_id    = "operator"                  # canonical id for the FIRST account (set this!)
rp_display_name  = "Familiar (test)"
rp_id            = "localhost"               # the host the BROWSER sees — NO scheme, NO port
rp_origins       = ["http://localhost:3000"] # EVERY scheme+host+port the UI loads from
session_max_age  = 7200                      # idle timeout (seconds); sessions slide on use
cookie_secure    = false                     # false for http/localhost; true behind HTTPS
```

Notes that bite people:
- **`rp_id`** is the bare hostname the browser shows (no `http://`, no `:port`).
- **`rp_origins`** must list the exact origin(s) of the **workspace** URL the user
  opens (scheme + host + port). WebAuthn rejects anything not listed. For a LAN/
  remote box this is e.g. `https://your-host.ts.net` — and then
  `rp_id = "your-host.ts.net"`, `cookie_secure = true`.
- Multi-origin (public hostname + Tailscale, say) uses repeated
  `[[admin.relying_party]]` blocks instead of the single `rp_id`/`rp_origins`
  pair — see `config.example.toml`.

`config.example.toml` documents every other optional block (`[push]`, `[sharing]`,
`[skills]`, `[media]`, `[instance]`, `[context]`, `[adapter.slack]`, per-task
`[sidecar]` model assignments, etc.). Add them as needed.

### 3.6 Write the workspace config

Create `~/.familiar/workspace.toml` (or keep it next to the binary):

```toml
listen_addr = ":3000"                  # the URL users open; ":8443" if you set [tls]
gateway_url = "http://localhost:8000"  # must match [adapter.http].listen_addr
static_dir  = "/home/youruser/repos/familiar-engine/familiar-workspace/static"

# Optional: let the workspace terminate TLS itself (raw HTTPS, no proxy).
# This is all you need to expose the instance publicly — see §4 for the
# full walkthrough (cert, DNS, and the passkey/RP config that must match).
# [tls]
# cert = "/etc/letsencrypt/live/<host>/fullchain.pem"
# key  = "/etc/letsencrypt/live/<host>/privkey.pem"
```

Static files are served **from disk** (`static_dir`), so UI changes need only a
file update + browser reload — no rebuild. Use an absolute `static_dir` so it
doesn't depend on the working directory.

### 3.7 Run

In two terminals (or via systemd — §9):

```sh
# 1) gateway — note --http (this is what mounts the API + admin console)
cd "$REPO/familiar-gateway"
FAMILIAR_HOME="$HOME/.familiar" ./familiar-gateway --http --config "$HOME/.familiar/gateway.toml" --verbose

# 2) workspace
cd "$REPO/familiar-workspace"
./familiar-workspace --config "$HOME/.familiar/workspace.toml"
```

Gateway logs should show migrations running and `OpenAI adapter starting`.
Workspace logs show `listening on :3000`.

### 3.8 Register the first user (admin)

1. Open the **workspace** URL in a browser — e.g. `http://localhost:3000`
   (not the gateway's `:8000`).
2. With an empty database, the app shows a **first-run setup** screen. Enter an
   email + display name and register a passkey (Touch ID / Face ID / security key /
   platform authenticator).
3. The first credential is bound to `admin.first_user_id` and created as an
   **admin, approved** account. You're in.
4. Add more users from the admin **Users** panel: invite by email → it generates a
   one-time **enrollment link** (valid 48h) you relay to them out-of-band; they
   open it and register their own passkey. (There is no self-service signup.)

⚠️ Passkeys are origin-bound. If WebAuthn errors with an origin/RP mismatch, your
`rp_id`/`rp_origins` don't match the URL in the address bar — fix §3.5 and restart
the gateway.

### 3.9 Verify

```sh
# gateway health (through the workspace proxy)
curl -fsS http://localhost:3000/api/health && echo OK

# auth status (unauthenticated → 401 is expected before login)
curl -s http://localhost:3000/console/api/auth/status
```

Then in the browser: log in, open a chat, send a message — confirm the model
responds. Check the **System Status** panel (admin) — your model should show
`online`.

---

## 4. Exposing it beyond localhost — direct HTTPS & passkeys

You do **not** need Tailscale (or nginx, or Caddy) to put Familiar on the
internet. The workspace can terminate TLS itself, and that is a first-class,
supported path. Tailscale Serve is just *one* way to provide the HTTPS the
login flow requires — not the only one.

### Why HTTPS is mandatory (not optional)

Login is **WebAuthn passkeys**, which browsers only allow in a *secure
context*. Plain HTTP works for `localhost` but not for any remote host — so a
remote HTTP-only deployment can't log anyone in. The choice is therefore not
"HTTP vs HTTPS"; it's "who terminates the HTTPS." Either is fine:

- **Workspace-terminated (raw HTTPS).** Give the workspace a cert + key; it
  serves HTTPS directly on `listen_addr`. No second process.
- **Fronted.** A reverse proxy / `tailscale serve` terminates TLS and forwards
  to the workspace (plain HTTP is fine on that hop if it's loopback).

### Three things must line up for passkeys

1. **A DNS hostname — not a bare IP.** WebAuthn's Relying Party ID must be a
   registrable domain; browsers **reject IP-address literals**. So
   `https://203.0.113.10:8443` can't do passkeys, but `https://fam.example.com`
   can. You need a name that resolves to the box (a real domain, or a dynamic-DNS
   name).
2. **A browser-trusted certificate** for that name (Let's Encrypt via certbot,
   Caddy, `acme.sh`, …). Self-signed certs give a secure context only after a
   manual exception and several browsers still refuse WebAuthn on them — use a
   real cert.
3. **Gateway RP config that matches the hostname exactly.** `rp_id` is the bare
   host (no scheme, no port); `rp_origins` lists the exact `scheme://host[:port]`
   the browser shows; and `cookie_secure = true` so session cookies get `Secure`.

### Concrete config (hostname `fam.example.com`, cert from Let's Encrypt)

**`workspace.toml`** — terminate TLS on :8443:

```toml
listen_addr = ":8443"
gateway_url = "http://localhost:8000"
static_dir  = "/home/youruser/repos/familiar-engine/familiar-workspace/static"

[tls]
cert = "/etc/letsencrypt/live/fam.example.com/fullchain.pem"
key  = "/etc/letsencrypt/live/fam.example.com/privkey.pem"
```

**`gateway.toml`** `[admin]` — match the RP to that origin:

```toml
[admin]
enabled         = true
first_user_id   = "operator"
rp_display_name = "Familiar"
rp_id           = "fam.example.com"               # bare host, no scheme/port
rp_origins      = ["https://fam.example.com"]     # exact origin; add ":8443" only if the URL has a port
cookie_secure   = true                            # required behind HTTPS
```

If you serve on a non-443 port, the origin **includes the port**
(`https://fam.example.com:8443`) and users must type it — put a real `:443` in
front if you want a bare-hostname URL. The gateway itself stays on
`127.0.0.1:8000` regardless; the workspace is still the only public listener,
so firewall everything except the workspace's HTTPS port.

### Coexisting with an existing Tailscale setup

Passkeys are **scoped to their `rp_id`**, so a credential enrolled under
`host-a.your-tailnet.ts.net` will **not** authenticate against
`fam.example.com` — moving domains means re-enrolling. To run both origins at
once (public HTTPS *and* Tailscale-direct), drop the single `rp_id`/`rp_origins`
pair and use one `[[admin.relying_party]]` block per host:

```toml
[[admin.relying_party]]
rp_id   = "fam.example.com"
origins = ["https://fam.example.com"]

[[admin.relying_party]]
rp_id   = "host-a.your-tailnet.ts.net"
origins = ["https://host-a.your-tailnet.ts.net"]
hosts   = ["host-a.your-tailnet.ts.net"]
```

The gateway picks the block whose `hosts` matches the inbound `Host` header
(preserved by the workspace proxy), falling back to the first block. See
`config.example.toml` for the full multi-RP notes.

⚠️ Renew automatically. Let's Encrypt certs last 90 days; certbot's timer or
Caddy handle this, but the workspace reads the cert **at startup** — restart
`familiar-workspace` after a renewal (a certbot `--deploy-hook` that runs
`systemctl restart familiar-workspace` is the clean fix).

---

## 5. Minimal vs full instance

| Component | Needed to boot? | Notes |
|-----------|-----------------|-------|
| Postgres + pgvector | **Yes** | gateway migrates on boot |
| `[adapter.http]` + `--http` | **Yes** | no API/admin without it |
| ≥1 `[[models]]` chat model | **Yes** | `capabilities` must include `"tools"` |
| `[system_prompt]` | **Yes** | `prompt_dir` → the `prompts/tiers/` dir (holds `base.md`) |
| `[admin]` + WebAuthn RP | Yes, to use the web UI | rp_id/origins must match the workspace URL |
| Embedder | No | omit → no semantic memory recall |
| Sidecar | No | `enabled=false` → all tasks use the chat model |
| Push / Sharing / Skills / Media / Slack | No | opt-in blocks |

---

## 6. Updating an existing instance

UI-only change (HTML/CSS/JS): `git pull` is enough — static is served from disk.
**Hard-reload the browser once** (Cmd/Ctrl+Shift+R) so the service worker picks up
the new shell and cache-busted assets.

Gateway code change: rebuild + restart the gateway. The repo ships
`familiar-deploy.sh` which does pull → `go build` → `systemctl restart
familiar-gateway` → health check:

```sh
cd ~/repos/familiar-engine && ./familiar-deploy.sh
```

(It assumes the repo is at `~/repos/familiar-engine` and a `familiar-gateway`
systemd unit exists.)

Workspace **binary** change (Go code under `familiar-workspace/`, not static):
`cd familiar-workspace && make build && sudo systemctl restart familiar-workspace`.

Database migrations apply automatically on the next gateway boot — no manual step.

---

## 7. Configuration reference (where to look)

- **`config.example.toml`** — every gateway config block, annotated.
- **`familiar-workspace/workspace.toml.example`** — workspace config.

---

## 8. Web Push (optional)

```sh
cd familiar-gateway
./familiar-gateway --gen-vapid     # prints a [push] block with a fresh keypair
```

Paste the keys into `[push]` in `gateway.toml` (keep the private key secret — use
`${PUSH_VAPID_PRIVATE_KEY}` env expansion), set `subject = "mailto:you@example.com"`,
restart. iOS only fires Web Push for a **home-screen-installed** PWA.

---

## 9. Running under systemd (optional)

The committed units in `systemd/` use placeholder paths and a `youruser`
account — adapt them to your box. The two that matter for a web instance:

**`familiar-gateway.service`**:

```ini
[Service]
User=youruser
Environment=FAMILIAR_HOME=/home/youruser/.familiar
ExecStart=/home/youruser/repos/familiar-engine/familiar-gateway/familiar-gateway \
    --http --config /home/youruser/.familiar/gateway.toml --verbose
Restart=on-failure
RestartSec=5
```

**`familiar-workspace.service`:**

```ini
[Service]
User=youruser
WorkingDirectory=/home/youruser/repos/familiar-engine/familiar-workspace
ExecStart=/home/youruser/repos/familiar-engine/familiar-workspace/familiar-workspace \
    --config /home/youruser/.familiar/workspace.toml
Restart=on-failure
RestartSec=5
```

If you run local model servers as services too, see the example
`familiar-embedder.service` / `familiar-sidecar.service` (the `llama-server`
invocations + ports `:8100` / `:8200`; the `HSA_OVERRIDE_GFX_VERSION` env is
AMD-GPU-only — drop it for CPU or NVIDIA).

```sh
sudo cp systemd/familiar-*.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now familiar-gateway familiar-workspace
journalctl -u familiar-gateway -f
```

---

## 10. Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| API 404s / admin console missing | Gateway not started with `--http`, or `[adapter.http]` missing → it isn't listening where the workspace proxies. |
| `listen_addr` ignored / gateway on `:80` | The listener block must be `[adapter.http]` — that's the key the config struct binds. |
| WebAuthn "origin not allowed" / RP mismatch | `rp_id`/`rp_origins` must match the **workspace** URL exactly; behind TLS set `cookie_secure = true`. Exposing it publicly? See §4. |
| WebAuthn refuses on a bare IP / self-signed cert | `rp_id` must be a DNS name, not an IP; use a browser-trusted cert. §4. |
| First-run setup screen never appears | DB already has credentials, or `admin.enabled=false`, or `admin.first_user_id` unset. |
| Model shows `offline` in System Status | Wrong `endpoint`, model server down, or missing `"tools"` capability for tool calls. |
| No memory recall | No `[embedder]` configured, or embedder down, or `dimension` mismatch with the model. |
| UI changes don't appear after deploy | Hard-reload once (service worker caches the shell); a normal reload may serve the old shell. |
| pgvector errors on boot | DB lacks the `vector` extension and the role can't `CREATE EXTENSION` — install pgvector / grant, or use the Docker `pgvector/pgvector` image. |

Logs: `journalctl -u familiar-gateway -n 100` (or the foreground process). The
gateway logs the classifier verdict + chosen model per turn when `--verbose`.
