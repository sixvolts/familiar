# Familiar

Familiar is an AI-enabled workspace where you can take Notes, collaborate in wikis, chat with models, and create smart automations. 

![Familiar workspace — chat alongside a live note](docs/screenshot.png)

> *In arcane tradition, a familiar is a magical entity bound to a practitioner —
> it scouts, communicates, and serves as an extension of the caster's will.*

Familiar has five major concepts - Chat, Notes, Wikis, scheduled actions, and shards.

## Chat
A chat interface for your local AI model that is tied into all of your data - your notes, wikis, and automations. Supports imported skills and system skills like research mode for doing deep dives and searches.

## Notes
Your personal notes, all indexed for easy access by familiar. Edit in rich text or directly in markdown. Share a note publicly if you want to, flattened into a read only page for external users.

## Wikis
Collaborate with other users just like you would expect in a simple mark-down enabled wiki. No bloat, just a simple way to share data with a team. Supports mermaid diagrams and images inline.

## Scheduled Actions
Schedule familiar to execute a task and deliver the output to you via Chat, Notes, Mobile Notifications, Slack or more. From cleaning up your notes every day and providing you a summary to scheduling searches for your current interests, or whatever else you can imagine.

## Shards
Shards are basically data-driven agents. You define the scope of data an agent has access to, it's skills, and how you can access it. You can use an API to access data in your notes, talk to a shard on slack that can only read your recipes, or define a scope for a task runner and assign it via a scheduled action.

Familiar is built with security taken seriously. You can login however you want, as long as it is with a passkey.



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
