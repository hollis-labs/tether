package sessionstore

import (
	"context"
	"database/sql"
	"errors"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// ErrTeamRecoveryUnavailable is returned when a session has no single retained team recovery
// authority (old ErrTeamShimRecoveryUnavailable).
var ErrTeamRecoveryUnavailable = errors.New("team shim recovery unavailable")

// repRetainedFromSQLite is the old retainedTeamRecoveryFrom (team_recovery_eligibility.go), unchanged
// except that "now" is bound by the store (the old strftime('%Y-%m-%dT%H:%M:%fZ','now') text, compared
// as text as before). Arguments: session id, session id, now, session id, session id.
const repRetainedFromSQLite = `FROM team_host_intents h
 JOIN team_port_intents p ON p.port_kind='session' AND p.intent_key=h.intent_key
 JOIN team_port_intents e ON e.port_kind='enrollment' AND e.intent_key=h.intent_key
 JOIN runtime_bindings b ON b.attempt_id=e.binding_secret AND b.target_urn=e.acquired_urn
 JOIN registry_entries profile ON profile.urn=b.target_urn
 JOIN team_rosters r ON r.run_id=COALESCE(NULLIF(json_extract(h.request,'$.RunID'),''),(SELECT json_extract(l.payload,'$.Run.id') FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key=h.intent_key))
 JOIN team_runs t ON t.run_id=r.run_id
 WHERE h.tombstone='' AND h.cleaned=0 AND h.dead=0 AND p.ended='' AND p.state='done' AND CAST(p.payload AS TEXT)=json_quote(?)
 AND e.ended='' AND e.binding_ended=0 AND e.state='done' AND b.session_id=? AND b.host_id='team' AND b.visibility='tether-hosted' AND b.revoked_at IS NULL
 AND (b.lease_expires_at IS NULL OR b.lease_expires_at>?) AND b.generation=(SELECT MAX(generation) FROM runtime_bindings WHERE target_urn=b.target_urn)
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

// repRetainedFromPostgres is the same predicate in the Postgres baseline's own translation, the one
// its replacement_principal_authority_fn trigger uses: json_extract becomes tether_jx over tether_json
// (the team tables' BLOB columns are bytea there), the nested json_extract over
// props_json.team_definition becomes tether_reparse, json_each becomes tether_each, and
// CAST(blob AS TEXT)=json_quote(x) becomes convert_from(blob)=to_json(x)::text. Same arguments.
const repRetainedFromPostgres = `FROM team_host_intents h
 JOIN team_port_intents p ON p.port_kind='session' AND p.intent_key=h.intent_key
 JOIN team_port_intents e ON e.port_kind='enrollment' AND e.intent_key=h.intent_key
 JOIN runtime_bindings b ON b.attempt_id=e.binding_secret AND b.target_urn=e.acquired_urn
 JOIN registry_entries profile ON profile.urn=b.target_urn
 JOIN team_rosters r ON r.run_id=COALESCE(NULLIF(tether_jx(tether_json(h.request),'{RunID}') #>> '{}',''),(SELECT tether_jx(tether_json(l.payload),'{Run,id}') #>> '{}' FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key=h.intent_key))
 JOIN team_runs t ON t.run_id=r.run_id
 WHERE h.tombstone='' AND h.cleaned=0 AND h.dead=0 AND p.ended='' AND p.state='done' AND convert_from(p.payload,'UTF8')=to_json(CAST(? AS text))::text
 AND e.ended='' AND e.binding_ended=0 AND e.state='done' AND b.session_id=? AND b.host_id='team' AND b.visibility='tether-hosted' AND b.revoked_at IS NULL
 AND (b.lease_expires_at IS NULL OR b.lease_expires_at>?) AND b.generation=(SELECT MAX(generation) FROM runtime_bindings WHERE target_urn=b.target_urn)
 AND profile.status='active' AND profile.kind='agent'
 AND tether_jx(tether_json(e.payload),'{Enrollment,actor}') #>> '{}'=b.target_urn
 AND tether_jx(tether_json(e.payload),'{Enrollment,agent_id}') #>> '{}'=b.target_urn
 AND tether_jx(tether_reparse(tether_jx(tether_json(profile.props_json),'{team_definition}')),'{id}')=tether_jx(tether_json(e.payload),'{Pin,id}')
 AND tether_jx(tether_reparse(tether_jx(tether_json(profile.props_json),'{team_definition}')),'{revision}')=tether_jx(tether_json(e.payload),'{Pin,revision}')
 AND COALESCE(tether_jx(tether_reparse(tether_jx(tether_json(profile.props_json),'{team_definition}')),'{digest}') #>> '{}','')=COALESCE(tether_jx(tether_json(e.payload),'{Pin,digest}') #>> '{}','')
 AND tether_jx(tether_json(h.member),'{enrolled}') IN ('true'::jsonb,'1'::jsonb)
 AND tether_jx(tether_json(h.member),'{session_id}') #>> '{}'=? AND tether_jx(tether_json(h.member),'{actor}') #>> '{}'=b.target_urn AND tether_jx(tether_json(h.member),'{status}') #>> '{}'='active'
 AND tether_jx(tether_json(t.payload),'{status}') #>> '{}' NOT IN ('completed','failed','canceled','rejected')
 AND EXISTS(SELECT 1 FROM tether_each(tether_jx(tether_json(r.payload),'{members}')) m WHERE tether_jx(m,'{session_id}') #>> '{}'=? AND tether_jx(m,'{actor}') #>> '{}'=b.target_urn AND tether_jx(m,'{status}') #>> '{}'='active')`

func repRetainedFrom(q gostore.Querier) string {
	if q.Dialect().Name() == gostore.DriverPostgres {
		return repRetainedFromPostgres
	}
	return repRetainedFromSQLite
}

func (s *Store) repRetainedArgs(sessionID string) []any {
	return []any{sessionID, sessionID, s.polMillis(), sessionID, sessionID}
}

// RetainedTeamRecovery reports whether the session still holds retained team recovery authority: an
// enrolled, active member of a live team run, with its current unrevoked, unexpired team binding to an
// active agent profile pinned to the same team definition (old RetainedTeamRecoverySQL). q is a reader
// or the caller's transaction.
func (s *Store) RetainedTeamRecovery(ctx context.Context, q gostore.Querier, sessionID string) (bool, error) {
	var eligible bool
	err := q.QueryRowContext(ctx, rebind(q, `SELECT EXISTS(SELECT 1 `+repRetainedFrom(q)+`)`), s.repRetainedArgs(sessionID)...).Scan(&eligible)
	return eligible, err
}

// TeamRecoveryFence is the retained binding a session's team recovery is fenced on (the binding
// half of the old TeamShimRecoveryFence; the custody snapshot is the shim store's, S2b-3c).
type TeamRecoveryFence struct {
	BindingID, ActorURI, IntentKey string
	BindingGeneration              int64
}

// TeamRecoveryFenceFor returns the session's single retained recovery binding, or
// ErrTeamRecoveryUnavailable when it has none or more than one (old teamShimRecoveryFence). q is a
// reader or the caller's transaction.
func (s *Store) TeamRecoveryFenceFor(ctx context.Context, q gostore.Querier, sessionID string) (TeamRecoveryFence, error) {
	var f TeamRecoveryFence
	eligible, err := s.RetainedTeamRecovery(ctx, q, sessionID)
	if err != nil {
		return f, err
	}
	if !eligible {
		return f, ErrTeamRecoveryUnavailable
	}
	var matches int
	err = q.QueryRowContext(ctx, rebind(q, `SELECT b.id, b.target_urn, b.generation, e.intent_key, COUNT(*) OVER() `+repRetainedFrom(q)), s.repRetainedArgs(sessionID)...).
		Scan(&f.BindingID, &f.ActorURI, &f.BindingGeneration, &f.IntentKey, &matches)
	if err != nil || matches != 1 {
		return TeamRecoveryFence{}, ErrTeamRecoveryUnavailable
	}
	return f, nil
}

// RecordTeamEvidence keeps the first observation of a team activation boundary (class B,
// team_activation_evidence): a retry or restart cannot replace the attested baseline. payloadJSON is
// the launch profile slice's encoded evidence, with its own recorded_at stamped by the caller.
func (s *Store) RecordTeamEvidence(ctx context.Context, kind, key, payloadJSON string) error {
	b := s.all.B()
	return b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO team_activation_evidence (kind, evidence_key, payload, recorded_at) VALUES (?, ?, ?, ?)
 ON CONFLICT (kind, evidence_key) DO NOTHING`), kind, key, payloadJSON, s.polMillis())
		return err
	})
}

// TeamEvidence returns the stored evidence payload (sql.ErrNoRows when none, as before).
func (s *Store) TeamEvidence(ctx context.Context, kind, key string) (string, error) {
	r := s.all.B().Reader()
	var raw string
	if err := r.QueryRowContext(ctx, rebind(r, `SELECT payload FROM team_activation_evidence WHERE kind = ? AND evidence_key = ?`), kind, key).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", sql.ErrNoRows
		}
		return "", err
	}
	return raw, nil
}
