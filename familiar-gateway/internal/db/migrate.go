package db

import (
	"context"
	"fmt"
)

// migration is a single, idempotent DDL step. Order matters: later
// migrations may depend on tables from earlier ones. Every statement
// uses CREATE TABLE IF NOT EXISTS (or the equivalent) so Migrate is
// safe to run on every boot.
type migration struct {
	name string
	ddl  string
}

// migrations is the ordered schema history for every pool-shared table,
// including the pgvector `memories` table. The previous engine used to own
// the memories bootstrap on connect; since the engine migration the
// in-process memengine assumes the table exists, so `memories_base`
// below creates it before any migration that alters or references it.
var migrations = []migration{
	{
		name: "sessions",
		ddl: `
CREATE TABLE IF NOT EXISTS sessions (
    session_key      TEXT PRIMARY KEY,
    running_summary  TEXT NOT NULL DEFAULT '',
    summarized_count INT  NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`,
	},
	{
		// user_profiles holds the per-user "assistant personality"
		// prompt — a user-authored block of behavioral instructions
		// that rides as its own labeled section after the admin
		// system prompt. It once also carried a JSONB working_context
		// grab-bag; that was scrubbed (see the migration just below).
		name: "user_profiles",
		ddl: `
CREATE TABLE IF NOT EXISTS user_profiles (
    user_id     TEXT PRIMARY KEY,
    user_prompt TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`,
	},
	{
		// Retire the working_context JSONB blob. Its one prompt-shaped
		// key (user_prompt) is promoted to a typed column; the rest of
		// the blob (name/location/role/preferences/…) is dropped —
		// those are facts and re-accrete as memory rows going forward.
		// Wrapped in a DO block so it's idempotent across boots: the
		// second run finds working_context already gone and just
		// ensures the typed column exists.
		name: "user_profiles_scrub_working_context",
		ddl: `
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'user_profiles' AND column_name = 'working_context'
    ) THEN
        ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS user_prompt TEXT NOT NULL DEFAULT '';
        UPDATE user_profiles
           SET user_prompt = working_context->>'user_prompt'
         WHERE COALESCE(working_context->>'user_prompt', '') <> ''
           AND user_prompt = '';
        ALTER TABLE user_profiles DROP COLUMN working_context;
    ELSE
        ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS user_prompt TEXT NOT NULL DEFAULT '';
    END IF;
END $$;`,
	},
	{
		name: "identity_map",
		ddl: `
CREATE TABLE IF NOT EXISTS identity_map (
    platform     TEXT NOT NULL,
    platform_id  TEXT NOT NULL,
    canonical_id TEXT NOT NULL,
    display_name TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (platform, platform_id)
);`,
	},
	// Identity seeding lives in internal/identity/Bootstrap, not in
	// schema migrations: it's operator policy ("here's who's allowed
	// to use this deployment"), not a database structure change.
	// Configure via [identity.seed] in gateway.toml; the seeder runs
	// on every startup and is idempotent.
	{
		// Base pgvector memories table. Historically bootstrapped by the
		// previous engine on connect, which meant a fresh database had no
		// `memories` relation and every later migration that touches it
		// (memories_user_id, memory_versions, memories_scope_tag, the
		// owner rename, ...) failed with 42P01. The engine is gone
		//, so the migration list owns the base
		// schema now.
		//
		// Two deliberate choices for portability across deployments:
		//
		//   - `embedding vector` is dimensionless. The embedder dimension
		//     is operator config (768 nomic, 1024 bge/mxbai, ...) and
		//     every query casts the parameter ($1::vector), never the
		//     column, so pgvector enforces consistency per-row. An ANN
		//     index (which would pin a dimension) was never created here;
		//     deployments that have one keep it.
		//
		//   - The (agent_id, content_hash) unique index backs the
		//     memengine's ON CONFLICT dedup. The DO block creates it only
		//     if no unique index on that column pair exists, so a deploy
		//     whose pre-existing index has a different name doesn't grow
		//     a duplicate.
		//
		// user_id and scope_tag are intentionally absent: the
		// memories_user_id / memories_scope_tag migrations below add
		// them, preserving the original schema history on databases
		// that predate this migration.
		name: "memories_base",
		ddl: `
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS memories (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id           TEXT NOT NULL,
    scope              TEXT NOT NULL DEFAULT 'session',
    content            TEXT NOT NULL,
    content_hash       TEXT,
    embedding          vector,
    source_type        TEXT NOT NULL DEFAULT 'conversation',
    source_ref         TEXT,
    source_description TEXT,
    confidence         REAL NOT NULL DEFAULT 0.5,
    confidence_basis   TEXT,
    tags               TEXT[] NOT NULL DEFAULT '{}',
    supersedes         UUID REFERENCES memories(id),
    decay_score        REAL,
    access_count       BIGINT NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_accessed      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_memories_supersedes
    ON memories (supersedes) WHERE supersedes IS NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM pg_index i
         WHERE i.indrelid = 'memories'::regclass
           AND i.indisunique
           AND i.indnatts = 2
           AND (
               SELECT array_agg(a.attname::text ORDER BY k.ord)
                 FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
                 JOIN pg_attribute a
                   ON a.attrelid = i.indrelid AND a.attnum = k.attnum
           ) = ARRAY['agent_id','content_hash']
    ) THEN
        CREATE UNIQUE INDEX idx_memories_agent_content_hash
            ON memories (agent_id, content_hash);
    END IF;
END $$;`,
	},
	{
		// Schema extension the gateway layers on top of the previous engine's
		// memories table. user_id identifies the owner of a fact; NULL
		// means "global, visible to everyone." The previous engine is
		// expected to start populating this column in a later phase;
		// until then the gateway writes via CommitFacts (engine-side) and
		// reads via pgvector.Search with a (user_id IS NULL OR user_id =
		// $n) predicate so existing unscoped rows remain visible.
		name: "memories_user_id",
		ddl: `
ALTER TABLE IF EXISTS memories ADD COLUMN IF NOT EXISTS user_id TEXT;
CREATE INDEX IF NOT EXISTS idx_memories_user_id
    ON memories (user_id) WHERE user_id IS NOT NULL;`,
	},
	{
		// Canonical user registry. Every row in identity_map points at a
		// users.id; a user has one row here and zero-or-more platform
		// links in identity_map. Status gates all pipeline access:
		//   pending  — created by a Slack DM from an unknown account; no
		//              LLM call or memory write, adapter replies "ask an
		//              admin to approve"
		//   approved — full access on every linked platform
		//   denied   — tombstoned request; repeat Slack DMs still bounce
		//              but do not re-open the request
		//   disabled — previously approved, now revoked
		name: "users",
		ddl: `
CREATE TABLE IF NOT EXISTS users (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    approved_at  TIMESTAMPTZ,
    approved_by  TEXT
);
CREATE INDEX IF NOT EXISTS idx_users_status ON users (status);`,
	},
	{
		// Backfill: every canonical_id currently referenced by
		// identity_map corresponds to a user that predates this table.
		// Mark them all approved so existing Slack / CLI / OpenAI traffic
		// keeps working. Display name picks an arbitrary value from the
		// group (they're typically consistent anyway).
		//
		// GATED ONE-SHOT: without the applied_data_fixes marker this
		// re-runs every boot, so a user deleted from `users` whose
		// identity_map rows linger would be silently RE-INSERTED as
		// approved on the next restart. Run it once and never again.
		name: "users_backfill",
		ddl: `
CREATE TABLE IF NOT EXISTS applied_data_fixes (
    name       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM applied_data_fixes WHERE name = 'users_backfill') THEN
        INSERT INTO users (id, display_name, status, approved_at)
        SELECT canonical_id,
               COALESCE(MAX(display_name), canonical_id),
               'approved',
               NOW()
        FROM identity_map
        GROUP BY canonical_id
        ON CONFLICT (id) DO NOTHING;
        INSERT INTO applied_data_fixes (name) VALUES ('users_backfill');
    END IF;
END $$;`,
	},
	{
		// Admin console WebAuthn credentials. One row per registered
		// authenticator (YubiKey, platform passkey, ...). user_id is the
		// canonical identity that owns the key; display_name is the human
		// label shown in the dashboard. sign_count is the replay-protection
		// counter the WebAuthn library bumps on every successful assertion.
		name: "webauthn_credentials",
		ddl: `
CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id              TEXT PRIMARY KEY,
    credential_blob BYTEA NOT NULL,
    user_id         TEXT NOT NULL,
    display_name    TEXT,
    sign_count      BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_user_id
    ON webauthn_credentials (user_id);`,
	},
	{
		// Admin sessions. Random 256-bit tokens live in an HttpOnly cookie
		// on the browser and index this table. expires_at is enforced on
		// every validate call; a bounded Cleanup pass deletes rows past
		// their TTL so the table never grows unbounded.
		name: "admin_sessions",
		ddl: `
CREATE TABLE IF NOT EXISTS admin_sessions (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires_at
    ON admin_sessions (expires_at);`,
	},
	{
		// Lightweight entity-relationship layer. Populated by the
		// sidecar fact-extraction pipeline alongside facts: each triple
		// (subject, predicate, object) captures a structured edge that
		// vector search can't express directly ("gpu-host has_ip
		// 10.0.0.10"). At retrieval time the pipeline augments the
		// memory block with one-hop neighbours of any entity mentioned
		// in a retrieved fact.
		//
		// user_id follows the same visibility rule as memories: NULL is
		// global, non-NULL is owned by one canonical user. The unique
		// index on (subject, predicate, user_id_key) lets UpsertRelationships
		// do ON CONFLICT DO UPDATE when the object changes (IP moved,
		// version bumped, etc.); user_id_key collapses NULL to '' so
		// Postgres treats global rows as a single conflict class.
		name: "relationships",
		ddl: `
CREATE TABLE IF NOT EXISTS relationships (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subject     TEXT NOT NULL,
    predicate   TEXT NOT NULL,
    object      TEXT NOT NULL,
    user_id     TEXT,
    user_id_key TEXT GENERATED ALWAYS AS (COALESCE(user_id, '')) STORED,
    source_fact UUID,
    confidence  REAL NOT NULL DEFAULT 1.0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rel_subject_pred_user
    ON relationships (subject, predicate, user_id_key);
CREATE INDEX IF NOT EXISTS idx_rel_subject ON relationships (subject);
CREATE INDEX IF NOT EXISTS idx_rel_object  ON relationships (object);
CREATE INDEX IF NOT EXISTS idx_rel_user    ON relationships (user_id) WHERE user_id IS NOT NULL;`,
	},
	{
		// Phase C multi-user integrity: every memory row must be owned
		// by exactly one user. Legacy rows were flushed before the previous
		// engine knew about user_id and have NULL; backfill them to the
		// owner (operator) so nothing vanishes from retrieval, then lock
		// the column with NOT NULL + a non-empty check so any future
		// code path that forgets to stamp user_id fails at the DB.
		// Schema constraints only — the original "backfill NULLs to
		// 'owner'" UPDATE was a one-shot data fix for the upgrade
		// from single-user to multi-user. Operators upgrading from
		// pre-multi-user must backfill user_id on existing rows
		// themselves before this migration runs (otherwise SET NOT
		// NULL fails loud, which is the desired outcome — picking a
		// canonical user for someone else's data isn't the schema's
		// job).
		name: "memories_user_id_constraints",
		ddl: `
ALTER TABLE memories ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE memories DROP CONSTRAINT IF EXISTS memories_user_id_nonempty;
ALTER TABLE memories ADD CONSTRAINT memories_user_id_nonempty CHECK (user_id <> '');`,
	},
	{
		name: "memory_versions",
		ddl: `
CREATE TABLE IF NOT EXISTS memory_versions (
    id          UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    memory_id   UUID NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    content     TEXT NOT NULL,
    scope       TEXT,
    source_type TEXT,
    version     INT NOT NULL DEFAULT 1,
    changed_by  TEXT,
    change_type TEXT,
    created_at  TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_memver_memory ON memory_versions(memory_id, version DESC);`,
	},
	{
		// Phase 1 of FAMILIAR-SHARDS-PHASE1-SPEC: shard registry. A shard is
		// a named bundle of prompt + memory scope + tool allowlist that
		// bounds what an external caller can do with Familiar. Invoke-only
		// in this phase; later phases layer on scheduler, webhooks, and
		// sharded Slack bots. scope_tag uniqueness is per-owner so two
		// owners can each have their own `shard:notes` without collision.
		name: "shards",
		ddl: `
CREATE TABLE IF NOT EXISTS shards (
    id                TEXT PRIMARY KEY,
    owner_id          TEXT NOT NULL REFERENCES users(id),
    name              TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    persistence       TEXT NOT NULL CHECK (persistence IN ('persistent', 'ephemeral')),
    visibility        TEXT NOT NULL CHECK (visibility IN ('isolated', 'promoted')),
    scope_tag         TEXT NOT NULL,
    tool_allowlist    JSONB NOT NULL DEFAULT '[]'::jsonb,
    system_prompt     TEXT NOT NULL,
    model_preference  TEXT NOT NULL DEFAULT '',
    tier_preference   TEXT NOT NULL DEFAULT '',
    input_schema      JSONB,
    output_schema     JSONB,
    max_tokens        INTEGER NOT NULL DEFAULT 2048,
    temperature       REAL NOT NULL DEFAULT 0.7,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disabled_at       TIMESTAMPTZ,
    UNIQUE (owner_id, scope_tag)
);
CREATE INDEX IF NOT EXISTS idx_shards_owner ON shards(owner_id) WHERE disabled_at IS NULL;`,
	},
	{
		// Bearer tokens scoped 1:1 to a shard. token_hash is bcrypt; plaintext
		// is shown to the operator exactly once at mint. token_prefix keeps
		// the first 8 chars of plaintext unhashed so the admin UI can
		// disambiguate tokens in a list without ever re-displaying the
		// secret. expires_at is in the schema but Phase 1 does not enforce
		// it — zero-work upgrade when enforcement lands.
		name: "shard_tokens",
		ddl: `
CREATE TABLE IF NOT EXISTS shard_tokens (
    id            TEXT PRIMARY KEY,
    shard_id      TEXT NOT NULL REFERENCES shards(id) ON DELETE CASCADE,
    owner_id      TEXT NOT NULL REFERENCES users(id),
    label         TEXT NOT NULL DEFAULT '',
    token_hash    TEXT NOT NULL,
    token_prefix  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_shard_tokens_shard ON shard_tokens(shard_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_shard_tokens_prefix ON shard_tokens(token_prefix);`,
	},
	{
		// Shard-scoped sessions. NULL for top-level Familiar sessions
		// (existing behavior); non-NULL for sessions owned by a persistent
		// shard. Ephemeral shards never create session rows.
		name: "sessions_scope_tag",
		ddl: `
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS scope_tag TEXT;
CREATE INDEX IF NOT EXISTS idx_sessions_scope ON sessions(scope_tag) WHERE scope_tag IS NOT NULL;`,
	},
	{
		// Shard-scoped memories. Populated when a shard writes memory; NULL
		// for top-level writes. Top-level retrieval excludes rows whose
		// scope_tag belongs to an `isolated` shard; shard retrieval filters
		// to the shard's own scope_tag.
		name: "memories_scope_tag",
		ddl: `
ALTER TABLE memories ADD COLUMN IF NOT EXISTS scope_tag TEXT;
CREATE INDEX IF NOT EXISTS idx_memories_scope_tag ON memories(scope_tag) WHERE scope_tag IS NOT NULL;`,
	},
	{
		// Phase 1 of FAMILIAR-USER-CONSOLE-SPEC: per-user RBAC scaffolding.
		// Adds role/email/bootstrap_source columns + a unique-where-not-null
		// email index. Phase 2 wires these into the admin middleware; until
		// then `role` is populated but not consulted, so this migration is
		// behavior-neutral for existing deploys.
		//
		// Two data updates are intentional one-shots, not portable seeds:
		//
		//   1. Promote the historical owner row to admin. New deploys go
		//      through the Phase-4 first-run registration, which creates
		//      the owner with role='admin' directly. Operators upgrading
		//      from older Familiar installs without an "owner" row should
		//      promote their admin manually after this migration runs.
		//
		//   2. Backfill emails from existing openai identity_map rows. The
		//      openai platform_id has always been an email per the current
		//      adapter convention, so this is data-driven and portable —
		//      it copies real emails into users.email wherever the link
		//      already exists.
		name: "add_user_roles_and_email",
		ddl: `
ALTER TABLE users
  ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'user'
    CHECK (role IN ('admin', 'user'));

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS email TEXT;

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS bootstrap_source TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_unique
  ON users(email) WHERE email IS NOT NULL;

-- GATED ONE-SHOT: the two backfills below are DATA fixes, not DDL, and
-- must run exactly once. Ungated they re-fire every boot — the
-- owner->admin UPDATE would auto-promote any FUTURE user who lands the
-- 'owner' id, and the email backfill would resurrect a deliberately
-- cleared email. The column ADDs / index above stay ungated (already
-- idempotent).
CREATE TABLE IF NOT EXISTS applied_data_fixes (
    name       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM applied_data_fixes WHERE name = 'user_roles_email_backfill') THEN
        UPDATE users SET role = 'admin' WHERE id = 'owner' AND role <> 'admin';

        UPDATE users u
           SET email = im.platform_id,
               bootstrap_source = COALESCE(bootstrap_source, 'manual')
          FROM identity_map im
         WHERE im.platform = 'openai'
           AND im.canonical_id = u.id
           AND im.platform_id LIKE '%@%'
           AND u.email IS NULL;

        INSERT INTO applied_data_fixes (name) VALUES ('user_roles_email_backfill');
    END IF;
END $$;`,
	},
	{
		// FAMILIAR-WORKSPACE-SPEC Phase 1 — Chat surface storage.
		// conversations is one row per chat thread; messages is the
		// per-turn append log. ON DELETE CASCADE on the FK keeps
		// "delete conversation" a single statement. Dedicated
		// indexes for the two access patterns the workspace exercises:
		// list-conversations-for-user (most-recent first) and
		// load-messages-for-conversation (oldest first).
		name: "workspace_conversations",
		ddl: `
CREATE TABLE IF NOT EXISTS conversations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     TEXT NOT NULL REFERENCES users(id),
    title       TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT 'familiar',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    archived_at TIMESTAMPTZ,
    pinned      BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_conversations_user
    ON conversations(user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS messages (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id   UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role              TEXT NOT NULL CHECK (role IN ('user','assistant','system','tool')),
    content           TEXT NOT NULL,
    model             TEXT,
    tool_calls        JSONB,
    tool_call_id      TEXT,
    tokens_prompt     INTEGER,
    tokens_completion INTEGER,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation
    ON messages(conversation_id, created_at);`,
	},
	{
		// Add reasoning_content to messages so the chat surface's
		// thinking/reasoning trace persists across reload. Same
		// channel the gateway emits via SSE delta.reasoning_content
		// (extended-thinking, qwen <think> blocks normalized by
		// llama-server, AND pipeline status updates from the
		// adapter). Critical for debugging Familiar's pipeline
		// after a conversation closes.
		name: "messages_reasoning_content",
		ddl: `
ALTER TABLE messages ADD COLUMN IF NOT EXISTS reasoning_content TEXT;`,
	},
	{
		// FAMILIAR-WORKSPACE-SPEC Phase 2 — Notes surface storage.
		// Per-user markdown documents with optional folder grouping
		// and full-text search. Soft delete via deleted_at; hard
		// purge happens at retention-cron time (not yet wired).
		// The notes skill reads + writes through this table; the
		// Notes panel reads + writes through the books migration
		// (personal book pages, BOOKS-WIKI-ARCHITECTURE).
		name: "workspace_notes",
		ddl: `
CREATE TABLE IF NOT EXISTS notes (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     TEXT NOT NULL REFERENCES users(id),
    title       TEXT NOT NULL DEFAULT 'Untitled',
    content     TEXT NOT NULL DEFAULT '',
    folder      TEXT,
    pinned      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_notes_user
    ON notes(user_id, updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notes_folder
    ON notes(user_id, folder) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notes_search
    ON notes USING gin(to_tsvector('english', title || ' ' || content));`,
	},
	{
		// BOOKS-WIKI-ARCHITECTURE Phase 1a — books + book_members +
		// wiki_pages + wiki_revisions. Books are named collections
		// of wiki pages shared between specific users via the
		// book_members join table. Pages live inside exactly one
		// book; slug uniqueness is per-book, not global.
		// parent_id + sort_order are present for Phase 2 hierarchy
		// but unused in 1a (no nesting yet).
		// maintained_by + edit_count are populated in Phase 3.
		name: "books_and_wiki",
		ddl: `
CREATE TABLE IF NOT EXISTS books (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT UNIQUE NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    archived_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_book_slug ON books(slug);

CREATE TABLE IF NOT EXISTS book_members (
    book_id   UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    user_id   TEXT NOT NULL REFERENCES users(id),
    role      TEXT NOT NULL DEFAULT 'editor'
              CHECK (role IN ('owner', 'editor', 'viewer')),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (book_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_book_members_user ON book_members(user_id);

CREATE TABLE IF NOT EXISTS wiki_pages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    book_id         UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    slug            TEXT NOT NULL,
    title           TEXT NOT NULL,
    content         TEXT NOT NULL DEFAULT '',
    parent_id       UUID REFERENCES wiki_pages(id),
    sort_order      INTEGER NOT NULL DEFAULT 0,
    created_by      TEXT NOT NULL REFERENCES users(id),
    updated_by      TEXT NOT NULL REFERENCES users(id),
    maintained_by   TEXT REFERENCES users(id),
    edit_count      INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,
    UNIQUE (book_id, slug)
);
CREATE INDEX IF NOT EXISTS idx_wiki_book
    ON wiki_pages(book_id, updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_wiki_parent
    ON wiki_pages(parent_id, sort_order);
CREATE INDEX IF NOT EXISTS idx_wiki_search
    ON wiki_pages USING gin(to_tsvector('english', title || ' ' || content));

CREATE TABLE IF NOT EXISTS wiki_revisions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    page_id     UUID NOT NULL REFERENCES wiki_pages(id) ON DELETE CASCADE,
    content     TEXT NOT NULL,
    edited_by   TEXT NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    summary     TEXT
);
CREATE INDEX IF NOT EXISTS idx_wiki_revisions
    ON wiki_revisions(page_id, created_at DESC);

ALTER TABLE relationships
    ADD COLUMN IF NOT EXISTS scope_tag TEXT;
CREATE INDEX IF NOT EXISTS idx_rel_scope_tag
    ON relationships(scope_tag) WHERE scope_tag IS NOT NULL;`,
	},
	{
		// BOOKS-WIKI-ARCHITECTURE bug fix — wiki_pages slug
		// uniqueness was a full-table UNIQUE (book_id, slug),
		// which collided with soft-deleted rows. Creating a page
		// whose slug had ever been used in the same book (then
		// soft-deleted) would 23505 even though uniquePageSlug
		// already filters deleted_at IS NULL. Convert to a
		// partial unique index so the constraint only applies to
		// live rows; soft-deleted pages no longer block slug
		// reuse, and the retention cron's eventual hard-purge is
		// a no-op against this index.
		name: "wiki_pages_partial_slug_unique",
		ddl: `
ALTER TABLE wiki_pages DROP CONSTRAINT IF EXISTS wiki_pages_book_id_slug_key;
DROP INDEX IF EXISTS wiki_pages_book_id_slug_key;
CREATE UNIQUE INDEX IF NOT EXISTS wiki_pages_book_slug_active
    ON wiki_pages (book_id, slug) WHERE deleted_at IS NULL;`,
	},
	{
		// BOOKS-WIKI-ARCHITECTURE permission update — three-tier
		// enforced roles:
		//   editor → writer   (page CRUD; cannot manage members)
		//   viewer → reader   (read-only; enforced from now on)
		//   owner stays owner (manages members + book settings)
		// The original CHECK constraint and DEFAULT used the old
		// names. Drop the constraint, rewrite the row data, add
		// the new constraint, and flip the DEFAULT — all
		// idempotent so a re-run is a no-op.
		name: "books_role_rename",
		ddl: `
ALTER TABLE book_members DROP CONSTRAINT IF EXISTS book_members_role_check;
UPDATE book_members SET role = 'writer' WHERE role = 'editor';
UPDATE book_members SET role = 'reader' WHERE role = 'viewer';
ALTER TABLE book_members ADD CONSTRAINT book_members_role_check
    CHECK (role IN ('owner', 'writer', 'reader'));
ALTER TABLE book_members ALTER COLUMN role SET DEFAULT 'writer';`,
	},
	{
		// BOOKS-WIKI-ARCHITECTURE Phase 1 schema additions. Pure
		// table + column adds — no behavior change yet. The wiki
		// store + handlers continue working unchanged; subsequent
		// phases (personal books, link parser, knowledge pipeline)
		// each turn on the readers/writers for these surfaces.
		//
		//   books.is_personal       — flag for per-user personal
		//                             books (auto-created, hidden
		//                             from /books listing)
		//   user_page_prefs         — per-user pinning + future
		//                             display flags; works on both
		//                             personal and shared pages
		//   wiki_page_links         — explicit [[]] graph edges,
		//                             populated on save by the
		//                             link parser (Phase 1 step 3)
		//   wiki_page_entities      — pages-as-entities registry
		//                             that bridges wiki nodes and
		//                             the entity graph (Phase 2
		//                             knowledge pipeline reads it)
		//   relationships.scope_tag — book-scoped facts. Existing
		//                             relationships rows have NULL
		//                             scope_tag (global facts).
		name: "wiki_phase1_schema_additions_v2",
		ddl: `
ALTER TABLE books
    ADD COLUMN IF NOT EXISTS is_personal BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_book_personal
    ON books(created_by) WHERE is_personal = true;

CREATE TABLE IF NOT EXISTS user_page_prefs (
    user_id     TEXT NOT NULL REFERENCES users(id),
    page_id     UUID NOT NULL REFERENCES wiki_pages(id) ON DELETE CASCADE,
    pinned      BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (user_id, page_id)
);
CREATE INDEX IF NOT EXISTS idx_user_page_prefs_user
    ON user_page_prefs(user_id) WHERE pinned = true;

CREATE TABLE IF NOT EXISTS wiki_page_links (
    source_page_id   UUID NOT NULL REFERENCES wiki_pages(id) ON DELETE CASCADE,
    target_book_slug TEXT,
    target_page_slug TEXT NOT NULL,
    target_page_id   UUID REFERENCES wiki_pages(id) ON DELETE SET NULL,
    display_text     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_wiki_page_links_pk
    ON wiki_page_links(source_page_id, COALESCE(target_book_slug, ''), target_page_slug);
CREATE INDEX IF NOT EXISTS idx_wiki_links_target
    ON wiki_page_links(target_page_id) WHERE target_page_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wiki_links_broken
    ON wiki_page_links(source_page_id) WHERE target_page_id IS NULL;

CREATE TABLE IF NOT EXISTS wiki_page_entities (
    page_id     UUID PRIMARY KEY REFERENCES wiki_pages(id) ON DELETE CASCADE,
    entity_name TEXT NOT NULL,
    book_id     UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_wiki_page_entities_name
    ON wiki_page_entities(entity_name, book_id);

ALTER TABLE relationships
    ADD COLUMN IF NOT EXISTS scope_tag TEXT;
CREATE INDEX IF NOT EXISTS idx_rel_scope_tag
    ON relationships(scope_tag) WHERE scope_tag IS NOT NULL;`,
	},
	{
		// Owner-mutation audit log. Tracks who changed membership or
		// book settings, when, and what changed. Append-only; no
		// updates or deletes. The retention story (if any) is
		// future-work — for now rows live forever, scoped only by
		// the cascading delete from books.
		//
		// action enum is intentionally a free-text column so we can
		// add new event kinds without a schema migration. Known
		// values today:
		//   - member_added           (target_user_id, new_value=role)
		//   - member_role_changed    (target_user_id, old_value=old_role, new_value=new_role)
		//   - member_removed         (target_user_id, old_value=role)
		//   - book_renamed           (old_value=old_name, new_value=new_name)
		//   - book_archived          (no values)
		//   - book_unarchived        (no values)
		name: "wiki_book_audit",
		ddl: `
CREATE TABLE IF NOT EXISTS book_audit (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    book_id         UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    actor_user_id   TEXT NOT NULL,
    action          TEXT NOT NULL,
    target_user_id  TEXT,
    old_value       TEXT,
    new_value       TEXT,
    metadata        JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_book_audit_book_time
    ON book_audit (book_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_book_audit_actor
    ON book_audit (actor_user_id, created_at DESC);`,
	},
	{
		// Slugs should always reflect the page title. Early pages
		// were created with "untitled-N" slugs before being renamed.
		// This one-time fix derives the slug from the current title.
		//
		// GATED ONE-SHOT: guarded by the applied_data_fixes marker so
		// it runs exactly once per deployment. The WHERE-clause is NOT
		// self-idempotent in the way it looks — UpdatePage lets users
		// set a custom slug that differs from slugify(title), and an
		// ungated re-run reverts every such slug to the title form on
		// EVERY boot, breaking external links and wiki_page_links. The
		// marker makes "already ran" the boot-cheap path and leaves
		// user slugs alone forever after.
		//
		// Collision-safe within the one run: two live pages in one book
		// whose titles normalize to the same slug would both rewrite to
		// it and trip wiki_pages_book_slug_active. The rn = 1 window
		// keeps at most one rename candidate per (book, target slug),
		// and the NOT EXISTS guard yields to a page already holding it.
		name: "wiki_fix_page_slugs_from_title",
		ddl: `
CREATE TABLE IF NOT EXISTS applied_data_fixes (
    name       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM applied_data_fixes WHERE name = 'wiki_fix_page_slugs_from_title') THEN
        WITH candidates AS (
            SELECT id, book_id,
                   TRIM(BOTH '-' FROM REGEXP_REPLACE(LOWER(TRIM(title)), '[^a-z0-9]+', '-', 'g')) AS new_slug,
                   ROW_NUMBER() OVER (
                       PARTITION BY book_id,
                                    TRIM(BOTH '-' FROM REGEXP_REPLACE(LOWER(TRIM(title)), '[^a-z0-9]+', '-', 'g'))
                       ORDER BY created_at, id
                   ) AS rn
              FROM wiki_pages
             WHERE deleted_at IS NULL
        )
        UPDATE wiki_pages p
           SET slug = c.new_slug
          FROM candidates c
         WHERE p.id = c.id
           AND c.rn = 1
           AND p.slug <> c.new_slug
           AND NOT EXISTS (
               SELECT 1 FROM wiki_pages q
                WHERE q.book_id = p.book_id
                  AND q.slug = c.new_slug
                  AND q.deleted_at IS NULL
                  AND q.id <> p.id);
        INSERT INTO applied_data_fixes (name) VALUES ('wiki_fix_page_slugs_from_title');
    END IF;
END $$;`,
	},
	{
		// SHARD-AUTH-SPEC Phase 1 schema additions. Three surfaces:
		//
		//   shard_passkeys     — WebAuthn credentials bound to a
		//                        shard (instead of a user). Mirrors
		//                        webauthn_credentials structure but
		//                        keyed on shards.id; ON DELETE
		//                        CASCADE so a deleted shard takes
		//                        its passkeys with it. Stores the
		//                        marshaled webauthn.Credential blob
		//                        for round-tripping into the
		//                        go-webauthn library — same pattern
		//                        the user-credentials store uses.
		//   admin_sessions     — principal_type / principal_id let
		//                        the same table back both user and
		//                        shard sessions. Existing rows
		//                        backfill to ('user', user_id) so
		//                        the auth middleware reads the new
		//                        columns as the source of truth.
		//   shards             — console_access / console_panels /
		//                        book_access / chat_enabled /
		//                        api_enabled / session_max_age make
		//                        each shard's permission envelope
		//                        explicit. chat_enabled + api_enabled
		//                        default true to preserve the
		//                        current "shard = LLM-invocable
		//                        bearer-token bucket" behavior;
		//                        console_access defaults false so a
		//                        shard can't be logged into through
		//                        the browser without an opt-in.
		//
		// All idempotent: ADD COLUMN IF NOT EXISTS + CREATE TABLE
		// IF NOT EXISTS so a re-run is a no-op.
		name: "shard_auth_phase1",
		ddl: `
CREATE TABLE IF NOT EXISTS shard_passkeys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    shard_id        TEXT NOT NULL REFERENCES shards(id) ON DELETE CASCADE,
    credential_id   BYTEA NOT NULL UNIQUE,
    credential_blob BYTEA NOT NULL,
    public_key      BYTEA,
    sign_count      BIGINT NOT NULL DEFAULT 0,
    transports      TEXT[],
    aaguid          BYTEA,
    label           TEXT NOT NULL DEFAULT '',
    created_by      TEXT NOT NULL REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at    TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_shard_passkeys_cred
    ON shard_passkeys(credential_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_shard_passkeys_shard
    ON shard_passkeys(shard_id);

ALTER TABLE admin_sessions
    ADD COLUMN IF NOT EXISTS principal_type TEXT NOT NULL DEFAULT 'user';
ALTER TABLE admin_sessions
    ADD COLUMN IF NOT EXISTS principal_id   TEXT NOT NULL DEFAULT '';
DO $shard_auth_check$ BEGIN
    BEGIN
        ALTER TABLE admin_sessions
            ADD CONSTRAINT admin_sessions_principal_type_check
            CHECK (principal_type IN ('user', 'shard'));
    EXCEPTION WHEN duplicate_object THEN
        -- already added on a prior run
        NULL;
    END;
END $shard_auth_check$;
UPDATE admin_sessions SET principal_id = user_id
 WHERE principal_type = 'user' AND principal_id = '';

ALTER TABLE shards
    ADD COLUMN IF NOT EXISTS console_access  BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE shards
    ADD COLUMN IF NOT EXISTS console_panels  TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE shards
    ADD COLUMN IF NOT EXISTS book_access     TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE shards
    ADD COLUMN IF NOT EXISTS chat_enabled    BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE shards
    ADD COLUMN IF NOT EXISTS api_enabled     BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE shards
    ADD COLUMN IF NOT EXISTS session_max_age INTEGER;`,
	},
	{
		// Cross-domain passkey enrollment tokens
		// (CROSS-DOMAIN-ENROLLMENT.md). Short-lived single-use
		// tokens bind (canonical_id, target_rp_id) so an already-
		// authenticated user on one RP can enroll a passkey on
		// another RP. consumed_at is set on successful registration
		// to prevent replay; expires_at enforces the 15-minute TTL.
		// created_by carries the canonical_id of the issuer so the
		// admin-shares-a-link flow is auditable.
		name: "passkey_enrollment_tokens",
		ddl: `
CREATE TABLE IF NOT EXISTS passkey_enrollment_tokens (
    token        TEXT PRIMARY KEY,
    canonical_id TEXT NOT NULL,
    target_rp_id TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_by   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pet_user_rp
    ON passkey_enrollment_tokens (canonical_id, target_rp_id);
CREATE INDEX IF NOT EXISTS idx_pet_expires_at
    ON passkey_enrollment_tokens (expires_at);`,
	},
	{
		// Full-text search index on memories.content. Powers the
		// hybrid retrieval path (dense pgvector + sparse Postgres FTS
		// fused with Reciprocal Rank Fusion) — see the chat-turn
		// context review §5. The GIN index on to_tsvector keeps the
		// ts_rank_cd arm fast; without it the sparse scan would be a
		// sequential to_tsvector over the whole table per query.
		//
		// Mirrors the notes / wiki_pages FTS indexes already in this
		// file. 'english' config matches those for consistency.
		name: "memories_content_fts",
		ddl: `
CREATE INDEX IF NOT EXISTS idx_memories_content_fts
    ON memories USING gin(to_tsvector('english', content));`,
	},
	{
		// webauthn_user_handle decouples the WebAuthn user handle (the
		// opaque id baked into the authenticator at registration, and
		// echoed back on every assertion) from the canonical user_id, so
		// a later user_id rename never desyncs the on-device handle and
		// trips go-webauthn's "User handle and User ID do not match".
		//
		// One-time backfill: every credential present when this column is
		// first added registered its handle equal to its user_id, so seed
		// the column from user_id. The WHERE … IS NULL guard makes it run
		// exactly once; new registrations write the column explicitly so
		// it's never NULL afterward.
		name: "webauthn_user_handle",
		ddl: `
ALTER TABLE webauthn_credentials
    ADD COLUMN IF NOT EXISTS webauthn_user_handle TEXT;

UPDATE webauthn_credentials
   SET webauthn_user_handle = user_id
 WHERE webauthn_user_handle IS NULL;`,
	},
	{
		// Public-link sharing for wiki pages (covers notes too — notes
		// are personal-book pages). One row per (page, visibility); the
		// unique partial index limits a page to a single live "public"
		// share. visibility is open-ended so a future "authed-only"
		// flavor can land without another migration.
		//
		// share_key is the public URL fragment — random alphanumeric,
		// 16 chars, generated by the handler. ON DELETE CASCADE so a
		// hard-deleted page drops its shares; soft-deletes (deleted_at)
		// don't, and the public render checks deleted_at IS NULL.
		name: "wiki_page_shares",
		ddl: `
CREATE TABLE IF NOT EXISTS wiki_page_shares (
    share_key   TEXT PRIMARY KEY,
    page_id     UUID NOT NULL REFERENCES wiki_pages(id) ON DELETE CASCADE,
    visibility  TEXT NOT NULL DEFAULT 'public',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_wiki_page_shares_one_public_per_page
    ON wiki_page_shares(page_id) WHERE visibility = 'public';
CREATE INDEX IF NOT EXISTS idx_wiki_page_shares_page
    ON wiki_page_shares(page_id);`,
	},
	{
		// Instance-level key/value settings. Admin-editable config
		// that shouldn't live in the TOML file (changes at runtime
		// without a redeploy) but also shouldn't be read from the DB
		// on the hot path — callers cache the values in memory at
		// boot and refresh the cache on write. Current keys:
		//   system_prompt_base          — admin override for the
		//                                 prompt base layer; empty
		//                                 = use the file-loaded
		//                                 base.md.
		//   system_prompt_user_visible  — "true"/"false"; whether
		//                                 non-admin users may view
		//                                 the system prompt.
		name: "instance_settings",
		ddl: `
CREATE TABLE IF NOT EXISTS instance_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT
);`,
	},
	{
		// SCHEDULED-ACTIONS-SPEC Phase 1 — DB-backed recurring work.
		// scheduled_actions is one row per user-owned action binding
		// an optional shard envelope, a schedule (exactly one of
		// cron / run_at), a prompt, and a JSONB list of report
		// targets. scheduled_action_runs is the append-only ledger
		// that powers run history, the failure circuit breaker, and
		// overlap accounting — the state the config-file scheduler
		// never had.
		//
		// shard_id is ON DELETE SET NULL so deleting a shard never
		// deletes the user's schedules — but a NULL shard_id means
		// TRUSTED full-capability runs, so the admin delete handler
		// disables dependent actions first (last_status =
		// 'shard_deleted'); re-enabling without an envelope is an
		// explicit owner decision, never a side effect.
		name: "scheduled_actions",
		ddl: `
CREATE TABLE IF NOT EXISTS scheduled_actions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        TEXT NOT NULL REFERENCES users(id),
    shard_id        TEXT REFERENCES shards(id) ON DELETE SET NULL,
    name            TEXT NOT NULL,
    prompt          TEXT NOT NULL,
    cron            TEXT,
    run_at          TIMESTAMPTZ,
    timezone        TEXT NOT NULL DEFAULT 'UTC',
    enabled         BOOLEAN NOT NULL DEFAULT true,
    report_targets  JSONB NOT NULL DEFAULT '[]'::jsonb,
    delivery_policy TEXT NOT NULL DEFAULT 'always'
                    CHECK (delivery_policy IN ('always', 'on_content')),
    timeout_seconds INT NOT NULL DEFAULT 600,
    max_consecutive_failures INT NOT NULL DEFAULT 5,
    consecutive_failures     INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_run_at     TIMESTAMPTZ,
    last_status     TEXT,
    CHECK ((cron IS NULL) <> (run_at IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_sched_actions_owner
    ON scheduled_actions(owner_id) WHERE enabled;

CREATE TABLE IF NOT EXISTS scheduled_action_runs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    action_id   UUID NOT NULL REFERENCES scheduled_actions(id) ON DELETE CASCADE,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'running',
    trigger     TEXT NOT NULL DEFAULT 'cron',
    model_id    TEXT,
    output      TEXT,
    error       TEXT,
    input_tokens  INT,
    output_tokens INT,
    duration_ms   INT,
    deliveries  JSONB
);
CREATE INDEX IF NOT EXISTS idx_sched_runs_action
    ON scheduled_action_runs(action_id, started_at DESC);`,
	},
	{
		// SCHEDULED-ACTIONS-SPEC Phase 2 — per-action daily run
		// budget. 0 = unlimited; the runner counts non-manual,
		// actually-executed ledger rows in a rolling 24h window and
		// records skipped_budget once the cap is hit. Manual run-now
		// is exempt (a human at the panel is not a runaway cron).
		name: "scheduled_actions_phase2",
		ddl: `
ALTER TABLE scheduled_actions
    ADD COLUMN IF NOT EXISTS max_runs_per_day INT NOT NULL DEFAULT 0;`,
	},
	{
		// SCHEDULED-ACTIONS-SPEC Phase 3 — triggers beyond the clock.
		// trigger_kind generalizes the schedule:
		//   cron       — recurring (cron required)
		//   one_shot   — run_at required
		//   page_saved — fires on page-saved events in watch_book_id;
		//                min_interval_seconds throttles autosave
		//                bursts, and the runner refuses to watch a
		//                book containing the action's own page target
		//                (self-trigger loop prevention is also
		//                enforced per-event)
		//   webhook    — fired by POST /console/api/actions/hooks/
		//                {token}; webhook_token is the bearer secret,
		//                generated server-side at create
		//
		// The Phase 1 CHECK ((cron IS NULL) <> (run_at IS NULL)) is
		// replaced by a per-kind shape check. Existing rows backfill
		// their kind from which schedule column they carry.
		name: "scheduled_actions_phase3",
		ddl: `
ALTER TABLE scheduled_actions
    ADD COLUMN IF NOT EXISTS trigger_kind TEXT NOT NULL DEFAULT 'cron';
ALTER TABLE scheduled_actions
    ADD COLUMN IF NOT EXISTS watch_book_id UUID;
ALTER TABLE scheduled_actions
    ADD COLUMN IF NOT EXISTS watch_book_slug TEXT NOT NULL DEFAULT '';
ALTER TABLE scheduled_actions
    ADD COLUMN IF NOT EXISTS min_interval_seconds INT NOT NULL DEFAULT 60;
ALTER TABLE scheduled_actions
    ADD COLUMN IF NOT EXISTS webhook_token TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_sched_actions_webhook_token
    ON scheduled_actions(webhook_token) WHERE webhook_token IS NOT NULL;

UPDATE scheduled_actions SET trigger_kind = 'one_shot'
 WHERE run_at IS NOT NULL AND trigger_kind = 'cron';

ALTER TABLE scheduled_actions
    DROP CONSTRAINT IF EXISTS scheduled_actions_check;
DO $sched_p3$ BEGIN
    BEGIN
        ALTER TABLE scheduled_actions ADD CONSTRAINT scheduled_actions_trigger_shape CHECK (
            (trigger_kind = 'cron'       AND cron IS NOT NULL AND run_at IS NULL) OR
            (trigger_kind = 'one_shot'   AND run_at IS NOT NULL AND cron IS NULL) OR
            (trigger_kind = 'page_saved' AND cron IS NULL AND run_at IS NULL AND watch_book_id IS NOT NULL) OR
            (trigger_kind = 'webhook'    AND cron IS NULL AND run_at IS NULL AND webhook_token IS NOT NULL)
        );
    EXCEPTION WHEN duplicate_object THEN
        NULL; -- already added on a prior boot
    END;
END $sched_p3$;`,
	},
	{
		// SKILL-PACKAGES-SPEC Phase 2 — imported Agent Skills.
		// skill_packages indexes what FAMILIAR_HOME/skills/<name>/
		// holds on disk (the directory IS the artifact; the row is
		// trust state + catalog metadata). shard_skills binds
		// packages to shards — imported skills are usable ONLY
		// through a shard (spec decision: shard-only in v1).
		// signature_status ships now so signed skills slot in later
		// without a migration; until the signing convention lands
		// every import records 'unsigned' and requires explicit
		// admin approval at import time.
		name: "skill_packages",
		ddl: `
CREATE TABLE IF NOT EXISTS skill_packages (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    version     TEXT NOT NULL DEFAULT '',
    digest      TEXT NOT NULL,
    signature_status TEXT NOT NULL DEFAULT 'unsigned'
        CHECK (signature_status IN ('trusted', 'unsigned', 'unknown_signer', 'broken')),
    signer      TEXT,
    has_wasm    BOOLEAN NOT NULL DEFAULT false,
    has_scripts BOOLEAN NOT NULL DEFAULT false,
    source_url  TEXT NOT NULL DEFAULT '',
    frontmatter JSONB NOT NULL,
    -- allowed-tools mapping computed at import: which entries match
    -- registered tool names and which are not applicable here.
    tools_matched   JSONB NOT NULL DEFAULT '[]'::jsonb,
    tools_unmatched JSONB NOT NULL DEFAULT '[]'::jsonb,
    imported_by TEXT NOT NULL REFERENCES users(id),
    imported_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disabled_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS shard_skills (
    shard_id  TEXT NOT NULL REFERENCES shards(id) ON DELETE CASCADE,
    skill_id  UUID NOT NULL REFERENCES skill_packages(id) ON DELETE CASCADE,
    bound_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (shard_id, skill_id)
);`,
	},
	{
		// Chat folders ("projects") — a flat per-user grouping for
		// conversations. Distinct from the wiki/notes hierarchy
		// because conversations are leaves only and the folder will
		// eventually become a shards scope. ON DELETE SET NULL keeps
		// conversations alive when their folder is deleted — they
		// just fall back into "uncategorized".
		name: "chat_folders",
		ddl: `
CREATE TABLE IF NOT EXISTS chat_folders (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    sort_order  INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_chat_folders_user
    ON chat_folders(user_id, sort_order, name);

ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS folder_id UUID REFERENCES chat_folders(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_conversations_folder
    ON conversations(folder_id) WHERE folder_id IS NOT NULL;`,
	},
	{
		// Page media (MEDIA-DIAGRAMS Phase 1): metadata for images
		// attached to wiki/notes pages. Bytes live on the filesystem
		// ([media] dir); the row is the queryable index + authz
		// anchor (page → book → membership). ON DELETE CASCADE rows
		// die with their page; the store's orphan sweep reaps the
		// files afterward.
		name: "page_media",
		ddl: `
CREATE TABLE IF NOT EXISTS page_media (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    page_id       UUID NOT NULL REFERENCES wiki_pages(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL REFERENCES users(id),
    filename      TEXT NOT NULL,
    content_type  TEXT NOT NULL,
    size_bytes    BIGINT NOT NULL,
    object_key    TEXT NOT NULL,
    thumb_key     TEXT NOT NULL DEFAULT '',
    width         INT NOT NULL DEFAULT 0,
    height        INT NOT NULL DEFAULT 0,
    alt_text      TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_page_media_page ON page_media(page_id);`,
	},
	{
		// Sliding sessions: ttl_seconds records the mint-time TTL so
		// Validate can renew an actively-used session (expiry becomes
		// an IDLE window, which is what kiosk devices need). NULL =
		// legacy rows; Validate derives expires_at - created_at and
		// backfills on first renewal.
		name: "admin_sessions_sliding",
		ddl: `
ALTER TABLE admin_sessions
    ADD COLUMN IF NOT EXISTS ttl_seconds INTEGER;`,
	},
	{
		// Scheduled-action envelope mode: user (trusted, "run as
		// you"), ephemeral (prompt-only), or shard. Pre-envelope rows
		// encoded the choice in shard_id alone — backfill those to
		// 'shard'. Value validity is enforced in actions.Validate;
		// no CHECK here so a future mode is a code change, not a
		// migration.
		name: "scheduled_actions_envelope",
		ddl: `
ALTER TABLE scheduled_actions
    ADD COLUMN IF NOT EXISTS envelope TEXT NOT NULL DEFAULT 'user';
UPDATE scheduled_actions SET envelope = 'shard'
 WHERE shard_id IS NOT NULL AND envelope = 'user';`,
	},
	{
		// Durable external identity for conversations created outside
		// the workspace UI. Slack DMs/threads and scheduled-action
		// deliveries map to a stable conversation row through this key
		// so a user's Slack reply hydrates the same history the bot
		// posted into (SLACK-CONTEXT). NULL for workspace-native rows;
		// the partial UNIQUE index lets many NULLs coexist while
		// guaranteeing one row per external key.
		//   DM:     slack:dm:<userID>            (stable per user)
		//   Thread: slack:thread:<channel>:<ts>  (stable per thread)
		name: "conversations_external_key",
		ddl: `
ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS external_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_conversations_external_key
    ON conversations(external_key) WHERE external_key IS NOT NULL;`,
	},
	{
		// Tenant-leak backstop for the relationships graph. memories
		// already CHECKs user_id non-empty (memories_user_id_constraints);
		// relationships did not, so an extract-path write with an
		// unresolved identity could land a NULL/empty user_id edge that
		// pgvector-style reads (user_id IS NULL OR user_id = $n) expose to
		// every tenant. The pipeline now skips empty-user extract at the
		// source (summarize.go), and this is the DB backstop.
		//
		// NOT VALID so the constraint binds all NEW inserts/updates while
		// leaving any pre-existing NULL rows (single-user-era data) in
		// place — adding it never fails a boot the way a bare SET NOT NULL
		// would. A later migration can VALIDATE once a deploy is known
		// clean. DROP-then-ADD keeps it idempotent across re-runs.
		name: "relationships_user_id_nonempty",
		ddl: `
ALTER TABLE relationships DROP CONSTRAINT IF EXISTS relationships_user_id_nonempty;
ALTER TABLE relationships ADD CONSTRAINT relationships_user_id_nonempty
    CHECK (user_id IS NOT NULL AND user_id <> '') NOT VALID;`,
	},
	{
		// Web Push subscriptions (PWA notifications). One row per
		// (user, device endpoint). endpoint is the push service URL the
		// browser handed us; p256dh + auth are the subscription's
		// encryption keys. UNIQUE(endpoint) so a re-subscribe upserts
		// rather than duplicating; the user_id index drives the
		// per-user fan-out when a scheduled action delivers a push.
		// Dead endpoints (404/410 from the push service) are pruned by
		// the sender.
		name: "push_subscriptions",
		ddl: `
CREATE TABLE IF NOT EXISTS push_subscriptions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    endpoint    TEXT NOT NULL UNIQUE,
    p256dh      TEXT NOT NULL,
    auth        TEXT NOT NULL,
    user_agent  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_push_subscriptions_user
    ON push_subscriptions(user_id);`,
	},
	{
		// Messages within a single agentic turn (user → assistant
		// w/ tool_calls → tool result → ...) get inserted in one
		// AppendIntermediateMessages transaction, so they all share
		// the same created_at NOW(). With identical timestamps the
		// ORDER BY created_at, id falls back to id — a random UUID —
		// which scrambles the turn sequence on hydration. A tool
		// result can sort before the assistant tool_call that
		// produced it, and Cohere2's Jinja template then crashes
		// (tool_call_id referenced before tool_ids_seen is built).
		//
		// A BIGSERIAL seq column gives a globally monotonic insertion
		// order independent of timestamp collisions. Backfill existing
		// rows by created_at then id so historical conversations keep
		// a stable (if approximate) order.
		name: "messages_seq_ordering",
		ddl: `
ALTER TABLE messages ADD COLUMN IF NOT EXISTS seq BIGSERIAL;
CREATE INDEX IF NOT EXISTS idx_messages_conversation_seq
    ON messages(conversation_id, seq);`,
	},
	{
		// Repair inverted supersede pointers written by the sleep
		// cycle's drift dedup. Everywhere in the system "superseded"
		// means "another row points at me" — retrieval hides
		// pointed-at rows, and the extraction pipeline correctly has
		// the NEW fact point at the OLD one. The sleep dedup had it
		// backwards (older pointed at newer), which hid the NEWER
		// member of every near-duplicate pair it touched.
		//
		// Inverted rows are precisely identifiable: a legitimate
		// pointer is always newer than its target (a new fact is
		// created pointing at an existing row), so any pointer OLDER
		// than its target came from the inverted dedup. Clearing it
		// un-hides the newer fact; the fixed dedup then re-collapses
		// the pair in the correct direction on its next pass.
		name: "repair_inverted_supersedes",
		ddl: `
UPDATE memories p
   SET supersedes = NULL,
       updated_at = NOW()
  FROM memories x
 WHERE p.supersedes = x.id
   AND p.created_at < x.created_at;`,
	},
	{
		// USER-SKILLS-SPEC Phase A — per-user skill ownership.
		// owner_id NULL keeps today's semantics (instance-wide,
		// admin-managed); a set owner_id marks a private user skill
		// living under <skills.dir>/users/<owner>/. origin records
		// provenance ('authored' in the workspace editor vs
		// 'imported' zip/URL) — the Phase B trust model treats them
		// differently. chat_enabled ships now but stays dormant until
		// Phase B (trusted-path exposure), same convention as
		// signature_status. Name uniqueness splits per scope: one
		// namespace for instance skills, one per owner.
		name: "skill_packages_ownership",
		ddl: `
ALTER TABLE skill_packages
  ADD COLUMN IF NOT EXISTS owner_id TEXT REFERENCES users(id) ON DELETE CASCADE;

ALTER TABLE skill_packages
  ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'imported'
    CHECK (origin IN ('authored', 'imported'));

ALTER TABLE skill_packages
  ADD COLUMN IF NOT EXISTS chat_enabled BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE skill_packages DROP CONSTRAINT IF EXISTS skill_packages_name_key;

CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_packages_instance_name
    ON skill_packages(name) WHERE owner_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_packages_owner_name
    ON skill_packages(owner_id, name) WHERE owner_id IS NOT NULL;`,
	},
	{
		// Built-in skills — first-party packages embedded in the
		// gateway binary and synced into the instance library at boot
		// (skillpkg.SyncBuiltins; RESEARCH-SKILL-SPEC §5). origin
		// gains 'builtin' as a third provenance, and imported_by goes
		// nullable: a builtin ships with the deploy, no importing
		// user exists (NULL = "shipped with gateway"). The original
		// origin CHECK was an inline column constraint with an auto-
		// generated name, so the DO block drops whatever CHECK
		// currently governs the column (catalog lookup, not a guessed
		// name) before adding the named replacement — migrations run
		// on every boot, so both halves must be re-runnable.
		name: "skillpkg_builtin_origin",
		ddl: `
DO $spk_builtin$
DECLARE
    con TEXT;
BEGIN
    FOR con IN
        SELECT c.conname
          FROM pg_constraint c
          JOIN pg_attribute a
            ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
         WHERE c.conrelid = 'skill_packages'::regclass
           AND c.contype = 'c'
           AND a.attname = 'origin'
           AND c.conname <> 'skill_packages_origin_check_v2'
    LOOP
        EXECUTE format('ALTER TABLE skill_packages DROP CONSTRAINT %I', con);
    END LOOP;
    BEGIN
        ALTER TABLE skill_packages ADD CONSTRAINT skill_packages_origin_check_v2
            CHECK (origin IN ('authored', 'imported', 'builtin'));
    EXCEPTION WHEN duplicate_object THEN
        NULL; -- already added on a prior boot
    END;
END $spk_builtin$;

ALTER TABLE skill_packages ALTER COLUMN imported_by DROP NOT NULL;`,
	},
	{
		// research_runs backs autonomous deep-research runs
		// (RESEARCH-SKILL-SPEC §6.7): the skill drives workers →
		// gap-fill → synthesis in the background, and this row is the
		// source of truth the workspace's progress card restores from
		// (survives tab-away) and the completion delivery reads.
		name: "research_runs",
		ddl: `
CREATE TABLE IF NOT EXISTS research_runs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    conversation_id   TEXT NOT NULL,
    topic             TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'researching'
                          CHECK (status IN ('researching','synthesizing','done','failed')),
    round             INT  NOT NULL DEFAULT 1,
    workers_total     INT  NOT NULL DEFAULT 0,
    workers_done      INT  NOT NULL DEFAULT 0,
    evidence_page_slug TEXT NOT NULL DEFAULT '',
    note_book_slug    TEXT NOT NULL DEFAULT '',
    note_page_slug    TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- The progress card looks up "the active run for this conversation";
-- terminal runs (done/failed) are excluded by the query, so this index
-- keeps the active lookup and the per-user history list fast.
CREATE INDEX IF NOT EXISTS idx_research_runs_conv
    ON research_runs (conversation_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_research_runs_user
    ON research_runs (user_id, updated_at DESC);
-- At most ONE active (non-terminal) run per conversation. This is the
-- atomic backstop for the kickoff guard's check-then-create window: a
-- concurrent second kickoff hits this and Create returns
-- ErrActiveRunExists instead of forking a duplicate run.
CREATE UNIQUE INDEX IF NOT EXISTS uq_research_runs_active
    ON research_runs (conversation_id)
    WHERE status IN ('researching','synthesizing');`,
	},
	{
		// Live progress stats for the research progress card (§6.7): a
		// running token + page-read tally, incremented per worker as it
		// finishes, and the evidence book slug so the workspace can open
		// the hidden evidence page in the right pane and watch it fill.
		// Separate ALTER (not folded into the CREATE) because
		// research_runs already shipped — an existing table won't pick
		// up new columns from a re-run CREATE ... IF NOT EXISTS.
		name: "research_runs_stats",
		ddl: `
ALTER TABLE research_runs
    ADD COLUMN IF NOT EXISTS tokens             BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS pages_read         INT    NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS evidence_book_slug TEXT   NOT NULL DEFAULT '';`,
	},
	{
		// Per-worker roster for the in-chat research card: a JSONB array
		// of {"question","state"} (state = queued|active|done|failed),
		// keyed by stable task index so gap-fill's round-local worker
		// renumbering doesn't scramble it. Separate ALTER, same reason as
		// research_runs_stats.
		name: "research_runs_workers",
		ddl: `
ALTER TABLE research_runs
    ADD COLUMN IF NOT EXISTS workers JSONB NOT NULL DEFAULT '[]'::jsonb;`,
	},
}

