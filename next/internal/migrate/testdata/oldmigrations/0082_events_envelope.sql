-- CW-20261016-0136 (X1c): persist the v1.1 envelope blocks on the events table.
-- Columns follow the event stream contract (runtimeevents scope.md / blocks.go) and
-- the retention schema agreed with the retention lane (CW-20261016-0146):
--   scope_*           the 17 closed scope dimensions (scope.md); the legacy `scope`
--                     column is the producer's scope STRING and is left alone
--   class, corr, actor blocks, content_type, truncated
--   recorded_at/epoch assigned by the log, never by producers
--   retention_class / retention_policy_id / payload_digest owned by retention
-- Every column is nullable and there is NO backfill (a mass UPDATE on the ~1M-row
-- hub table would hold the single DB connection): legacy rows read as NULL, and a
-- NULL retention_class / retention_policy_id means "legacy hold", never "derive at
-- trim time". Forward-only; an older binary ignores the columns. The partial
-- indexes below are empty on existing rows (every value is NULL) but each costs one
-- table scan at the first start.
ALTER TABLE events ADD COLUMN scope_environment_id TEXT;
ALTER TABLE events ADD COLUMN scope_authority TEXT;
ALTER TABLE events ADD COLUMN scope_worker_id TEXT;
ALTER TABLE events ADD COLUMN scope_host TEXT;
ALTER TABLE events ADD COLUMN scope_project_id TEXT;
ALTER TABLE events ADD COLUMN scope_workstream_id TEXT;
ALTER TABLE events ADD COLUMN scope_epic_id TEXT;
ALTER TABLE events ADD COLUMN scope_sprint_id TEXT;
ALTER TABLE events ADD COLUMN scope_plan_id TEXT;
ALTER TABLE events ADD COLUMN scope_phase_id TEXT;
ALTER TABLE events ADD COLUMN scope_task_id TEXT;
ALTER TABLE events ADD COLUMN scope_run_id TEXT;
ALTER TABLE events ADD COLUMN scope_attempt INTEGER;
ALTER TABLE events ADD COLUMN scope_launch_id TEXT;
ALTER TABLE events ADD COLUMN scope_agent_urn TEXT;
ALTER TABLE events ADD COLUMN scope_logical_agent_id TEXT;
ALTER TABLE events ADD COLUMN scope_team_run_id TEXT;

ALTER TABLE events ADD COLUMN category TEXT;
ALTER TABLE events ADD COLUMN severity TEXT;
ALTER TABLE events ADD COLUMN sensitivity TEXT;
ALTER TABLE events ADD COLUMN pii INTEGER;
ALTER TABLE events ADD COLUMN visibility TEXT;
ALTER TABLE events ADD COLUMN payload_schema TEXT;
ALTER TABLE events ADD COLUMN sanitized INTEGER;
ALTER TABLE events ADD COLUMN redactions INTEGER;
ALTER TABLE events ADD COLUMN labels_json TEXT;
ALTER TABLE events ADD COLUMN content_type TEXT;
ALTER TABLE events ADD COLUMN truncated INTEGER;

ALTER TABLE events ADD COLUMN trace_id TEXT;
ALTER TABLE events ADD COLUMN span_id TEXT;
ALTER TABLE events ADD COLUMN causation_id TEXT;
ALTER TABLE events ADD COLUMN correlation_id TEXT;
ALTER TABLE events ADD COLUMN thread_id TEXT;
ALTER TABLE events ADD COLUMN idempotency_key TEXT;
ALTER TABLE events ADD COLUMN depth INTEGER;
ALTER TABLE events ADD COLUMN actor_kind TEXT;
ALTER TABLE events ADD COLUMN actor_urn TEXT;
ALTER TABLE events ADD COLUMN actor_verified TEXT;
ALTER TABLE events ADD COLUMN on_behalf_of TEXT;

ALTER TABLE events ADD COLUMN recorded_at TEXT;
ALTER TABLE events ADD COLUMN epoch INTEGER;

ALTER TABLE events ADD COLUMN retention_class TEXT
  CHECK (retention_class IS NULL OR retention_class IN ('R-audit','R-state','R-activity','R-debug'));
ALTER TABLE events ADD COLUMN retention_policy_id TEXT;
-- Written only by the retention pass that removes the exact stored payload (the
-- same transaction sets payload_json to NULL): sha256:<64 lowercase hex>, or
-- 'withheld' when the event is marked pii. Never set at insert.
ALTER TABLE events ADD COLUMN payload_digest TEXT
  CHECK (payload_digest IS NULL OR payload_digest = 'withheld'
         OR (length(payload_digest) = 71 AND substr(payload_digest, 1, 7) = 'sha256:'
             AND substr(payload_digest, 8) NOT GLOB '*[^0-9a-f]*'))
  CHECK (payload_digest IS NULL OR payload_json IS NULL);

CREATE INDEX idx_events_scope_project ON events(scope_project_id, id) WHERE scope_project_id IS NOT NULL;
CREATE INDEX idx_events_scope_task ON events(scope_task_id, id) WHERE scope_task_id IS NOT NULL;
CREATE INDEX idx_events_scope_logical_agent ON events(scope_logical_agent_id, id) WHERE scope_logical_agent_id IS NOT NULL;
CREATE INDEX idx_events_retention_class ON events(retention_class, id) WHERE retention_class IS NOT NULL;
