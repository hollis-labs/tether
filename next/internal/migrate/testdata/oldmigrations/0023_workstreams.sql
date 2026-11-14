-- 0023_workstreams.sql
--
-- S1 of sprint SP-20260912-0001 (CW-20260912-0059), design record
-- CW-20260912-0023.
--
-- A workstream is the durable container for a unit of work that outlives any
-- one session:
--
--     workstream --< session (fresh -> compact -> resume -> fork)
--
-- WHY THIS EXISTS. `sessions.intent` (migration 0019) already includes
-- 'compact', and a compaction creates a NEW session row carrying
-- parent_session_id. So anything attached to a session_id is orphaned by the
-- exact compaction it was meant to survive. Attaching to the workstream
-- instead, and inheriting workstream_id structurally down the lineage, is what
-- makes the attachment span fresh -> compact -> resume -> fork.
--
-- workflow_id is first-class here rather than reached for through
-- session_groups. session_groups has carried a workflow_id since migration
-- 0009 with zero rows ever written; it was meant for agent self-organisation
-- that was never exercised. Per Chrispian, workflow_id is elevated onto the
-- workstream and session_groups is left untouched -- NOT retired here. If it
-- is still empty later that is its own answer.
--
-- FK declarations follow ADR-0008 (deferred enforcement) -- documentation
-- only; PRAGMA foreign_keys stays off, matching every migration since 0015.

CREATE TABLE workstreams (
    id          TEXT PRIMARY KEY,
    name        TEXT,
    -- workflow_id is free-form and carries no FK: the workflow it names is
    -- owned by whatever orchestrator created it (Torque, Nanite, a caller
    -- outside the portfolio), never by Tether. Tether records the
    -- correlation and does not resolve it -- the same boundary ADR 0041
    -- draws for the registry, which holds identity and a callback and never
    -- operational content.
    --
    -- Consequence: a workflow_id may name a workflow that never existed, or
    -- one since deleted, and nothing here will notice. A reader resolving it
    -- must treat "not found" as ordinary, not as corruption. Filtering
    -- workstreams by workflow_id is therefore a lookup, never a validation.
    workflow_id TEXT,
    status      TEXT NOT NULL DEFAULT 'active'
        CHECK(status IN ('active', 'closed')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_workstreams_workflow ON workstreams(workflow_id);
CREATE INDEX IF NOT EXISTS idx_workstreams_status ON workstreams(status);

-- Nullable on purpose. A session is not required to belong to a workstream --
-- "a one-off session need not manufacture a permanent record merely by
-- existing" is the same position leaseActorBinding takes about durable actors.
-- One is created on demand when something actually needs a container.
ALTER TABLE sessions ADD COLUMN workstream_id TEXT REFERENCES workstreams(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_sessions_workstream ON sessions(workstream_id);
