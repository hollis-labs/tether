-- Private protocol state includes the durable event inbox and partial bytes in
-- the same CAS record. It is not registry metadata or a public API payload.
CREATE TABLE codex_shim_protocol (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES session_shims(session_id),
    shim_key TEXT NOT NULL,
    protocol TEXT NOT NULL,
    revision TEXT NOT NULL,
    state_json TEXT NOT NULL
);
