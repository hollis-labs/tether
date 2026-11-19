-- UI-only lineage evidence. Neither table grants native resume or custody.
-- No historical backfill: intent/state alone cannot prove an acknowledged launch.
CREATE TABLE session_relaunch_provenance (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 source_session_id TEXT NOT NULL REFERENCES sessions(id),
 logical_agent_id TEXT NOT NULL CHECK(logical_agent_id<>''),
 source_worker_id TEXT NOT NULL,
 worker_id TEXT NOT NULL CHECK(worker_id<>''),
 created_at TEXT NOT NULL,
 CHECK(session_id<>source_session_id)
);
CREATE TRIGGER relaunch_provenance_immutable BEFORE UPDATE ON session_relaunch_provenance
BEGIN SELECT RAISE(ABORT,'relaunch provenance is immutable'); END;
CREATE TABLE worker_lineage_receipts (
 session_id TEXT PRIMARY KEY REFERENCES session_relaunch_provenance(session_id) ON DELETE CASCADE,
 source_session_id TEXT NOT NULL,
 logical_agent_id TEXT NOT NULL,
 source_worker_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 command_id TEXT NOT NULL,
 acknowledged_at TEXT NOT NULL
);
CREATE TRIGGER worker_lineage_immutable BEFORE UPDATE ON worker_lineage_receipts
BEGIN SELECT RAISE(ABORT,'worker lineage receipt is immutable'); END;
