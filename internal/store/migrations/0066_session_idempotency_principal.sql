-- 0066_session_idempotency_principal.sql
--
-- CW-20261010-0263 (Identity I3): idempotency keys are scoped per caller.
--
-- session_idempotency was one global key space (0031, key TEXT PRIMARY KEY).
-- It is rebuilt with PRIMARY KEY (principal_id, key), so two principals can
-- use the same key without colliding or seeing each other's sessions.
--
-- Backfill: rows written by the daemon's team runtime (key 'team-port:*') are
-- attributed to 'team-runtime', the principal the daemon itself now claims
-- them under, because internal code addresses them by exact key. Every other
-- existing row becomes principal 'legacy'. A 'legacy' row is never claimed by
-- a new request; it is read-only history that a lookup falls back to for
-- replay, so a client retrying a pre-upgrade key still gets its session.
-- 'anonymous' and 'team-runtime' are reserved ids; real principals are URNs
-- (or device ids) and cannot collide with them.
--
-- ONE-WAY. Folding back to a global key space would be lossy once two
-- principals share a key. Back up the state DB before upgrading; the rows are
-- preserved in full, so restoring the backup is the rollback.

CREATE TABLE session_idempotency_v2 (
    principal_id               TEXT NOT NULL,
    key                        TEXT NOT NULL,
    operation                  TEXT NOT NULL,
    request_digest             TEXT NOT NULL,
    session_id                 TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    resolved_parent_session_id TEXT,
    created_at                 TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (principal_id, key)
);

INSERT INTO session_idempotency_v2
    (principal_id, key, operation, request_digest, session_id, resolved_parent_session_id, created_at)
SELECT CASE WHEN substr(key, 1, 10) = 'team-port:' THEN 'team-runtime' ELSE 'legacy' END,
       key, operation, request_digest, session_id, resolved_parent_session_id, created_at
FROM session_idempotency;

DROP TABLE session_idempotency;
ALTER TABLE session_idempotency_v2 RENAME TO session_idempotency;
CREATE INDEX idx_session_idempotency_session ON session_idempotency(session_id);
