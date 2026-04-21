-- 0009_session_groups.sql
--
-- Scope: T-v003-s05-03 (mailbox semantics sprint).
--
-- Workflow-scoped session groups for the multiplexor pattern. A group
-- ties a primary session to its siblings under a shared identity so
-- broker envelopes can be scoped and sessions can be queried together.
--
-- sessions.session_group_id is nullable — sessions not participating in
-- a multiplexor workflow leave it NULL. One session may belong to at
-- most one group (single-group per session rule per ADR 0018 scope).

CREATE TABLE session_groups (
    id         TEXT PRIMARY KEY,
    name       TEXT,
    workflow_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

ALTER TABLE sessions ADD COLUMN session_group_id TEXT REFERENCES session_groups(id);

CREATE INDEX IF NOT EXISTS idx_sessions_group
    ON sessions(session_group_id);
