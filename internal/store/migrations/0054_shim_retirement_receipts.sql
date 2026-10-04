-- Nonsecret idempotency/audit survives payload cleanup. No retention DELETE
-- targets this table. The store adapter owns exact typed JSON binding.
CREATE TABLE session_shim_retirements (
    operation_id TEXT PRIMARY KEY NOT NULL CHECK(length(operation_id)>0),
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    request_digest TEXT NOT NULL CHECK(length(request_digest)>0),
    revision TEXT NOT NULL CHECK(length(revision)>0),
    phase TEXT NOT NULL CHECK(phase IN ('intent_recorded','retirement_committed','descriptor_cleanup_complete','state_reconciled')),
    record_json TEXT NOT NULL CHECK(json_valid(record_json) AND length(CAST(record_json AS BLOB))<=262144),
    retired_at TEXT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_session_shim_retirements_session
ON session_shim_retirements(session_id,retired_at,operation_id);

-- Tether currently leaves foreign_keys OFF. Preserve the promised restriction
-- under that connection configuration too; audit-bearing sessions cannot be
-- erased by a caller bypassing the typed store adapter.
CREATE TRIGGER session_retirement_preserve_audit
BEFORE DELETE ON sessions
WHEN EXISTS(SELECT 1 FROM session_shim_retirements WHERE session_id=OLD.id)
BEGIN
    SELECT RAISE(ABORT,'session retirement audit retained');
END;
