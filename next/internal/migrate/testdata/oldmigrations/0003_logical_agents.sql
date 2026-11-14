-- 0003_logical_agents.sql
--
-- Scope: T-v002-s03-02.
--
-- 1. Create logical_agents with the full LogicalAgent column set from
--    context-pack §02. Policy/hot-cold/checkpoint fields are nullable
--    TEXT — v0.0.2 catalog can't express them yet (ADR 0003).
-- 2. Backfill logical_agents from distinct sessions.agent_id values so
--    the rebuilt sessions table's FK resolves for historical rows.
-- 3. Rebuild sessions with logical_agent_id (FK, NOT NULL) replacing
--    agent_id. Preserve data by copying agent_id → logical_agent_id.
--    Drop old sessions, rename new one into place, recreate indexes.
--
-- No back-compat shim for agent_id: pre-launch, catalog agent id ≡
-- logical agent id today (ADR 0003). Entire file runs inside the
-- migrator's per-file transaction.

CREATE TABLE logical_agents (
    id                TEXT PRIMARY KEY,
    role              TEXT,
    name              TEXT,
    responsibilities  TEXT,
    capabilities      TEXT,
    memory_scopes     TEXT,
    policies_json     TEXT,
    permitted_tools   TEXT,
    escalation_rules  TEXT,
    checkpoint_policy TEXT,
    hot_cold_policy   TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

INSERT OR IGNORE INTO logical_agents (id, name, created_at, updated_at)
SELECT DISTINCT
    agent_id,
    agent_id,
    strftime('%Y-%m-%dT%H:%M:%SZ', 'now'),
    strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM sessions
WHERE agent_id IS NOT NULL AND agent_id <> '';

CREATE TABLE sessions_new (
    id                TEXT PRIMARY KEY,
    launch_id         TEXT NOT NULL,
    project_id        TEXT NOT NULL,
    logical_agent_id  TEXT NOT NULL REFERENCES logical_agents(id),
    provider_id       TEXT NOT NULL,
    workspace         TEXT NOT NULL,
    state             TEXT NOT NULL,
    pid               INTEGER,
    exit_code         INTEGER,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    ended_at          TEXT
);

INSERT INTO sessions_new (
    id, launch_id, project_id, logical_agent_id, provider_id, workspace,
    state, pid, exit_code, created_at, updated_at, ended_at
)
SELECT
    id, launch_id, project_id, agent_id, provider_id, workspace,
    state, pid, exit_code, created_at, updated_at, ended_at
FROM sessions;

DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;

CREATE INDEX IF NOT EXISTS idx_sessions_state ON sessions(state);
CREATE INDEX IF NOT EXISTS idx_sessions_logical_agent ON sessions(logical_agent_id);
