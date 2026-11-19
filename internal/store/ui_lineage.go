package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
)

// ReplacementLineage is committed retained local-team execution lineage only.
// It grants no native context, launch permission, credential or custody.
type ReplacementLineage struct {
	IntentKey            string `json:"intent_key"`
	Kind                 string `json:"kind"`
	SourceSessionID      string `json:"source_session_id"`
	ReplacementSessionID string `json:"replacement_session_id"`
	LogicalAgentID       string `json:"logical_agent_id"`
	ActorURI             string `json:"actor_uri"`
	SourceActorURI       string `json:"source_actor_uri"`
	ReplacementActorURI  string `json:"replacement_actor_uri"`
	BindingID            string `json:"binding_id"`
	BindingGeneration    int64  `json:"binding_generation"`
	CommittedAt          string `json:"committed_at"`
}

// GetReplacementLineage projects an immutable committed replacement after
// independently binding its original enrollment/session request, saved plans
// and retained binding to the same actor. Historical settled intermediates are
// supported; callers must separately verify the current running chain tip.
func (s *Store) GetReplacementLineage(ctx context.Context, id string) (*ReplacementLineage, error) {
	var r ReplacementLineage
	var sourceAgent, destAgent, sourceWorker, destWorker, parent, intent, sourceState, destState string
	var sourceJSON, destJSON, digest, enrolledActor, enrollmentPayloadActor, bindingActor string
	var generation int64
	err := s.db.QueryRowContext(ctx, `SELECT x.source_session_id,x.replacement_session_id,x.intent_key,x.actor_uri,x.binding_id,x.binding_generation,x.committed_at,x.source_plan_digest,
 COALESCE(a.logical_agent_id,''),COALESCE(b.logical_agent_id,''),COALESCE(a.worker_id,''),COALESCE(b.worker_id,''),COALESCE(b.parent_session_id,''),COALESCE(b.intent,''),COALESCE(a.state,''),COALESCE(b.state,''),
 COALESCE(pa.plan_json,''),COALESCE(pb.plan_json,''),COALESCE(json_extract(p.request,'$.Actor'),''),COALESCE(e.acquired_urn,''),COALESCE(json_extract(e.payload,'$.Enrollment.actor'),''),COALESCE(rb.target_urn,''),COALESCE(rb.generation,0)
 FROM session_replacements x LEFT JOIN sessions a ON a.id=x.source_session_id LEFT JOIN sessions b ON b.id=x.replacement_session_id
 LEFT JOIN launch_plans pa ON pa.session_id=a.id LEFT JOIN launch_plans pb ON pb.session_id=b.id
 LEFT JOIN team_port_intents p ON p.port_kind='session' AND p.intent_key=x.intent_key
 LEFT JOIN team_port_intents e ON e.port_kind='enrollment' AND e.intent_key=x.intent_key
 LEFT JOIN runtime_bindings rb ON rb.id=x.binding_id WHERE x.replacement_session_id=?`, id).Scan(
		&r.SourceSessionID, &r.ReplacementSessionID, &r.IntentKey, &r.ActorURI, &r.BindingID, &r.BindingGeneration, &r.CommittedAt, &digest,
		&sourceAgent, &destAgent, &sourceWorker, &destWorker, &parent, &intent, &sourceState, &destState, &sourceJSON, &destJSON, &r.SourceActorURI, &enrolledActor, &enrollmentPayloadActor, &bindingActor, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a, b struct {
		TeamMember            bool   `json:"team_member"`
		RecoveryActorURI      string `json:"recovery_actor_uri"`
		ResumeSourceSessionID string `json:"resume_source_session_id"`
	}
	sum := sha256.Sum256([]byte(sourceJSON))
	if json.Unmarshal([]byte(sourceJSON), &a) != nil || json.Unmarshal([]byte(destJSON), &b) != nil || !a.TeamMember || !b.TeamMember || hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("replacement lineage plan identity mismatch")
	}
	r.Kind = "retained_team_v1"
	r.LogicalAgentID = destAgent
	r.ReplacementActorURI = b.RecoveryActorURI
	if r.SourceSessionID == "" || r.SourceSessionID == id || r.ReplacementSessionID != id || sourceAgent == "" || sourceAgent != destAgent || sourceWorker != "" || destWorker != "" || parent != r.SourceSessionID || intent != "resume" || b.ResumeSourceSessionID != r.SourceSessionID || r.IntentKey == "" || r.ActorURI == "" || r.SourceActorURI != r.ActorURI || r.ReplacementActorURI != r.ActorURI || enrolledActor != r.ActorURI || enrollmentPayloadActor != r.ActorURI || bindingActor != r.ActorURI || r.BindingID == "" || r.BindingGeneration <= 0 || generation != r.BindingGeneration || a.RecoveryActorURI != "" && a.RecoveryActorURI != r.ActorURI {
		return nil, fmt.Errorf("replacement lineage conflicts with saved identities")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.CommittedAt); err != nil {
		return nil, fmt.Errorf("replacement lineage timestamp invalid")
	}
	if !lineageSettled(sourceState, true) || destState != "running" && !lineageSettled(destState, true) {
		return nil, nil
	}
	return &r, nil
}

