-- Native fabric persistence (ADR 0054 and ADR 0060).
-- No import, backfill or runtime activation. References are checked by the
-- repository transaction because existing connections do not enforce FKs.
CREATE TABLE fabric_definition_revisions (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_definition_artifacts (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_actors (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_agents (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_sessions (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_instances (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_binding_heads (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_binding_history (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_admissions (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_migration_candidates (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_import_receipts (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE TABLE fabric_legacy_refs (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE INDEX fabric_sessions_agent ON fabric_sessions(json_extract(record_json, '$.agent_urn'));
CREATE INDEX fabric_instances_session ON fabric_instances(json_extract(record_json, '$.session_urn'));
CREATE INDEX fabric_binding_history_agent ON fabric_binding_history(json_extract(record_json, '$.lease.agent_urn'));
CREATE TABLE fabric_outbox (
    cursor INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE CHECK (length(event_id) > 0),
    aggregate_urn TEXT NOT NULL CHECK (length(aggregate_urn) > 0),
    event_type TEXT NOT NULL CHECK (length(event_type) > 0),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    created_at TEXT NOT NULL,
    delivered_at TEXT
);
