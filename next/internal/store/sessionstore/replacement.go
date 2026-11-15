package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
)

// SessionReplacement records execution lineage. It carries no credential, provider payload, native
// inbox or authority to retire a runtime.
type SessionReplacement struct {
	SourceID, ReplacementID, ActorURI, IntentKey, BindingID string
	BindingGeneration                                       int64
}

const repReplacementColumns = `source_session_id, replacement_session_id, actor_uri, intent_key, binding_id, binding_generation`

func repReadReplacement(ctx context.Context, q gostore.Querier, where, arg string) (SessionReplacement, error) {
	var r SessionReplacement
	err := q.QueryRowContext(ctx, rebind(q, `SELECT `+repReplacementColumns+` FROM session_replacements WHERE `+where+` = ?`), arg).
		Scan(&r.SourceID, &r.ReplacementID, &r.ActorURI, &r.IntentKey, &r.BindingID, &r.BindingGeneration)
	return r, err
}

// SessionReplacement returns the replacement of source (sql.ErrNoRows when it was not replaced).
func (s *Store) SessionReplacement(ctx context.Context, source string) (SessionReplacement, error) {
	return repReadReplacement(ctx, s.c.Reader(), "source_session_id", source)
}

// SessionReplacementDestination returns the replacement whose new execution is destination, so that
// placement work committed before a crash can be found again (sql.ErrNoRows when none).
func (s *Store) SessionReplacementDestination(ctx context.Context, destination string) (SessionReplacement, error) {
	return repReadReplacement(ctx, s.c.Reader(), "replacement_session_id", destination)
}

// ReplacementCredentialFloor is inherited delegation metadata, never a token or a principal: a new
// execution credential stays within this scope and expiry ceiling.
type ReplacementCredentialFloor struct {
	Scopes    []string
	ExpiresAt *time.Time
	Mode      string
}