// WorkerLineage is a historical authenticated launch acknowledgement for an
// explicitly relaunched seat, never evidence that a provider thread resumed.
type WorkerLineage struct {
	Kind                 string `json:"kind"`
	SourceSessionID      string `json:"source_session_id"`
	ReplacementSessionID string `json:"replacement_session_id"`
	LogicalAgentID       string `json:"logical_agent_id"`
	SourceWorkerID       string `json:"source_worker_id"`
	WorkerID             string `json:"worker_id"`
	CommandID            string `json:"command_id"`
	AcknowledgedAt       string `json:"acknowledged_at"`
}

// WorkerLaunchAcknowledgement may be supplied only by the authenticated worker
// command/result path. Generic state updates and request JSON never call it.
type WorkerLaunchAcknowledgement struct {
	SessionID, WorkerID, CommandID, ReplyKind, ReplySessionID, ReplyCommandID string
}

func recordRelaunchProvenanceTx(ctx context.Context, q ReplacementTx, row SessionRow, plan *launch.Plan, now string) error {
	if plan == nil || plan.Continuity != "relaunch" || plan.NativeResumeOnly || plan.TeamMember || row.WorkerID == "" || row.Intent != "resume" || !row.ParentSessionID.Valid {
		return nil
	}
	var agent, worker string
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(logical_agent_id,''),COALESCE(worker_id,'') FROM sessions WHERE id=?`, row.ParentSessionID.String).Scan(&agent, &worker); err != nil {
		return err
	}
	if agent == "" || agent != row.LogicalAgentID || row.ID == row.ParentSessionID.String {
		return fmt.Errorf("relaunch provenance source mismatch")
	}
	_, err := q.ExecContext(ctx, `INSERT INTO session_relaunch_provenance(session_id,source_session_id,logical_agent_id,source_worker_id,worker_id,created_at) VALUES(?,?,?,?,?,?)`, row.ID, row.ParentSessionID.String, agent, worker, row.WorkerID, now)
	return err
}

// RecordWorkerLineageAck persists only a positive, exact command acknowledgement.
// Unknown outcomes cannot be upgraded later by a state frame, GET or reconnect.
func (s *Store) RecordWorkerLineageAck(ctx context.Context, a WorkerLaunchAcknowledgement) error {
	if a.SessionID == "" || a.WorkerID == "" || a.CommandID != "launch:"+a.SessionID || a.ReplyKind != "launched" || a.ReplySessionID != a.SessionID || a.ReplyCommandID != a.CommandID {
		return fmt.Errorf("worker launch acknowledgement identity unconfirmed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var r WorkerLineage
	err = tx.QueryRowContext(ctx, `SELECT source_session_id,session_id,logical_agent_id,source_worker_id,worker_id FROM session_relaunch_provenance WHERE session_id=?`, a.SessionID).Scan(&r.SourceSessionID, &r.ReplacementSessionID, &r.LogicalAgentID, &r.SourceWorkerID, &r.WorkerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r.Kind = "worker_lineage_v1"
	r.CommandID = a.CommandID
	r.AcknowledgedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if a.WorkerID != r.WorkerID {
		return fmt.Errorf("worker launch acknowledgement placement mismatch")
	}
	if _, _, err = validateWorkerLineage(ctx, tx, r); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO worker_lineage_receipts(session_id,source_session_id,logical_agent_id,source_worker_id,worker_id,command_id,acknowledged_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(session_id) DO NOTHING`, r.ReplacementSessionID, r.SourceSessionID, r.LogicalAgentID, r.SourceWorkerID, r.WorkerID, r.CommandID, r.AcknowledgedAt); err != nil {
		return err
	}
	saved, err := readWorkerLineage(ctx, tx, a.SessionID)
	if err != nil {
		return err
	}
	r.AcknowledgedAt = saved.AcknowledgedAt
	if r != saved {
		return fmt.Errorf("worker lineage receipt conflict")
	}
	return tx.Commit()
}

