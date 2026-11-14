-- Nonsecret bounded sweep progress; neither TTL nor payload cleanup deletes it.
CREATE TABLE session_shim_retention_cursors (
    cursor_id TEXT PRIMARY KEY NOT NULL,
    request_digest TEXT NOT NULL,
    revision TEXT NOT NULL,
    record_json TEXT NOT NULL CHECK(json_valid(record_json) AND length(CAST(record_json AS BLOB))<=262144),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
