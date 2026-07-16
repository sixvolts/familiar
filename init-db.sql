-- Familiar — initial database schema
-- Runs automatically on first `docker compose up`.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Semantic facts / memories
CREATE TABLE IF NOT EXISTS facts (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    content     TEXT NOT NULL,
    embedding   vector(768),       -- matches nomic-embed-text default dim
    source_type TEXT NOT NULL DEFAULT 'conversation',
    source_ref  TEXT,
    source_desc TEXT,
    confidence  REAL NOT NULL DEFAULT 0.5,
    conf_basis  TEXT,
    scope       TEXT NOT NULL DEFAULT 'user',
    tags        TEXT[] DEFAULT '{}',
    domains     TEXT[] DEFAULT '{}',
    allowed_channels TEXT[] DEFAULT '{}',
    allowed_agents   TEXT[] DEFAULT '{}',
    agent_private    BOOLEAN NOT NULL DEFAULT FALSE,
    internal_only    BOOLEAN NOT NULL DEFAULT FALSE,
    supersedes       UUID REFERENCES facts(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_accessed    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_verified    TIMESTAMPTZ,
    access_count     BIGINT NOT NULL DEFAULT 0
);

-- HNSW index for fast ANN queries on embeddings
CREATE INDEX IF NOT EXISTS facts_embedding_idx
    ON facts USING hnsw (embedding vector_cosine_ops);

CREATE INDEX IF NOT EXISTS facts_scope_idx ON facts (scope);
CREATE INDEX IF NOT EXISTS facts_created_idx ON facts (created_at);

-- Conversation history
CREATE TABLE IF NOT EXISTS conversation_turns (
    id          BIGSERIAL PRIMARY KEY,
    session_id  TEXT NOT NULL,
    role        TEXT NOT NULL,
    content     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS conv_session_idx ON conversation_turns (session_id, created_at);

-- Entity-relationship edges (persistent knowledge graph tier)
CREATE TABLE IF NOT EXISTS entity_edges (
    id          BIGSERIAL PRIMARY KEY,
    from_entity TEXT NOT NULL,
    to_entity   TEXT NOT NULL,
    rel_type    TEXT NOT NULL,
    weight      REAL NOT NULL DEFAULT 1.0,
    metadata    JSONB DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS edges_from_idx ON entity_edges (from_entity);
CREATE INDEX IF NOT EXISTS edges_to_idx ON entity_edges (to_entity);
CREATE INDEX IF NOT EXISTS edges_rel_idx ON entity_edges (rel_type);

-- Layer 1 working context: small per-user profile always injected into the
-- system prompt by the ctxbuild assembler. Phase 2 seeds this manually; a
-- later phase lets the LLM edit it via a core_memory_update tool.
CREATE TABLE IF NOT EXISTS user_profiles (
    user_id         TEXT PRIMARY KEY,
    working_context JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Example seed (commented — edit and run manually):
-- INSERT INTO user_profiles (user_id, working_context) VALUES (
--   'U0123EXAMPLE',
--   '{"name":"Ada Lovelace","location":"Boring, OR","role":"Software Engineer",
--     "preferences":{"communication_style":"direct, technical, no fluff","timezone":"America/Los_Angeles"},
--     "active_goals":[],"recent_context":""}'::jsonb
-- ) ON CONFLICT (user_id) DO UPDATE SET working_context = EXCLUDED.working_context, updated_at = now();
