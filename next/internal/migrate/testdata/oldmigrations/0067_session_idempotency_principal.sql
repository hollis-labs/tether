-- 0067_session_idempotency_principal.sql
--
-- CW-20261010-0263 (Identity I3): idempotency keys are scoped per caller.
--
-- session_idempotency was one global key space (0031, key TEXT PRIMARY KEY).
-- It is rebuilt with PRIMARY KEY (principal_id, key), so two principals can
-- use the same key without colliding or seeing each other's sessions.
--
-- Backfill: EVERY existing row becomes principal 'legacy'. Ownership is never
-- inferred from the key, which the client chose. 'legacy' rows are kept for
-- audit only and are never matched by a lookup: no principal can replay one,
-- and none sees its conflict text or session id. Consequences, accepted:
--   * a client retrying a pre-upgrade key creates a new session;
--   * remote keys stored as device_v1_<hash> (the pre-upgrade device
--     namespacing) are likewise unreachable now that keys are stored as sent,
--     scoped by the device id.
-- 'anonymous' (unverified local callers, who share one space in observe mode)
-- and 'team-runtime' (keys the daemon's team runtime derives from now on) are
-- reserved ids; real principals are URNs or device ids and cannot collide.
-- Existing 'team-port:*' rows are not carried over to 'team-runtime'.

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
SELECT 'legacy', key, operation, request_digest, session_id, resolved_parent_session_id, created_at
FROM session_idempotency;

DROP TABLE session_idempotency;
ALTER TABLE session_idempotency_v2 RENAME TO session_idempotency;
CREATE INDEX idx_session_idempotency_session ON session_idempotency(session_id);
