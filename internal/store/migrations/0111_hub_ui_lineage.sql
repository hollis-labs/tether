-- Server-owned explicit hub relaunch evidence; no historical inference.
-- UI ancestry only, never native custody or permission to launch.
CREATE TABLE hub_relaunch_provenance (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 source_session_id TEXT NOT NULL REFERENCES sessions(id),
 logical_agent_id TEXT NOT NULL CHECK(logical_agent_id<>''),
 source_worker_id TEXT NOT NULL CHECK(source_worker_id=''),
 worker_id TEXT NOT NULL CHECK(worker_id=''),
 created_at TEXT NOT NULL,
 CHECK(session_id<>source_session_id)
);
CREATE TRIGGER hub_relaunch_provenance_immutable BEFORE UPDATE ON hub_relaunch_provenance
BEGIN SELECT RAISE(ABORT,'hub relaunch provenance is immutable'); END;