// migrateLockKey is the pg_advisory_lock key that serializes Migrate
// across every process sharing one database. Arbitrary but must never
// change: two gateway builds with different keys would happily migrate
// concurrently again.
const migrateLockKey = 0x46414D494C494152 // "FAMILIAR"

// Migrate runs every registered migration in order against the pool.
// Each statement is expected to be idempotent — the current set all use
// IF NOT EXISTS / ON CONFLICT so a second call is a no-op.
//
// The whole run holds a session-level advisory lock so concurrent
// boots against one database (parallel E2E gateways, `go test ./...`
// integration packages) queue up instead of deadlocking each other's
// ALTER TABLEs. The lock lives on a dedicated connection and releases
// on unlock or connection close, whichever comes first.
func Migrate(ctx context.Context, p *Pool) error {
	if p == nil || p.DB == nil {
		return fmt.Errorf("db: migrate on nil pool")
	}
	conn, err := p.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db: migrate acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, int64(migrateLockKey)); err != nil {
		return fmt.Errorf("db: migrate advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(migrateLockKey))
	}()

	for _, m := range migrations {
		if _, err := conn.ExecContext(ctx, m.ddl); err != nil {
			return fmt.Errorf("db: migration %q: %w", m.name, err)
		}
	}
	return nil
}
