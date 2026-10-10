-- Extend caller identity without changing existing credential hashes or
-- retained session/shim authority. Rebuild within the migration transaction.
DROP TRIGGER session_terminal_revokes_credentials;
DROP TRIGGER session_delete_revokes_credentials;
CREATE TABLE principals_device_upgrade (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('operator', 'session', 'service', 'interactive', 'device')),
    display TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE CHECK(length(token_hash) = 64),
    scopes_json TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    addresses_json TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    expires_at TEXT,
    last_used_at TEXT,
    last_remote_addr TEXT NOT NULL DEFAULT '',
    last_user_agent TEXT NOT NULL DEFAULT '',
    CHECK(kind <> 'device' OR (expires_at IS NOT NULL AND principal_id <> 'msg://user/local/operator' AND session_id = ''))
);
INSERT INTO principals_device_upgrade (id, principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json, created_by, created_at, revoked_at, expires_at)
 SELECT id, principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json, created_by, created_at, revoked_at, expires_at FROM principals;
-- Preserve AUTOINCREMENT history even if the old highest row was removed.
INSERT INTO sqlite_sequence(name,seq) SELECT 'principals_device_upgrade',seq FROM sqlite_sequence
 WHERE name='principals' AND NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='principals_device_upgrade');
UPDATE sqlite_sequence SET seq=MAX(seq,COALESCE((SELECT seq FROM sqlite_sequence WHERE name='principals'),0)) WHERE name='principals_device_upgrade';
DROP TABLE principals;
ALTER TABLE principals_device_upgrade RENAME TO principals;
CREATE INDEX idx_principals_identity ON principals(principal_id);
CREATE INDEX idx_principals_session ON principals(session_id);
CREATE UNIQUE INDEX principals_active_session ON principals(session_id) WHERE kind='session' AND revoked_at IS NULL;
CREATE UNIQUE INDEX principals_device_identity ON principals(principal_id) WHERE kind='device';
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
CREATE TRIGGER replacement_principal_authority BEFORE INSERT ON principals
WHEN NEW.kind='session' AND EXISTS(SELECT 1 FROM session_replacements WHERE replacement_session_id=NEW.session_id)
BEGIN
 SELECT CASE WHEN EXISTS(SELECT 1 FROM session_replacements WHERE replacement_session_id=NEW.session_id AND credential_mode='off')
 THEN RAISE(ABORT,'replacement identity remains off') END;
 SELECT CASE WHEN NOT EXISTS(
  SELECT 1 FROM session_replacements x
  JOIN runtime_bindings b ON b.id=x.binding_id AND b.target_urn=x.actor_uri AND b.generation=x.binding_generation
  JOIN team_host_intents h ON h.intent_key=x.intent_key
  JOIN team_port_intents p ON p.port_kind='session' AND p.intent_key=h.intent_key
  JOIN team_port_intents e ON e.port_kind='enrollment' AND e.intent_key=h.intent_key
  JOIN registry_entries profile ON profile.urn=b.target_urn
  JOIN team_rosters r ON r.run_id=COALESCE(NULLIF(json_extract(h.request,'$.RunID'),''),(SELECT json_extract(l.payload,'$.Run.id') FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key=h.intent_key))
  JOIN team_runs t ON t.run_id=r.run_id
  WHERE x.replacement_session_id=NEW.session_id
   AND b.session_id=NEW.session_id AND b.host_id='team' AND b.visibility='tether-hosted' AND b.revoked_at IS NULL
   AND (b.lease_expires_at IS NULL OR julianday(b.lease_expires_at)>julianday('now'))
   AND b.generation=(SELECT MAX(generation) FROM runtime_bindings WHERE target_urn=b.target_urn)
   AND h.tombstone='' AND h.cleaned=0 AND h.dead=0
   AND p.state='done' AND p.ended='' AND CAST(p.payload AS TEXT)=json_quote(NEW.session_id)
   AND e.state='done' AND e.ended='' AND e.binding_ended=0 AND e.binding_secret=b.attempt_id AND e.acquired_urn=b.target_urn
   AND profile.status='active' AND profile.kind='agent'
   AND json_extract(e.payload,'$.Enrollment.actor')=b.target_urn AND json_extract(e.payload,'$.Enrollment.agent_id')=b.target_urn
   AND json_extract(json_extract(profile.props_json,'$.team_definition'),'$.id')=json_extract(e.payload,'$.Pin.id')
   AND json_extract(json_extract(profile.props_json,'$.team_definition'),'$.revision')=json_extract(e.payload,'$.Pin.revision')
   AND COALESCE(json_extract(json_extract(profile.props_json,'$.team_definition'),'$.digest'),'')=COALESCE(json_extract(e.payload,'$.Pin.digest'),'')
   AND json_extract(h.member,'$.session_id')=NEW.session_id AND json_extract(h.member,'$.actor')=b.target_urn
   AND json_extract(h.member,'$.status')='active' AND json_extract(h.member,'$.enrolled')=1
   AND json_extract(t.payload,'$.status') NOT IN ('completed','failed','canceled','rejected')
   AND EXISTS(SELECT 1 FROM json_each(r.payload,'$.members') m WHERE json_extract(m.value,'$.session_id')=NEW.session_id AND json_extract(m.value,'$.actor')=b.target_urn AND json_extract(m.value,'$.status')='active')
 ) THEN RAISE(ABORT,'replacement retained authority unavailable') END;
 SELECT CASE WHEN EXISTS(
  SELECT 1 FROM session_replacements x WHERE x.replacement_session_id=NEW.session_id
   AND (json_type(NEW.scopes_json)<>'array'
    OR (NOT EXISTS(SELECT 1 FROM json_each(x.credential_scopes_json) WHERE value='*')
     AND EXISTS(SELECT 1 FROM json_each(NEW.scopes_json) n WHERE NOT EXISTS(SELECT 1 FROM json_each(x.credential_scopes_json) ceiling WHERE ceiling.value=n.value)))
    OR (x.credential_expires_at IS NOT NULL AND (NEW.expires_at IS NULL OR julianday(NEW.expires_at)>julianday(x.credential_expires_at) OR julianday(x.credential_expires_at)<=julianday('now'))))
 ) THEN RAISE(ABORT,'replacement execution credential exceeds inherited ceiling') END;
END;

CREATE TABLE pairing_grants (
 id TEXT PRIMARY KEY,
 code_hash TEXT NOT NULL UNIQUE CHECK(length(code_hash)=64),
 label TEXT NOT NULL,
 scopes_json TEXT NOT NULL CHECK(json_valid(scopes_json) AND json_type(scopes_json)='array'),
 created_by TEXT NOT NULL,
 created_at TEXT NOT NULL,
 expires_at TEXT NOT NULL,
 consumed_at TEXT,
 revoked_at TEXT,
 key_thumbprint TEXT NOT NULL DEFAULT ''
);
CREATE INDEX pairing_grants_expiry ON pairing_grants(expires_at);
