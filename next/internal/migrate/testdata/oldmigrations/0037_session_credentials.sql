-- Clean pre-existing terminal/orphan credentials and keep only the newest
-- active token row per session before introducing the uniqueness guard.
UPDATE principals SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE kind = 'session' AND revoked_at IS NULL AND NOT EXISTS (
 SELECT 1 FROM sessions WHERE id = principals.session_id
 AND state NOT IN ('completed', 'failed', 'killed')
);
UPDATE principals SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE kind = 'session' AND revoked_at IS NULL AND id NOT IN (
 SELECT MAX(id) FROM principals WHERE kind = 'session' AND revoked_at IS NULL GROUP BY session_id
);

-- Give existing active session credentials the same expiry backstop as new ones.
UPDATE principals SET expires_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '+7 days')
WHERE kind = 'session' AND revoked_at IS NULL AND expires_at IS NULL;

-- A concurrent launch must not mint a second live credential for the same
-- session. Revoked rows remain history; resume creates a new session.
CREATE UNIQUE INDEX principals_active_session ON principals(session_id)
WHERE kind = 'session' AND revoked_at IS NULL;

-- Terminal state and credential revocation commit together, including crash
-- recovery and launch failures that bypass the runtime's state sink.
CREATE TRIGGER session_principal_requires_active_session BEFORE INSERT ON principals
WHEN NEW.kind = 'session' AND NOT EXISTS (
 SELECT 1 FROM sessions WHERE id = NEW.session_id AND state NOT IN ('completed', 'failed', 'killed')
)
BEGIN
 SELECT RAISE(ABORT, 'session principal requires an active session');
END;
CREATE TRIGGER session_terminal_revokes_credentials AFTER UPDATE OF state ON sessions
WHEN NEW.state IN ('completed', 'failed', 'killed')
BEGIN
 UPDATE principals SET revoked_at = COALESCE(revoked_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
 WHERE kind = 'session' AND session_id = NEW.id;
END;
CREATE TRIGGER session_delete_revokes_credentials AFTER DELETE ON sessions
BEGIN
 UPDATE principals SET revoked_at = COALESCE(revoked_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
 WHERE kind = 'session' AND session_id = OLD.id;
END;
