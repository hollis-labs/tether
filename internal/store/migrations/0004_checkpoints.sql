-- 0004_checkpoints.sql
--
-- Scope: T-v002-s03-03.
--
-- Durable continuity record for a LogicalAgent. v0.0.2 lands the
-- storage primitive only — no create-from-session or resume flows
-- (those are Sprint v003-04). Payload columns are deliberately TEXT /
-- free-form JSON; v0.0.3 will pin the shape.
--
-- FKs are declared but enforcement stays off (PRAGMA foreign_keys
-- unchanged per ADR 0003). The declarations are documentation for the
-- relationship between checkpoints, logical_agents, and sessions.
CREATE TABLE checkpoints (
    id                   TEXT PRIMARY KEY,
    logical_agent_id     TEXT NOT NULL REFERENCES logical_agents(id),
    task_id              TEXT,
    workflow_id          TEXT,
    status               TEXT,
    completed_work       TEXT,
    pending_work         TEXT,
    key_decisions        TEXT,
    referenced_artifacts TEXT,
    summary              TEXT,
    next_recommendation  TEXT,
    created_at           TEXT NOT NULL,
    source_session_id    TEXT REFERENCES sessions(id)
);

CREATE INDEX IF NOT EXISTS idx_checkpoints_logical_agent
    ON checkpoints(logical_agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_checkpoints_workflow
    ON checkpoints(workflow_id, created_at);
