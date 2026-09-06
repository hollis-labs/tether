-- 0019_canonical_session_identity.sql
--
-- Messaging vNext T02 (CW-20260906-0034, plan CW-20260906-0023). Adds three
-- purely additive pieces of schema; nothing existing changes meaning:
--
--   1. Canonical-session bootstrap fields on `sessions`. Mirrors the shape
--      of agentkit's (unreleased-as-of-this-migration) SessionBootstrap
--      vocabulary -- Intent, ParentSessionID, Publication -- without
--      requiring agentkit as a schema dependency. Every existing row
--      backfills to intent='fresh' (the historical default: nothing before
--      this migration recorded resume/fresh lineage) and
--      publication='private-local' (nothing before this migration was ever
--      explicitly published).
--
--   2. session_provider_mappings: a real per-session, per-provider
--      native-ID table. Replaces, going forward, the single
--      logical_agents.claude_session_id slot -- confirmed by inventory
--      (planning/docs/messaging-vnext/T01-compatibility-contract.md §2.12
--      and the T02 session-lifecycle research) to be shared across every
--      provider kind that reports a session id (not just Claude, despite
--      the name), single-valued per logical agent rather than per session,
--      and never actually read back in production. That column is left
--      untouched here -- still written by the existing code path -- and
--      formally superseded once T02's Go code starts writing this table
--      as the authoritative mapping.
--
--   3. runtime_bindings: a leased live host/attempt binding against an
--      arbitrary msg:// target URN (a session URN, e.g.
--      msg://session/<authority>/<session_id>, or a durable actor's
--      registry URN, e.g. msg://agent/<authority>/<id>). generation is a
--      monotonic per-target-URN counter so exactly one binding is ever
--      "current" for a target -- architecture: "one active home-host
--      binding per concrete session, with generation fencing during
--      replacement/recovery." Reuses the msg:// address space rather than
--      inventing a separate binding-key shape, consistent with the T01
--      finding that registry URNs and messaging URNs already share one
--      identifier space (§2.2).
--
-- FK declarations follow ADR-0008 (deferred enforcement) -- documentation
-- only; PRAGMA foreign_keys stays off, matching every migration since 0015.

ALTER TABLE sessions ADD COLUMN parent_session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL;
ALTER TABLE sessions ADD COLUMN intent TEXT NOT NULL DEFAULT 'fresh'
    CHECK(intent IN ('preassigned', 'resume', 'compact', 'fresh', 'fork'));
ALTER TABLE sessions ADD COLUMN publication TEXT NOT NULL DEFAULT 'private-local'
    CHECK(publication IN ('private-local', 'published-local', 'tether-hosted'));

CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);

CREATE TABLE session_provider_mappings (
    session_id        TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    owner             TEXT NOT NULL,
    provider          TEXT NOT NULL,
    native_session_id TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (session_id, owner, provider)
);

CREATE INDEX IF NOT EXISTS idx_session_provider_mappings_native
    ON session_provider_mappings(provider, native_session_id);

CREATE TABLE runtime_bindings (
    id               TEXT PRIMARY KEY,
    target_urn       TEXT NOT NULL,
    session_id       TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    host_id          TEXT NOT NULL,
    attempt_id       TEXT NOT NULL,
    generation       INTEGER NOT NULL,
    capabilities_json TEXT,
    visibility       TEXT NOT NULL DEFAULT 'private-local'
        CHECK(visibility IN ('private-local', 'published-local', 'tether-hosted')),
    leased_at        TEXT NOT NULL,
    lease_expires_at TEXT,
    revoked_at       TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_runtime_bindings_target_generation
    ON runtime_bindings(target_urn, generation DESC);
CREATE INDEX IF NOT EXISTS idx_runtime_bindings_session
    ON runtime_bindings(session_id);
