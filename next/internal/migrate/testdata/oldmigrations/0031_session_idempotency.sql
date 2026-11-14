-- 0031_session_idempotency.sql
--
-- CW-20260930-0229: idempotent session create and resume.
--
-- One row per idempotency key, binding it permanently to the session its first
-- request created. A retry with the same key and the same request digest
-- replays that session; a different digest is a conflict. Only the digest is
-- stored, never the request itself, because a request may carry prompts and
-- injected files.
--
-- Keys are one global space. Tether has no authenticated caller identity
-- (CW-20260918-0037), so the key is not a security boundary and callers are
-- expected to prefix their own (for example `hadron/<run>/<node>/<iteration>`).
--
-- resolved_parent_session_id records, for a resume, the parent the resolver
-- chose. It is audit only and deliberately not part of the digest, so a retry
-- after the new session has written its own checkpoint still replays.
--
-- The foreign key is declared for intent; enforcement is off (ADR-0008).

CREATE TABLE IF NOT EXISTS session_idempotency (
    key                        TEXT PRIMARY KEY,
    operation                  TEXT NOT NULL,
    request_digest             TEXT NOT NULL,
    session_id                 TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    resolved_parent_session_id TEXT,
    created_at                 TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_session_idempotency_session ON session_idempotency(session_id);
