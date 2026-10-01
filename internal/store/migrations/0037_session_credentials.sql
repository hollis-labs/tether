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