// ReplacementCredentialFloor returns the ceiling recorded for the replacement session destination
// (found false when destination is not a replacement). A found floor is ErrSessionReplacementUnavailable
// when the destination lost its retained team authority, the scopes do not decode, or the ceiling
// has expired.
func (s *Store) ReplacementCredentialFloor(ctx context.Context, destination string) (ReplacementCredentialFloor, bool, error) {
	r := s.c.Reader()
	var floor ReplacementCredentialFloor
	var raw string
	var expires sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT credential_scopes_json, credential_expires_at, credential_mode FROM session_replacements WHERE replacement_session_id = ?`), destination).
		Scan(&raw, &expires, &floor.Mode)
	if errors.Is(err, sql.ErrNoRows) {
		return floor, false, nil
	}
	if err != nil {
		return floor, false, err
	}
	if eligible, err := s.RetainedTeamRecovery(ctx, r, destination); err != nil || !eligible {
		return floor, true, ErrSessionReplacementUnavailable
	}
	if json.Unmarshal([]byte(raw), &floor.Scopes) != nil || floor.Scopes == nil {
		return floor, true, ErrSessionReplacementUnavailable
	}
	if expires.Valid {
		at, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil || !at.After(s.now()) {
			return floor, true, ErrSessionReplacementUnavailable
		}
		floor.ExpiresAt = &at
	}
	return floor, true, nil
}

// ReplacementCustodyEvidence is the retained-custody accounting a Codex replacement records (protocol
// revision and digest, the custody receipt and the accounting reference, each as the caller encoded
// it). Empty fields are stored NULL.
type ReplacementCustodyEvidence struct {
	Revision, Digest, Custody, Reference string
}

// TeamReplacementInput is one team session replacement, admitted by the caller under the actor and
// session launch gates and the original team's receipt lease. The store checks and writes the rows;
// the domain decisions it needs on the way come in as hooks, which must not touch the database except
// through the tx they are given.
type TeamReplacementInput struct {
	Source                              SessionRow // the source session as the caller read it
	SourcePlanJSON                      string     // and its stored launch plan
	DestinationID, Workspace, IntentKey string
	PlanJSON                            string // the destination's resolved launch plan, encoded
	RecoveryActorURI                    string // the plan's recovery actor; must be the binding's actor
	// CheckedPID is the recorded direct provider PID the caller positively observed absent. Zero
	// requires the exact, already confirmed retired archive (team_gone_shim_recoveries).
	CheckedPID int64
	// CredentialMode is the configured off, observe or enforce: off admits no execution credential.
	CredentialMode string

	// CheckPlan is the session slice's plan admission (the old launch.Plan comparison): given the
	// source's stored plan it returns nil only when both plans are team-member plans, the new one
	// resumes the source, and nothing but the recovery-only fields differ. Required.
	CheckPlan func(sourcePlanJSON string) error
	// ResealPolicy turns the source's sealed MCP policy into the destination's (old Validate, the
	// session and agent checks, then SessionID and Seal), or refuses. Required.
	ResealPolicy func(sourcePolicyJSON, sourceID, agentID string) (string, error)
	// Custody is set only for a retained-custody (Codex) replacement, under the joint
	// "PrepareCodexTeamReplacementTx": it validates the historical accounting through tx and returns
	// the evidence to record. With Custody nil, a session holding shim or Codex custody is refused;
	// with it set, a session without custody is.
	Custody func(ctx context.Context, tx store.Tx) (ReplacementCustodyEvidence, error)
	// Stamp enriches the session.replaced_by outbox event (nil: as built).
	Stamp EventStamp
}

// PrepareTeamReplacementTx commits every current enrolled reference to the source session (binding,
// host intent, port intent, team idempotency key, roster member) to the destination together, inside
// the caller's transaction: the allowlisted joint "PrepareTeamReplacementTx" (or
// "PrepareCodexTeamReplacementTx" with in.Custody), which the caller (S9) opens with its own team
// writes. Historical snapshots, deliveries, addresses and accepted effects stay at their old
// identities; the launch comes strictly after the commit, and the caller wakes the outbox then.
//
// A repeat for the same source returns the recorded replacement when it names the same destination,
// intent and source plan, and ErrSessionReplacementUnavailable otherwise. On Postgres the source
// session row is locked first, so concurrent prepares of one source serialize and the second takes
// that path instead of failing on a unique index; SQLite's one writer already serializes them.
func (s *Store) PrepareTeamReplacementTx(ctx context.Context, tx store.Tx, events *eventstore.Store, in TeamReplacementInput) (SessionReplacement, error) {
	if tx == nil || events == nil || in.CheckPlan == nil || in.ResealPolicy == nil || in.Source.ID == "" || in.DestinationID == "" ||
		in.DestinationID == in.Source.ID || in.Workspace == "" || in.IntentKey == "" || in.PlanJSON == "" {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	if in.CredentialMode != "off" && in.CredentialMode != "observe" && in.CredentialMode != "enforce" {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	pg := tx.Dialect().Name() == gostore.DriverPostgres
	id := in.Source.ID
	if pg {
		var locked string
		if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT id FROM sessions WHERE id = ? FOR UPDATE`), id).Scan(&locked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return SessionReplacement{}, ErrSessionReplacementUnavailable
			}
			return SessionReplacement{}, err
		}
	}
	sum := sha256.Sum256([]byte(in.SourcePlanJSON))
	digest := hex.EncodeToString(sum[:])
	if saved, err := repReadReplacement(ctx, tx, "source_session_id", id); err == nil {
		var original string
		if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT source_plan_digest FROM session_replacements WHERE source_session_id = ?`), id).Scan(&original); err != nil {
			return saved, err
		}
		if saved.ReplacementID != in.DestinationID || saved.IntentKey != in.IntentKey || original != digest {
			return saved, ErrSessionReplacementUnavailable
		}
		return saved, ctx.Err()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return SessionReplacement{}, err
	}
	source, err := scanSession(tx.QueryRowContext(ctx, rebind(tx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`), id).Scan)
	if err != nil || !repSameSession(source, in.Source) {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	var state, agentID, originalPlan string
	var exit sql.NullInt64
	err = tx.QueryRowContext(ctx, rebind(tx, `SELECT s.state, s.exit_code, COALESCE(s.logical_agent_id,''), l.plan_json FROM sessions s JOIN launch_plans l ON l.session_id = s.id WHERE s.id = ?`), id).
		Scan(&state, &exit, &agentID, &originalPlan)
	eligibleEnd := state == StateOrphaned || state == "failed" && exit.Valid && exit.Int64 == -1
	if in.Custody != nil {
		// Explicit completion or stop stays terminal; only proven lost custody admits the active states.
		eligibleEnd = eligibleEnd || state == StateRunning || state == StateLaunching || state == StateDetached
	}
	if err != nil || originalPlan != in.SourcePlanJSON || state != in.Source.State || agentID != in.Source.LogicalAgentID || !eligibleEnd {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	if in.CheckPlan(originalPlan) != nil {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	var custody bool
	err = tx.QueryRowContext(ctx, rebind(tx, `SELECT EXISTS(SELECT 1 FROM session_shims WHERE session_id = ?) OR EXISTS(SELECT 1 FROM codex_shim_protocol WHERE session_id = ?)`), id, id).Scan(&custody)
	if err != nil || custody != (in.Custody != nil) {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	var evidence ReplacementCustodyEvidence
	if in.Custody != nil {
		if evidence, err = in.Custody(ctx, tx); err != nil {
			return SessionReplacement{}, err
		}
	}
	f, err := s.TeamRecoveryFenceFor(ctx, tx, id)
	if err != nil || f.IntentKey != in.IntentKey || f.ActorURI != in.RecoveryActorURI {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	result := SessionReplacement{SourceID: id, ReplacementID: in.DestinationID, ActorURI: f.ActorURI, IntentKey: f.IntentKey, BindingID: f.BindingID, BindingGeneration: f.BindingGeneration}
	now := s.now()
	var bindingExpiry sql.NullString
	if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT lease_expires_at FROM runtime_bindings WHERE id = ?`), f.BindingID).Scan(&bindingExpiry); err != nil {
		return result, err
	}
	if bindingExpiry.Valid {
		at, err := time.Parse(time.RFC3339Nano, bindingExpiry.String)
		if err != nil || !at.After(now) {
			return result, ErrSessionReplacementUnavailable
		}
	}
	var recordedPID sql.NullInt64
	if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT pid FROM sessions WHERE id = ?`), id).Scan(&recordedPID); err != nil {
		return result, err
	}
	var archived bool
	if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT EXISTS(SELECT 1 FROM team_gone_shim_recoveries WHERE session_id = ? AND binding_id = ? AND actor_uri = ?
 AND binding_generation = ? AND intent_key = ? AND state = 'recovery_pending')`), id, f.BindingID, f.ActorURI, f.BindingGeneration, f.IntentKey).Scan(&archived); err != nil {
		return result, err
	}
	sourcePID := int64(0)
	if in.Source.PID != nil {
		sourcePID = *in.Source.PID
	}
	if in.Custody == nil && !archived && (in.CheckedPID <= 0 || !recordedPID.Valid || recordedPID.Int64 != in.CheckedPID || in.Source.PID == nil || sourcePID != recordedPID.Int64) {
		return result, ErrSessionReplacementUnavailable
	}
	// A second active binding or membership needs a complete explicit mapping: do not silently move
	// only the one found.
	var bindings, memberships int
	if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT COUNT(*) FROM runtime_bindings WHERE session_id = ? AND revoked_at IS NULL`), id).Scan(&bindings); err != nil {
		return result, err
	}
	membershipsSQL := `SELECT COUNT(*) FROM team_rosters r, json_each(r.payload,'$.members') m WHERE json_extract(m.value,'$.session_id') = ? AND json_extract(m.value,'$.status') = 'active'`
	if pg {
		membershipsSQL = `SELECT COUNT(*) FROM team_rosters r, tether_each(tether_jx(tether_json(r.payload),'{members}')) m
 WHERE tether_jx(m,'{session_id}') #>> '{}' = ? AND tether_jx(m,'{status}') #>> '{}' = 'active'`
	}
	if err := tx.QueryRowContext(ctx, rebind(tx, membershipsSQL), id).Scan(&memberships); err != nil {
		return result, err
	}
	if bindings != 1 || memberships != 1 {
		return result, ErrSessionReplacementUnavailable
	}
	// The current generation alone cannot authorize an old in-flight ack after a same-binding remap:
	// any unsettled delivery lease keeps the source. (go-messaging's tables exist only where its store
	// does, SQLite today, CW-20261115-0013; on Postgres the check runs once they exist.)
	if leased, err := repDeliveryLeased(ctx, tx, f.ActorURI, "msg://session/local/"+id); err != nil || leased {
		return result, ErrSessionReplacementUnavailable
	}
	var scopesJSON string
	var expires sql.NullString
	if in.CredentialMode == "off" {
		scopesJSON = "[]"
	} else {
		err := tx.QueryRowContext(ctx, rebind(tx, `SELECT scopes_json, expires_at FROM principals WHERE kind = 'session' AND session_id = ? ORDER BY id DESC LIMIT 1`), id).Scan(&scopesJSON, &expires)
		var scopes []string
		if err != nil || json.Unmarshal([]byte(scopesJSON), &scopes) != nil || scopes == nil {
			return result, ErrSessionReplacementUnavailable
		}
	}
	if expires.Valid {
		at, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil || !at.After(now) {
			return result, ErrSessionReplacementUnavailable
		}
	}
	var nonce, keyedSession string
	err = tx.QueryRowContext(ctx, rebind(tx, `SELECT nonce FROM team_port_intents WHERE port_kind = 'session' AND intent_key = ? AND ended = '' AND state = 'done'`), in.IntentKey).Scan(&nonce)
	if err != nil || nonce == "" {
		return result, ErrSessionReplacementUnavailable
	}
	key := "team-port:" + nonce
	err = tx.QueryRowContext(ctx, rebind(tx, `SELECT session_id FROM session_idempotency WHERE principal_id = ? AND key = ? AND operation = 'create'`), IdempotencyPrincipalTeam, key).Scan(&keyedSession)
	if err != nil || keyedSession != id {
		return result, ErrSessionReplacementUnavailable
	}
	runSQL := `SELECT r.run_id, r.version FROM team_host_intents h JOIN team_rosters r ON r.run_id = COALESCE(NULLIF(json_extract(h.request,'$.RunID'),''),
 (SELECT json_extract(l.payload,'$.Run.id') FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key = h.intent_key)) WHERE h.intent_key = ?`
	memberSQL := `SELECT CAST(m.key AS INTEGER) FROM team_rosters r, json_each(r.payload,'$.members') m WHERE r.run_id = ? AND json_extract(m.value,'$.session_id') = ?
 AND json_extract(m.value,'$.actor') = ? AND json_extract(m.value,'$.status') = 'active'`
	if pg {
		runSQL = `SELECT r.run_id, r.version FROM team_host_intents h JOIN team_rosters r ON r.run_id = COALESCE(NULLIF(tether_jx(tether_json(h.request),'{RunID}') #>> '{}',''),
 (SELECT tether_jx(tether_json(l.payload),'{Run,id}') #>> '{}' FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key = h.intent_key)) WHERE h.intent_key = ?`
		// json_each's key over an array is the element index; members is an array (an object would
		// have no index to write back, and is refused here).
		memberSQL = `SELECT m.idx - 1 FROM team_rosters r,
 jsonb_array_elements(CASE WHEN jsonb_typeof(tether_jx(tether_json(r.payload),'{members}')) = 'array' THEN tether_jx(tether_json(r.payload),'{members}') ELSE '[]'::jsonb END) WITH ORDINALITY m(value, idx)
 WHERE r.run_id = ? AND tether_jx(m.value,'{session_id}') #>> '{}' = ? AND tether_jx(m.value,'{actor}') #>> '{}' = ? AND tether_jx(m.value,'{status}') #>> '{}' = 'active'`
	}
	var runID string
	var rosterVersion int64
	if err := tx.QueryRowContext(ctx, rebind(tx, runSQL), in.IntentKey).Scan(&runID, &rosterVersion); err != nil || rosterVersion <= 0 || rosterVersion == math.MaxInt64 {
		return result, ErrSessionReplacementUnavailable
	}
	var memberIndex int
	if err := tx.QueryRowContext(ctx, rebind(tx, memberSQL), runID, id, f.ActorURI).Scan(&memberIndex); err != nil || memberIndex < 0 {
		return result, ErrSessionReplacementUnavailable
	}
	stamp := now.Format(time.RFC3339Nano)
	// The source row is the authoritative provider, workstream and route identity; the caller supplies
	// only an isolated prepared destination workspace.
	if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO sessions (id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, created_at, updated_at,
 parent_session_id, intent, publication, session_group_id, workstream_id, ref_attribution, route_json, task_id, app)
 SELECT ?, launch_id, project_id, logical_agent_id, provider_id, provider_kind, ?, 'created', ?, ?, id, 'resume', publication, session_group_id, workstream_id, NULL, route_json, task_id, app
 FROM sessions WHERE id = ?`), in.DestinationID, in.Workspace, stamp, stamp, id); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO launch_plans (session_id, plan_json) VALUES (?, ?)`), in.DestinationID, in.PlanJSON); err != nil {
		return result, err
	}
	// Carry the source's sealed MCP ceiling, not a possibly wider current profile.
	var policyJSON string
	if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT policy_json FROM session_mcp_policy WHERE session_id = ?`), id).Scan(&policyJSON); err != nil {
		return result, ErrSessionReplacementUnavailable
	}
	resealed, err := in.ResealPolicy(policyJSON, id, agentID)
	if err != nil {
		return result, ErrSessionReplacementUnavailable
	}
	if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_mcp_policy (session_id, policy_json) VALUES (?, ?)`), in.DestinationID, resealed); err != nil {
		return result, err
	}
	// The native mapping is conversation identity, not protocol state: the old row stays.
	if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_provider_mappings (session_id, owner, provider, native_session_id, created_at, updated_at)
 SELECT ?, owner, provider, native_session_id, ?, ? FROM session_provider_mappings WHERE session_id = ?`), in.DestinationID, stamp, stamp, id); err != nil {
		return result, err
	}
	if err := repUpdateOne(ctx, tx, `UPDATE runtime_bindings SET session_id = ?, updated_at = ? WHERE id = ? AND session_id = ? AND revoked_at IS NULL AND generation = ?`,
		in.DestinationID, stamp, f.BindingID, id, f.BindingGeneration); err != nil {
		return result, err
	}
	if err := repMoveHostIntent(ctx, tx, pg, in.IntentKey, id, in.DestinationID); err != nil {
		return result, err
	}
	portSQL := `UPDATE team_port_intents SET payload = json_quote(?) WHERE port_kind = 'session' AND intent_key = ? AND CAST(payload AS TEXT) = json_quote(?) AND ended = '' AND state = 'done'`
	if pg {
		portSQL = `UPDATE team_port_intents SET payload = convert_to(to_json(CAST(? AS text))::text, 'UTF8') WHERE port_kind = 'session' AND intent_key = ?
 AND convert_from(payload, 'UTF8') = to_json(CAST(? AS text))::text AND ended = '' AND state = 'done'`
	}
	if err := repUpdateOne(ctx, tx, portSQL, in.DestinationID, in.IntentKey, id); err != nil {
		return result, err
	}
	if err := repUpdateOne(ctx, tx, `UPDATE session_idempotency SET session_id = ? WHERE principal_id = ? AND key = ? AND session_id = ?`,
		in.DestinationID, IdempotencyPrincipalTeam, key, id); err != nil {
		return result, err
	}
	if err := repMoveRosterMember(ctx, tx, pg, runID, rosterVersion, memberIndex, in.DestinationID); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO team_roster_snapshots (run_id, version, payload) SELECT run_id, version, payload FROM team_rosters WHERE run_id = ?`), runID); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE kind = 'session' AND session_id = ?`), stamp, id); err != nil {
		return result, err
	}
	if in.Custody != nil {
		// Record the loss before the immutable execution tombstone; old protocol and custody bytes stay.
		if err := repUpdateOne(ctx, tx, `UPDATE sessions SET state = 'orphaned', exit_code = -1, updated_at = ? WHERE id = ? AND state = ?`, stamp, id, in.Source.State); err != nil {
			return result, err
		}
	}
	if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_replacements (source_session_id, replacement_session_id, actor_uri, intent_key, binding_id, binding_generation,
 source_plan_digest, credential_scopes_json, credential_expires_at, credential_mode, source_protocol_revision, source_protocol_digest, custody_json, obligation_proof_ref, committed_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, in.DestinationID, f.ActorURI, in.IntentKey, f.BindingID, f.BindingGeneration, digest, scopesJSON, expires, in.CredentialMode,
		nullText(evidence.Revision), nullText(evidence.Digest), nullText(evidence.Custody), nullText(evidence.Reference), stamp); err != nil {
		return result, err
	}
	lineage, _ := json.Marshal(struct {
		ReplacedBy string `json:"replaced_by"`
		Replaces   string `json:"replaces"`
		Actor      string `json:"actor"`
	}{in.DestinationID, id, f.ActorURI})
	row := in.Stamp.apply(eventstore.Row{Scope: "session", SessionID: id, Kind: "session.replaced_by", PayloadJSON: string(lineage), At: now})
	if _, _, err := events.InsertTx(ctx, tx, row, true); err != nil {
		return result, err
	}
	return result, ctx.Err()
}

// repSameSession compares two session rows field by field (the pointer fields by value).
func repSameSession(a, b SessionRow) bool {
	samePtr := func(x, y *int64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	if !samePtr(a.PID, b.PID) || !samePtr(a.ExitCode, b.ExitCode) {
		return false
	}
	a.PID, a.ExitCode, b.PID, b.ExitCode = nil, nil, nil, nil
	return a == b
}

// repUpdateOne runs a guarded UPDATE that must change exactly one row (ErrSessionReplacementUnavailable
// otherwise: the row moved on since it was checked).
func repUpdateOne(ctx context.Context, tx store.Tx, query string, args ...any) error {
	res, err := tx.ExecContext(ctx, rebind(tx, query), args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSessionReplacementUnavailable
	}
	return nil
}

// repDeliveryLeased reports whether a delivery to either recipient holds an active lease. go-messaging
// creates messaging_deliveries itself, after the migrations (on SQLite at start; on Postgres it has no
// store yet, CW-20261115-0013): where the table does not exist there is no delivery to hold a lease.
func repDeliveryLeased(ctx context.Context, tx store.Tx, recipients ...string) (bool, error) {
	exists := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'messaging_deliveries')`
	if tx.Dialect().Name() == gostore.DriverPostgres {
		exists = `SELECT to_regclass('messaging_deliveries') IS NOT NULL`
	}
	var present bool
	if err := tx.QueryRowContext(ctx, exists).Scan(&present); err != nil || !present {
		return false, err
	}
	var leased bool
	err := tx.QueryRowContext(ctx, rebind(tx, `SELECT EXISTS(SELECT 1 FROM messaging_deliveries WHERE recipient_urn IN (`+placeholders(len(recipients))+`) AND active_attempt_id <> '')`),
		repAnys(recipients)...).Scan(&leased)
	return leased, err
}

