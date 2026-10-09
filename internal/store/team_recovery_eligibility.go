package store

// RetainedTeamRecoverySQL is shared by same-ID recovery and confirmed-Gone
// custody retirement. Its four parameters are the same canonical session ID.
// The predicate validates current intent, roster, pin and highest binding.
const RetainedTeamRecoverySQL = `SELECT EXISTS(SELECT 1 ` + retainedTeamRecoveryFrom + `)`

const retainedTeamRecoveryFrom = `FROM team_host_intents h
 JOIN team_port_intents p ON p.port_kind='session' AND p.intent_key=h.intent_key
 JOIN team_port_intents e ON e.port_kind='enrollment' AND e.intent_key=h.intent_key
 JOIN runtime_bindings b ON b.attempt_id=e.binding_secret AND b.target_urn=e.acquired_urn
 JOIN registry_entries profile ON profile.urn=b.target_urn
 JOIN team_rosters r ON r.run_id=COALESCE(NULLIF(json_extract(h.request,'$.RunID'),''),(SELECT json_extract(l.payload,'$.Run.id') FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key=h.intent_key))
 JOIN team_runs t ON t.run_id=r.run_id
 WHERE h.tombstone='' AND h.cleaned=0 AND h.dead=0 AND p.ended='' AND p.state='done' AND CAST(p.payload AS TEXT)=json_quote(?)
 AND e.ended='' AND e.binding_ended=0 AND e.state='done' AND b.session_id=? AND b.host_id='team' AND b.visibility='tether-hosted' AND b.revoked_at IS NULL
 AND (b.lease_expires_at IS NULL OR b.lease_expires_at>strftime('%Y-%m-%dT%H:%M:%fZ','now')) AND b.generation=(SELECT MAX(generation) FROM runtime_bindings WHERE target_urn=b.target_urn)
 AND profile.status='active' AND profile.kind='agent'
 AND json_extract(e.payload,'$.Enrollment.actor')=b.target_urn
 AND json_extract(e.payload,'$.Enrollment.agent_id')=b.target_urn
 AND json_extract(json_extract(profile.props_json,'$.team_definition'),'$.id')=json_extract(e.payload,'$.Pin.id')
 AND json_extract(json_extract(profile.props_json,'$.team_definition'),'$.revision')=json_extract(e.payload,'$.Pin.revision')
 AND COALESCE(json_extract(json_extract(profile.props_json,'$.team_definition'),'$.digest'),'')=COALESCE(json_extract(e.payload,'$.Pin.digest'),'')
 AND json_extract(h.member,'$.enrolled')=1
 AND json_extract(h.member,'$.session_id')=? AND json_extract(h.member,'$.actor')=b.target_urn AND json_extract(h.member,'$.status')='active'
 AND json_extract(t.payload,'$.status') NOT IN ('completed','failed','canceled','rejected')
 AND EXISTS(SELECT 1 FROM json_each(r.payload,'$.members') m WHERE json_extract(m.value,'$.session_id')=? AND json_extract(m.value,'$.actor')=b.target_urn AND json_extract(m.value,'$.status')='active')`
