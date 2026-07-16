# Familiar

Familiar is an AI-enabled workspace where you can take Notes, collaborate in wikis, chat with models, and create smart automations. 

> *In arcane tradition, a familiar is a magical entity bound to a practitioner —
> it scouts, communicates, and serves as an extension of the caster's will.*

In a nutshell - What if OpenWebUI, OpenClaw, Obsidian, and Mediawiki had an unholy four-way and created a demon spawn. And it's here to help you. 

Familiar's chat and automations can use the data stored in your notes and wiki directly, without external integrations. Familiar can use skills (native go or imported) and have scopes, meaning you can define what data and capabilities it has access to. You can use to Familiar on the desktop web inteface or mobile, or slack with it. It's also an iOS and Android compatible portable web app designed to be pinned to your home screen, complete with the ability to send you notifications. Pin it to your desktop through Chrome for a more integrated experience on the Desktop. 

Familiar is built with security taken seriously. You can login however you want, as long as it is with a passkey.

![Familiar workspace — chat alongside a live note](docs/screenshot.png)

## Quick start

Full instructions — prerequisites, Postgres, model backends, config, first-user
passkey registration, systemd, and troubleshooting — are in
**[DEPLOYMENT.md](DEPLOYMENT.md)**.

The short version:

```sh
# 1. Postgres + pgvector
docker compose up -d

# 2. build both binaries
cd familiar-gateway && go build -o familiar-gateway ./cmd/gateway/ && cd ..
cd familiar-workspace && make build && cd ..

# 3. write ~/.familiar/gateway.toml and ~/.familiar/workspace.toml (see DEPLOYMENT.md)

# 4. run (note --http: it mounts the API + admin console)
./familiar-gateway/familiar-gateway --http --config ~/.familiar/gateway.toml &
./familiar-workspace/familiar-workspace --config ~/.familiar/workspace.toml &

# 5. open the workspace URL and register the first passkey
```

## Development

```sh
# gateway unit tests (needs a throwaway Postgres for the DB-backed ones)
cd familiar-gateway
FAMILIAR_TEST_DSN="postgresql://familiar_test:familiar_test@localhost:5432/familiar_test?sslmode=disable" \
  go test ./...

# end-to-end (Playwright) — builds both binaries into a temp dir and drives a browser
cd tests/e2e
npm install
FAMILIAR_TEST_DSN="postgresql://familiar_test:familiar_test@localhost:5432/familiar_test?sslmode=disable" \
  npx playwright test
```

See `tests/e2e/MAKE_TESTS.md` for the E2E harness details.

## Configuration

`config.example.toml` documents every gateway block. System prompts live in
`prompts/` as a tiered set (`base.md`, `tier_*.md`, `tool_policy.md`); point
`[system_prompt].dir` at it. The single config knobs you must set for a usable
instance: `[adapter.http].listen_addr`, `[memory].local_dsn`, at least one
`[[models]]` chat model (with `"tools"` in its capabilities), `[system_prompt]`,
and the `[admin]` WebAuthn relying-party (`rp_id` / `rp_origins`).

## Documentation

- **[DEPLOYMENT.md](DEPLOYMENT.md)** — new-instance setup and updates.
- **`config.example.toml`** — authoritative config reference.

## License

MIT