func readWorkerLineage(ctx context.Context, q ReplacementTx, id string) (WorkerLineage, error) {
	r := WorkerLineage{Kind: "worker_lineage_v1"}
	err := q.QueryRowContext(ctx, `SELECT source_session_id,session_id,logical_agent_id,source_worker_id,worker_id,command_id,acknowledged_at FROM worker_lineage_receipts WHERE session_id=?`, id).Scan(&r.SourceSessionID, &r.ReplacementSessionID, &r.LogicalAgentID, &r.SourceWorkerID, &r.WorkerID, &r.CommandID, &r.AcknowledgedAt)
	return r, err
}

func validateWorkerLineage(ctx context.Context, q ReplacementTx, r WorkerLineage) (string, string, error) {
	var sourceAgent, destAgent, sourceWorker, destWorker, parent, intent, sourceState, destState string
	var proven bool
	err := q.QueryRowContext(ctx, `SELECT COALESCE(a.logical_agent_id,''),COALESCE(b.logical_agent_id,''),COALESCE(a.worker_id,''),COALESCE(b.worker_id,''),COALESCE(b.parent_session_id,''),b.intent,a.state,b.state,
 EXISTS(SELECT 1 FROM session_relaunch_provenance p WHERE p.session_id=b.id AND p.source_session_id=a.id AND p.logical_agent_id=? AND p.source_worker_id=? AND p.worker_id=?)
 FROM sessions a JOIN sessions b ON b.id=? WHERE a.id=?`, r.LogicalAgentID, r.SourceWorkerID, r.WorkerID, r.ReplacementSessionID, r.SourceSessionID).Scan(&sourceAgent, &destAgent, &sourceWorker, &destWorker, &parent, &intent, &sourceState, &destState, &proven)
	if err != nil {
		return "", "", err
	}
	if r.Kind != "worker_lineage_v1" || !proven || r.SourceSessionID == "" || r.SourceSessionID == r.ReplacementSessionID || r.LogicalAgentID == "" || r.WorkerID == "" || r.CommandID != "launch:"+r.ReplacementSessionID || r.LogicalAgentID != sourceAgent || sourceAgent != destAgent || sourceWorker != r.SourceWorkerID || destWorker != r.WorkerID || parent != r.SourceSessionID || intent != "resume" {
		return "", "", fmt.Errorf("worker lineage conflicts with saved session")
	}
	if _, err = time.Parse(time.RFC3339Nano, r.AcknowledgedAt); err != nil {
		return "", "", fmt.Errorf("worker lineage timestamp invalid")
	}
	return sourceState, destState, nil
}

func (s *Store) GetWorkerLineage(ctx context.Context, id string) (*WorkerLineage, error) {
	r, err := readWorkerLineage(ctx, s.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.ReplacementSessionID != id {
		return nil, fmt.Errorf("worker lineage session mismatch")
	}
	sourceState, destState, err := validateWorkerLineage(ctx, s.db, r)
	if err != nil {
		return nil, err
	}
	if !lineageSettled(sourceState, false) || destState != "running" && !lineageSettled(destState, false) {
		return nil, nil
	}
	return &r, nil
}

func lineageSettled(state string, retainedTeam bool) bool {
	return state == "completed" || state == "failed" || state == "killed" || retainedTeam && state == "orphaned"
}
