-- Replacement is execution lineage, never a new actor or host-binding grant.
-- Historical rows, native mappings and private protocol bytes stay at old IDs.
CREATE TABLE session_replacements (
    source_session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE RESTRICT,
    replacement_session_id TEXT NOT NULL UNIQUE REFERENCES sessions(id) ON DELETE RESTRICT,
    actor_uri TEXT NOT NULL,
    intent_key TEXT NOT NULL,
    binding_id TEXT NOT NULL,
    binding_generation INTEGER NOT NULL,
    source_plan_digest TEXT NOT NULL,
    credential_scopes_json TEXT NOT NULL CHECK(json_valid(credential_scopes_json) AND json_type(credential_scopes_json)='array'),
    credential_expires_at TEXT,
    credential_mode TEXT NOT NULL CHECK(credential_mode IN ('off','observe','enforce')),
    source_protocol_revision TEXT,
    source_protocol_digest TEXT,
    custody_json TEXT,
    obligation_proof_ref TEXT,
    committed_at TEXT NOT NULL,
    CHECK(source_session_id <> replacement_session_id)
);

CREATE TRIGGER replacement_lineage_update BEFORE UPDATE ON session_replacements
BEGIN SELECT RAISE(ABORT,'replacement lineage is immutable'); END;
CREATE TRIGGER replacement_lineage_delete BEFORE DELETE ON session_replacements
BEGIN SELECT RAISE(ABORT,'replacement lineage is retained'); END;

-- Raw protocol history cannot be reset, drained, reassigned or removed by an
-- old handle after the lineage transaction has frozen its execution.
CREATE TRIGGER replaced_codex_protocol_update BEFORE UPDATE ON codex_shim_protocol
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced protocol is frozen'); END;
CREATE TRIGGER replaced_codex_protocol_delete BEFORE DELETE ON codex_shim_protocol
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced protocol history is retained'); END;
CREATE TRIGGER replaced_codex_protocol_insert BEFORE INSERT ON codex_shim_protocol
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=NEW.session_id)
BEGIN SELECT RAISE(ABORT,'replaced protocol cannot be reinitialized'); END;

CREATE TRIGGER replaced_shim_insert BEFORE INSERT ON session_shims
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=NEW.session_id)
BEGIN SELECT RAISE(ABORT,'replaced execution cannot reacquire placement'); END;
CREATE TRIGGER replaced_shim_update BEFORE UPDATE ON session_shims
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced custody is frozen'); END;

CREATE TRIGGER replaced_session_admission BEFORE UPDATE OF state,pid ON sessions
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.id)
 AND (NEW.state <> OLD.state OR COALESCE(NEW.pid,0) <> COALESCE(OLD.pid,0))
BEGIN SELECT RAISE(ABORT,'replaced session cannot regain runtime ownership'); END;
CREATE TRIGGER replacement_preserve_sessions BEFORE DELETE ON sessions
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.id OR replacement_session_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'replacement lineage is retained'); END;
CREATE TRIGGER replaced_launch_plan_update BEFORE UPDATE ON launch_plans
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced launch plan is frozen'); END;
CREATE TRIGGER replaced_launch_plan_delete BEFORE DELETE ON launch_plans
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced launch plan is retained'); END;
CREATE TRIGGER replaced_native_mapping_update BEFORE UPDATE ON session_provider_mappings
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced native mapping is frozen'); END;
CREATE TRIGGER replaced_native_mapping_insert BEFORE INSERT ON session_provider_mappings
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=NEW.session_id)
BEGIN SELECT RAISE(ABORT,'replaced native mapping is frozen'); END;
CREATE TRIGGER replaced_native_mapping_delete BEFORE DELETE ON session_provider_mappings
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced native mapping is retained'); END;

-- Late provider callbacks must not stage new public output under an execution
-- whose authority was moved. Existing accepted/staged output remains history.
CREATE TRIGGER replaced_session_output BEFORE INSERT ON messages
WHEN NEW.routing_staged=1 AND EXISTS(
 SELECT 1 FROM session_replacements WHERE NEW.from_urn='msg://session/local/'||source_session_id)
BEGIN SELECT RAISE(ABORT,'replaced session output is fenced'); END;

-- The new credential is execution authentication subordinate to the SAME
-- retained host authority. Recheck inside Mint's insert transaction, so a
-- binding/stop race between admission and mint cannot widen authority.
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
