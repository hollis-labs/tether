-- CW-20261016-0146: immutable resolved policy epochs and explicitly activated
-- kind-to-class admission. No mappings/default activation and no event backfill.
CREATE TABLE event_retention_policies (
    id TEXT PRIMARY KEY,
    policy_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TRIGGER event_retention_policies_immutable
BEFORE UPDATE ON event_retention_policies
BEGIN
    SELECT RAISE(ABORT, 'retention policy snapshots are immutable');
END;
CREATE TABLE event_retention_admission (
    kind TEXT PRIMARY KEY,
    retention_class TEXT NOT NULL CHECK (retention_class IN ('R-audit','R-state','R-activity','R-debug')),
    policy_id TEXT NOT NULL REFERENCES event_retention_policies(id)
);