func repAnys(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

// repMoveHostIntent points the team host intent's member at the destination. SQLite runs the old
// json_set. Postgres holds the member as bytea: the row is locked and read, the member rewritten in Go
// as json_set writes it (minified, other members' bytes and order kept), and written back guarded on
// the bytes read.
func repMoveHostIntent(ctx context.Context, tx store.Tx, pg bool, intentKey, from, to string) error {
	if !pg {
		return repUpdateOne(ctx, tx, `UPDATE team_host_intents SET member = json_set(member,'$.session_id',?) WHERE intent_key = ? AND json_extract(member,'$.session_id') = ?
 AND tombstone = '' AND cleaned = 0 AND dead = 0`, to, intentKey, from)
	}
	var member []byte
	err := tx.QueryRowContext(ctx, rebind(tx, `SELECT member FROM team_host_intents WHERE intent_key = ? AND tombstone = '' AND cleaned = 0 AND dead = 0 FOR UPDATE`), intentKey).Scan(&member)
	if err != nil {
		return ErrSessionReplacementUnavailable
	}
	var fields struct {
		SessionID *string `json:"session_id"`
	}
	if json.Unmarshal(member, &fields) != nil || fields.SessionID == nil || *fields.SessionID != from {
		return ErrSessionReplacementUnavailable
	}
	updated, err := repSetPath(member, []any{"session_id"}, wsQuote(to))
	if err != nil {
		return ErrSessionReplacementUnavailable
	}
	return repUpdateOne(ctx, tx, `UPDATE team_host_intents SET member = ? WHERE intent_key = ? AND member = ?`, updated, intentKey, member)
}

// repMoveRosterMember moves roster member index to the destination and bumps the roster version, in
// the payload and the column, guarded on the version read. SQLite runs the old json_set; Postgres
// rewrites the bytea payload in Go as json_set writes it.
func repMoveRosterMember(ctx context.Context, tx store.Tx, pg bool, runID string, version int64, index int, to string) error {
	if !pg {
		path := "$.members[" + strconv.Itoa(index) + "].session_id"
		return repUpdateOne(ctx, tx, `UPDATE team_rosters SET version = version + 1, payload = json_set(payload, ?, ?, '$.version', version + 1) WHERE run_id = ? AND version = ?`,
			path, to, runID, version)
	}
	var payload []byte
	if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT payload FROM team_rosters WHERE run_id = ? AND version = ? FOR UPDATE`), runID, version).Scan(&payload); err != nil {
		return ErrSessionReplacementUnavailable
	}
	updated, err := repSetPath(payload, []any{"members", index, "session_id"}, wsQuote(to))
	if err == nil {
		updated, err = repSetPath(updated, []any{"version"}, strconv.FormatInt(version+1, 10))
	}
	if err != nil {
		return ErrSessionReplacementUnavailable
	}
	return repUpdateOne(ctx, tx, `UPDATE team_rosters SET version = version + 1, payload = ? WHERE run_id = ? AND version = ?`, updated, runID, version)
}

// repSetPath is SQLite's json_set(doc, path, value) for a path of object keys (string) and array
// indexes (int), with value already encoded as JSON: the document is minified (as json_set returns
// it), the value at the path replaced in place, or the last key appended to its object when absent,
// so every other member keeps its bytes and order. A missing intermediate is an error (json_set would
// leave the document unchanged; here the guarded write refuses instead).
func repSetPath(doc []byte, path []any, value string) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, doc); err != nil {
		return nil, err
	}
	return repSetIn(compact.Bytes(), path, []byte(value))
}

func repSetIn(v []byte, path []any, value []byte) ([]byte, error) {
	if len(path) == 0 {
		return value, nil
	}
	dec := json.NewDecoder(bytes.NewReader(v))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch want := path[0].(type) {
	case string:
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			return nil, fmt.Errorf("json path: %q is not in an object", want)
		}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name, _ := tok.(string)
			start := dec.InputOffset() + 1 // past the ':' (compact JSON)
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			end := dec.InputOffset()
			if name == want {
				child, err := repSetIn(v[start:end], path[1:], value)
				if err != nil {
					return nil, err
				}
				return repJoin(v[:start], child, v[end:]), nil
			}
		}
		if len(path) > 1 {
			return nil, fmt.Errorf("json path: no member %q", want)
		}
		closing := bytes.LastIndexByte(v, '}')
		sep := ","
		if closing == 1 {
			sep = ""
		}
		return repJoin(v[:closing], []byte(sep+wsQuote(want)+":"), value, v[closing:]), nil
	case int:
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			return nil, fmt.Errorf("json path: [%d] is not in an array", want)
		}
		for i := 0; dec.More(); i++ {
			start := int(dec.InputOffset())
			if v[start] == ',' {
				start++
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			end := dec.InputOffset()
			if i == want {
				child, err := repSetIn(v[start:end], path[1:], value)
				if err != nil {
					return nil, err
				}
				return repJoin(v[:start], child, v[end:]), nil
			}
		}
		return nil, fmt.Errorf("json path: no element [%d]", want)
	default:
		return nil, fmt.Errorf("json path: unsupported step %v", want)
	}
}

func repJoin(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// InstallReplacementDeliveryFences installs Tether's session-ownership guards on go-messaging's
// delivery tables: a replaced execution can neither claim (messaging_attempts) nor acknowledge
// (messaging_receipts) a delivery. The library owns those tables and creates them after Tether's
// migrations, so the guards cannot be a migration; the caller runs this once the delivery schema is
// applied (the old store did it at open, after delivery.ApplySQLiteSchema). It goes through
// Stores.LibraryDB, the sanctioned handle for the library's tables, and so must not run inside a
// WriteTx or Joint. On Postgres, where go-messaging has no store yet (CW-20261115-0013), there are no
// such tables and nothing to install: it returns nil. Idempotent (IF NOT EXISTS). The third old
// guard, replaced_shim_delete on session_shims, is migration 00006.
func (s *Store) InstallReplacementDeliveryFences(ctx context.Context) error {
	db, err := s.all.LibraryDB(ctx)
	if errors.Is(err, store.ErrLibraryUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE TRIGGER IF NOT EXISTS replaced_delivery_claim BEFORE INSERT ON messaging_attempts
 WHEN EXISTS(SELECT 1 FROM session_replacements WHERE NEW.holder=source_session_id OR NEW.holder='msg://session/local/'||source_session_id)
 BEGIN SELECT RAISE(ABORT,'replaced execution cannot claim delivery'); END`,
		`CREATE TRIGGER IF NOT EXISTS replaced_delivery_receipt BEFORE INSERT ON messaging_receipts
 WHEN EXISTS(SELECT 1 FROM messaging_attempts a JOIN session_replacements x ON a.holder=x.source_session_id OR a.holder='msg://session/local/'||x.source_session_id WHERE a.id=NEW.attempt_id)
 AND NOT EXISTS(SELECT 1 FROM messaging_receipts WHERE delivery_id=NEW.delivery_id AND attempt_id=NEW.attempt_id AND stage=NEW.stage)
 BEGIN SELECT RAISE(ABORT,'replaced execution cannot acknowledge delivery'); END`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("install replacement delivery fences: %w", err)
		}
	}
	return nil
}
