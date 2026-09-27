-- Familiar — database bootstrap for the Docker image.
-- Runs automatically on first `docker compose up`.
--
-- The gateway creates and migrates its whole schema itself at startup
-- (familiar-gateway/internal/db/migrate.go). This file only enables
-- pgvector, which needs a superuser the gateway's role may not be on a
-- native cluster. It used to create a legacy schema (facts,
-- conversation_turns, entity_edges) that nothing reads.

CREATE EXTENSION IF NOT EXISTS vector;
