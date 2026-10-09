package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

var ErrSessionReplacementUnavailable = errors.New("session replacement unavailable")

// Delivery owns its table initialization after Tether migrations. Install only
// Tether's session-ownership guards after that initialization; do not fork the
// library schema or replace its claim/receipt state machine.
func installReplacementDeliveryFences(ctx context.Context, db *sql.DB) error {
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
			return err
		}
	}
	return nil
}

// SessionReplacement records execution lineage. It carries no credential,
// provider payload, native inbox or authority to retire a runtime.
type SessionReplacement struct {
	SourceID, ReplacementID, ActorURI, IntentKey, BindingID string
	BindingGeneration                                       int64
}

// TeamReplacementInput is admitted only under the actor/session launch gates
// and the original team's receipt lease. This initial entry point accepts only
// a positively lost direct execution with no retained protocol/custody. Codex
// custody requires the separate trusted historical accounting issuer.
type TeamReplacementInput struct {
	Source                              SessionRow
	SourcePlanJSON                      string
	DestinationID, Workspace, IntentKey string
	Plan                                *launch.Plan
	// CheckedPID is the recorded direct provider PID positively observed absent
	// by the caller. Zero requires the exact already-confirmed retired archive.
	CheckedPID int64
	// CredentialMode preserves the configured off/observe/enforce behavior.
	// Off admits no execution credential and cannot later default to a grant.
	CredentialMode string
}

// ReplacementTx is the existing write transaction supplied by teamstore. No
// nested database call is made, so the one-connection daemon remains safe.
type ReplacementTx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) SessionReplacement(ctx context.Context, source string) (SessionReplacement, error) {
	return readSessionReplacement(ctx, s.db, source)
}

// SessionReplacementDestination identifies committed placement work after a
// crash. Its current enrolled receipt already addresses the new execution.
func (s *Store) SessionReplacementDestination(ctx context.Context, destination string) (SessionReplacement, error) {
	var source string
	err := s.db.QueryRowContext(ctx, `SELECT source_session_id FROM session_replacements WHERE replacement_session_id=?`, destination).Scan(&source)
	if err != nil {
		return SessionReplacement{}, err
	}
	return s.SessionReplacement(ctx, source)
}

func readSessionReplacement(ctx context.Context, q ReplacementTx, source string) (SessionReplacement, error) {
	var r SessionReplacement
	err := q.QueryRowContext(ctx, `SELECT source_session_id,replacement_session_id,actor_uri,intent_key,binding_id,binding_generation FROM session_replacements WHERE source_session_id=?`, source).
		Scan(&r.SourceID, &r.ReplacementID, &r.ActorURI, &r.IntentKey, &r.BindingID, &r.BindingGeneration)
	return r, err
}

// ReplacementCredentialFloor is inherited delegation metadata, never a token
// or a principal. A new execution credential remains subordinate to this scope
// and expiry ceiling; the historical source credential stays revoked.
type ReplacementCredentialFloor struct {
	Scopes    []string
	ExpiresAt *time.Time
	Mode      string
}

