-- Exact retired custody and the existing binding fence survive same-ID
-- recovery. No capability, launch environment, descriptor content or journal
-- payload is archived here. This is not a general cleanup/retirement table.
CREATE TABLE team_gone_shim_recoveries (
 shim_key TEXT PRIMARY KEY NOT NULL,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
 binding_id TEXT NOT NULL,
 actor_uri TEXT NOT NULL,
 binding_generation INTEGER NOT NULL,
 intent_key TEXT NOT NULL,
 custody_json TEXT NOT NULL CHECK(json_valid(custody_json)),
 state TEXT NOT NULL CHECK(state='recovery_pending'),
 confirmed_at TEXT NOT NULL
);
CREATE INDEX team_gone_shim_recoveries_session ON team_gone_shim_recoveries(session_id);
CREATE TRIGGER team_gone_shim_recovery_preserve_audit BEFORE DELETE ON sessions
WHEN EXISTS(SELECT 1 FROM team_gone_shim_recoveries WHERE session_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'team shim recovery audit retained'); END;