func (s *Store) ReplacementCredentialFloor(ctx context.Context, destination string) (ReplacementCredentialFloor, bool, error) {
	var floor ReplacementCredentialFloor
	var raw string
	var expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT credential_scopes_json,credential_expires_at,credential_mode FROM session_replacements WHERE replacement_session_id=?`, destination).Scan(&raw, &expires, &floor.Mode)
	if errors.Is(err, sql.ErrNoRows) {
		return floor, false, nil
	}
	if err != nil {
		return floor, false, err
	}
	var eligible bool
	if err := s.db.QueryRowContext(ctx, RetainedTeamRecoverySQL, destination, destination, destination, destination).Scan(&eligible); err != nil || !eligible {
		return floor, true, ErrSessionReplacementUnavailable
	}
	if json.Unmarshal([]byte(raw), &floor.Scopes) != nil || floor.Scopes == nil {
		return floor, true, ErrSessionReplacementUnavailable
	}
	if expires.Valid {
		at, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil || !at.After(time.Now()) {
			return floor, true, ErrSessionReplacementUnavailable
		}
		floor.ExpiresAt = &at
	}
	return floor, true, nil
}

// PrepareTeamReplacementTx commits every current enrolled SessionID reference
// together. Historical snapshots, deliveries, addresses and accepted effects
// stay at their old identities. Launch is strictly after this commit.
func (s *Store) PrepareTeamReplacementTx(ctx context.Context, q ReplacementTx, in TeamReplacementInput) (SessionReplacement, error) {
	if q == nil || in.Source.ID == "" || in.DestinationID == "" || in.DestinationID == in.Source.ID || in.Workspace == "" || in.IntentKey == "" || in.Plan == nil {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	if in.CredentialMode != "off" && in.CredentialMode != "observe" && in.CredentialMode != "enforce" {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	id := in.Source.ID
	digest := sha256.Sum256([]byte(in.SourcePlanJSON))
	digestString := hex.EncodeToString(digest[:])
	if saved, err := readSessionReplacement(ctx, q, id); err == nil {
		var originalDigest string
		if err := q.QueryRowContext(ctx, `SELECT source_plan_digest FROM session_replacements WHERE source_session_id=?`, id).Scan(&originalDigest); err != nil {
			return saved, err
		}
		if saved.ReplacementID != in.DestinationID || saved.IntentKey != in.IntentKey || originalDigest != digestString {
			return saved, ErrSessionReplacementUnavailable
		}
		return saved, ctx.Err()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return SessionReplacement{}, err
	}
	var source SessionRow
	err := q.QueryRowContext(ctx, `SELECT id, launch_id, project_id, COALESCE(logical_agent_id,''), provider_id, provider_kind, workspace, state, pid, exit_code, created_at, updated_at, ended_at, session_group_id, parent_session_id, intent, publication, workstream_id, ref_attribution, route_json FROM sessions WHERE id=?`, id).
		Scan(&source.ID, &source.LaunchID, &source.ProjectID, &source.LogicalAgentID, &source.ProviderID, &source.ProviderKind, &source.Workspace, &source.State, &source.PID, &source.ExitCode, &source.CreatedAt, &source.UpdatedAt, &source.EndedAt, &source.SessionGroupID, &source.ParentSessionID, &source.Intent, &source.Publication, &source.WorkstreamID, &source.RefAttribution, &source.RouteJSON)
	if err != nil || source != in.Source {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	var state, originalPlan, agentID string
	var exit sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT s.state,s.exit_code,COALESCE(s.logical_agent_id,''),l.plan_json FROM sessions s JOIN launch_plans l ON l.session_id=s.id WHERE s.id=?`, id).
		Scan(&state, &exit, &agentID, &originalPlan)
	eligibleEnd := state == "orphaned" || state == "failed" && exit.Valid && exit.Int64 == -1
	if err != nil || originalPlan != in.SourcePlanJSON || state != in.Source.State || agentID != in.Source.LogicalAgentID || !eligibleEnd {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	// Preserve all admitted launch authority and provider/work-root choices.
	// Recovery-only inputs may change; an arbitrary fresh resolved plan may not.
	var old launch.Plan
	if json.Unmarshal([]byte(originalPlan), &old) != nil || !old.TeamMember || !in.Plan.TeamMember || in.Plan.ResumeSourceSessionID != id {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	normalized := *in.Plan
	normalized.ResumeSourceSessionID = old.ResumeSourceSessionID
	normalized.ResumeProviderSessionID = old.ResumeProviderSessionID
	normalized.RecoveryActorURI = old.RecoveryActorURI
	normalized.RecoveryPrompt = old.RecoveryPrompt
	normalized.RecoveryCursors = old.RecoveryCursors
	normalized.NativeStateRoot = old.NativeStateRoot
	a, err := json.Marshal(old)
	if err != nil {
		return SessionReplacement{}, err
	}
	b, err := json.Marshal(normalized)
	if err != nil || string(a) != string(b) {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	var custody bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_shims WHERE session_id=?) OR EXISTS(SELECT 1 FROM codex_shim_protocol WHERE session_id=?)`, id, id).Scan(&custody)
	if err != nil || custody {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	f, err := teamShimRecoveryFence(ctx, q, SessionShimRow{SessionID: id})
	if err != nil || f.IntentKey != in.IntentKey || f.ActorURI != in.Plan.RecoveryActorURI {
		return SessionReplacement{}, ErrSessionReplacementUnavailable
	}
	result := SessionReplacement{SourceID: id, ReplacementID: in.DestinationID, ActorURI: f.ActorURI, IntentKey: f.IntentKey, BindingID: f.BindingID, BindingGeneration: f.BindingGeneration}
	var bindingExpiry sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT lease_expires_at FROM runtime_bindings WHERE id=?`, f.BindingID).Scan(&bindingExpiry); err != nil {
		return result, err
	}
	if bindingExpiry.Valid {
		at, err := time.Parse(time.RFC3339Nano, bindingExpiry.String)
		if err != nil || !at.After(time.Now()) {
			return result, ErrSessionReplacementUnavailable
		}
	}
	var recordedPID sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT pid FROM sessions WHERE id=?`, id).Scan(&recordedPID); err != nil {
		return result, err
	}
	var archived bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_gone_shim_recoveries WHERE session_id=? AND binding_id=? AND actor_uri=? AND binding_generation=? AND intent_key=? AND state='recovery_pending')`, id, f.BindingID, f.ActorURI, f.BindingGeneration, f.IntentKey).Scan(&archived); err != nil {
		return result, err
	}
	if !archived && (in.CheckedPID <= 0 || !recordedPID.Valid || recordedPID.Int64 != in.CheckedPID || in.Source.PID != recordedPID) {
		return result, ErrSessionReplacementUnavailable
	}
	// A second active authority association or membership needs a complete
	// explicit mapping; do not silently move only the one we happened to find.
	var bindings, memberships int
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_bindings WHERE session_id=? AND revoked_at IS NULL`, id).Scan(&bindings); err != nil {
		return result, err
	}
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM team_rosters r,json_each(r.payload,'$.members') m WHERE json_extract(m.value,'$.session_id')=? AND json_extract(m.value,'$.status')='active'`, id).Scan(&memberships); err != nil {
		return result, err
	}
	if bindings != 1 || memberships != 1 {
		return result, ErrSessionReplacementUnavailable
	}
	// Current generation alone cannot authorize old in-flight ack after a
	// same-binding remap. Retain all unsettled leases until ownership accounting
	// is available; never cancel, acknowledge or inherit them here.
	var leased bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messaging_deliveries WHERE recipient_urn IN (?,?) AND active_attempt_id<>'')`, f.ActorURI, "msg://session/local/"+id).Scan(&leased)
	if err != nil || leased {
		return result, ErrSessionReplacementUnavailable
	}
	var scopesJSON string
	var expires sql.NullString
	if in.CredentialMode == "off" {
		scopesJSON = "[]"
	} else {
		err = q.QueryRowContext(ctx, `SELECT scopes_json,expires_at FROM principals WHERE kind='session' AND session_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&scopesJSON, &expires)
		var scopes []string
		if err != nil || json.Unmarshal([]byte(scopesJSON), &scopes) != nil || scopes == nil {
			return result, ErrSessionReplacementUnavailable
		}
	}
	if expires.Valid {
		at, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil || !at.After(time.Now()) {
			return result, ErrSessionReplacementUnavailable
		}
	}
	var nonce, key, keyedSession string
	err = q.QueryRowContext(ctx, `SELECT nonce FROM team_port_intents WHERE port_kind='session' AND intent_key=? AND ended='' AND state='done'`, in.IntentKey).Scan(&nonce)
	if err != nil || nonce == "" {
		return result, ErrSessionReplacementUnavailable
	}
	key = "team-port:" + nonce
	err = q.QueryRowContext(ctx, `SELECT session_id FROM session_idempotency WHERE key=? AND operation='create'`, key).Scan(&keyedSession)
	if err != nil || keyedSession != id {
		return result, ErrSessionReplacementUnavailable
	}
	var runID string
	var rosterVersion int64
	err = q.QueryRowContext(ctx, `SELECT r.run_id,r.version FROM team_host_intents h JOIN team_rosters r ON r.run_id=COALESCE(NULLIF(json_extract(h.request,'$.RunID'),''),(SELECT json_extract(l.payload,'$.Run.id') FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key=h.intent_key)) WHERE h.intent_key=?`, in.IntentKey).Scan(&runID, &rosterVersion)
	if err != nil || rosterVersion <= 0 || rosterVersion == math.MaxInt64 {
		return result, ErrSessionReplacementUnavailable
	}
	var memberIndex int
	err = q.QueryRowContext(ctx, `SELECT CAST(m.key AS INTEGER) FROM team_rosters r,json_each(r.payload,'$.members') m WHERE r.run_id=? AND json_extract(m.value,'$.session_id')=? AND json_extract(m.value,'$.actor')=? AND json_extract(m.value,'$.status')='active'`, runID, id, f.ActorURI).Scan(&memberIndex)
	if err != nil || memberIndex < 0 {
		return result, ErrSessionReplacementUnavailable
	}
	planJSON, err := json.Marshal(in.Plan)
	if err != nil {
		return result, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// The source row is the authoritative provider/workstream/route identity;
	// the caller supplies only an isolated prepared destination workspace.
	_, err = q.ExecContext(ctx, `INSERT INTO sessions(id,launch_id,project_id,logical_agent_id,provider_id,provider_kind,workspace,state,created_at,updated_at,parent_session_id,intent,publication,session_group_id,workstream_id,ref_attribution,route_json)
 SELECT ?,launch_id,project_id,logical_agent_id,provider_id,provider_kind,?,'created',?,?,id,'resume',publication,session_group_id,workstream_id,NULL,route_json FROM sessions WHERE id=?`, in.DestinationID, in.Workspace, now, now, id)
	if err != nil {
		return result, err
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO launch_plans(session_id,plan_json) VALUES(?,?)`, in.DestinationID, string(planJSON)); err != nil {
		return result, err
	}
	// Carry the original sealed MCP ceiling rather than resolving a potentially
	// wider current named profile. Launch must match this immutable snapshot;
	// current gateway restrictions continue to intersect it independently.
	var policyJSON string
	if err = q.QueryRowContext(ctx, `SELECT policy_json FROM session_mcp_policy WHERE session_id=?`, id).Scan(&policyJSON); err != nil {
		return result, ErrSessionReplacementUnavailable
	}
	var policy mcpgateway.SessionPolicy
	if json.Unmarshal([]byte(policyJSON), &policy) != nil || policy.Validate() != nil || policy.SessionID != id || policy.AgentID != agentID {
		return result, ErrSessionReplacementUnavailable
	}
	policy.SessionID = in.DestinationID
	policy = policy.Seal()
	policyRaw, err := json.Marshal(policy)
	if err != nil {
		return result, err
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO session_mcp_policy(session_id,policy_json) VALUES(?,?)`, in.DestinationID, string(policyRaw)); err != nil {
		return result, err
	}
	// The native mapping is conversation identity, not protocol state. Keep
	// the old row unchanged; initialize the replacement's native resume input.
	if _, err = q.ExecContext(ctx, `INSERT INTO session_provider_mappings(session_id,owner,provider,native_session_id,created_at,updated_at) SELECT ?,owner,provider,native_session_id,?,? FROM session_provider_mappings WHERE session_id=?`, in.DestinationID, now, now, id); err != nil {
		return result, err
	}
	if err = replacementUpdate(ctx, q, `UPDATE runtime_bindings SET session_id=?,updated_at=? WHERE id=? AND session_id=? AND revoked_at IS NULL AND generation=?`, in.DestinationID, now, f.BindingID, id, f.BindingGeneration); err != nil {
		return result, err
	}
	if err = replacementUpdate(ctx, q, `UPDATE team_host_intents SET member=json_set(member,'$.session_id',?) WHERE intent_key=? AND json_extract(member,'$.session_id')=? AND tombstone='' AND cleaned=0 AND dead=0`, in.DestinationID, in.IntentKey, id); err != nil {
		return result, err
	}
	if err = replacementUpdate(ctx, q, `UPDATE team_port_intents SET payload=json_quote(?) WHERE port_kind='session' AND intent_key=? AND CAST(payload AS TEXT)=json_quote(?) AND ended='' AND state='done'`, in.DestinationID, in.IntentKey, id); err != nil {
		return result, err
	}
	if err = replacementUpdate(ctx, q, `UPDATE session_idempotency SET session_id=? WHERE key=? AND session_id=?`, in.DestinationID, key, id); err != nil {
		return result, err
	}
	path := "$.members[" + strconv.Itoa(memberIndex) + "].session_id"
	if err = replacementUpdate(ctx, q, `UPDATE team_rosters SET version=version+1,payload=json_set(payload,? ,?,'$.version',version+1) WHERE run_id=? AND version=?`, path, in.DestinationID, runID, rosterVersion); err != nil {
		return result, err
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO team_roster_snapshots(run_id,version,payload) SELECT run_id,version,payload FROM team_rosters WHERE run_id=?`, runID); err != nil {
		return result, err
	}
	if _, err = q.ExecContext(ctx, `UPDATE principals SET revoked_at=COALESCE(revoked_at,?) WHERE kind='session' AND session_id=?`, now, id); err != nil {
		return result, err
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO session_replacements(source_session_id,replacement_session_id,actor_uri,intent_key,binding_id,binding_generation,source_plan_digest,credential_scopes_json,credential_expires_at,credential_mode,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, in.DestinationID, f.ActorURI, in.IntentKey, f.BindingID, f.BindingGeneration, digestString, scopesJSON, expires, in.CredentialMode, now); err != nil {
		return result, err
	}
	lineage, _ := json.Marshal(struct {
		ReplacedBy string `json:"replaced_by"`
		Replaces   string `json:"replaces"`
		Actor      string `json:"actor"`
	}{in.DestinationID, id, f.ActorURI})
	if _, err = q.ExecContext(ctx, `INSERT INTO events(scope,session_id,at,kind,payload_json) VALUES('session',?,?,'session.replaced_by',?)`, id, now, string(lineage)); err != nil {
		return result, err
	}
	return result, ctx.Err()
}

func replacementUpdate(ctx context.Context, q ReplacementTx, query string, args ...any) error {
	r, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSessionReplacementUnavailable
	}
	return nil
}
