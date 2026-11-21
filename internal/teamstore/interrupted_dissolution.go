package teamstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/session"
)

// InterruptedDissolution observes the durable whole-roster termination plan.
// It never treats a transition label alone as proof that a session ended.
func (s *Store) InterruptedDissolution(ctx context.Context, id string) (roster teams.Roster, ready bool, err error) {
	err = s.fencedImmediate(ctx, func(c *sql.Conn) error {
		var raw []byte
		if err := c.QueryRowContext(ctx, `SELECT payload FROM team_rosters WHERE run_id=?`, id).Scan(&raw); err != nil {
			return readError("team roster", err)
		}
		if err := decode(raw, &roster); err != nil {
			return err
		}
		ready, err = s.interruptedProof(ctx, c, roster, false)
		return err
	})
	return
}

func (s *Store) interruptedProof(ctx context.Context, c *sql.Conn, r teams.Roster, cleaned bool) (bool, error) {
	if len(r.Members) == 0 {
		return false, nil
	}
	owner, ending := false, false
	for _, m := range r.Members {
		if m.Governance == teams.Owner {
			owner = true
		}
		switch m.Status {
		case "stopped", "released", "failed":
		case "stopping", "releasing":
			ending = true
			ok, err := terminationProof(ctx, c, r.RunID, m, cleaned)
			if err != nil || !ok {
				return false, err
			}
		default:
			return false, nil
		}
	}
	var installed bool
	if err := c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_host_routing WHERE run_id=?)`, r.RunID).Scan(&installed); err != nil {
		return false, err
	}
	return owner && ending && installed, nil
}

// TerminationReady also identifies adopted sessions: their host membership can
// be released, but their external session/binding/enrollment must stay alive.
func (s *Store) TerminationReady(ctx context.Context, runID string, m teams.Member) (ready, adopted bool, err error) {
	err = s.fencedImmediate(ctx, func(c *sql.Conn) error {
		ready, err = terminationProof(ctx, c, runID, m, false)
		if err != nil || !ready || m.Intent == nil {
			return err
		}
		return c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_host_adoptions WHERE intent_key=?)`, m.Intent.IdempotencyKey).Scan(&adopted)
	})
	return
}

func terminationProof(ctx context.Context, c *sql.Conn, runID string, m teams.Member, cleaned bool) (bool, error) {
	if m.Status != "stopping" && m.Status != "releasing" {
		return false, nil
	}
	if m.Intent == nil {
		if m.Status != "releasing" || m.Governance != teams.Owner || m.ID == "" || m.Actor == "" || m.SessionID != "" {
			return false, nil
		}
		var acquired bool
		err := c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_host_intents WHERE json_extract(request,'$.MemberID')=? OR json_extract(member,'$.id')=?)`, m.ID, m.ID).Scan(&acquired)
		return !acquired, err
	}
	req := *m.Intent
	mode := "release"
	if m.Resolution == teams.Fresh {
		mode = "retire"
	}
	if req.IdempotencyKey == "" || req.MemberID != m.ID || req.Slot.Resolution != m.Resolution || (m.Status == "stopping") != (mode == "retire") {
		return false, nil
	}
	if req.RunID != "" {
		if req.RunID != runID {
			return false, nil
		}
	} else {
		var owned bool
		if err := c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_host_launch_intents l JOIN team_host_workflows w USING(launch_key) WHERE l.intent_key=? AND w.run_id=?)`, req.IdempotencyKey, runID).Scan(&owned); err != nil || !owned {
			return false, err
		}
	}
	var request, member []byte
	var tombstone string
	var done bool
	err := c.QueryRowContext(ctx, `SELECT request,member,tombstone,cleaned FROM team_host_intents WHERE intent_key=?`, req.IdempotencyKey).Scan(&request, &member, &tombstone, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var saved teams.ProvisionRequest
	var acquired teams.Member
	if err = decode(request, &saved); err != nil {
		return false, err
	}
	if len(member) == 0 {
		return false, nil
	}
	if err = decode(member, &acquired); err != nil {
		return false, err
	}
	if !reflect.DeepEqual(saved, req) || acquired.ID != m.ID || acquired.Actor != m.Actor || acquired.SessionID != m.SessionID || acquired.Resolution != m.Resolution || (tombstone != "" && tombstone != mode) {
		return false, nil
	}
	if cleaned {
		if !done || tombstone != mode {
			return false, nil
		}
		var bound bool
		if err = c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_host_bindings WHERE intent_key=?)`, req.IdempotencyKey).Scan(&bound); err != nil || bound {
			return false, err
		}
	}
	var adoptionRun, actor, adoptedSession string
	var released bool
	err = c.QueryRowContext(ctx, `SELECT run_id,actor,session_id,released FROM team_host_adoptions WHERE intent_key=?`, req.IdempotencyKey).Scan(&adoptionRun, &actor, &adoptedSession, &released)
	if err == nil {
		return mode == "release" && adoptionRun == runID && actor == string(m.Actor) && adoptedSession == m.SessionID && (!cleaned || released), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// The retained session nonce must still own exactly this member/session.
	var nonce, state, ended string
	var observedSequence int64
	var payload, portRequest []byte
	err = c.QueryRowContext(ctx, `SELECT nonce,state,ended,payload,request,observed_sequence FROM team_port_intents WHERE port_kind='session' AND intent_key=?`, req.IdempotencyKey).Scan(&nonce, &state, &ended, &payload, &portRequest, &observedSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var sessionID string
	if err = json.Unmarshal(payload, &sessionID); err != nil {
		return false, err
	}
	var port struct {
		IntentKey string
		Actor     string
		Provision teams.ProvisionRequest
	}
	if err = json.Unmarshal(portRequest, &port); err != nil {
		return false, err
	}
	if nonce == "" || sessionID != m.SessionID || port.IntentKey != req.IdempotencyKey || port.Actor != string(m.Actor) || !reflect.DeepEqual(port.Provision, req) || (state != "done" && state != "cleaned") || (ended != "" && ended != "stop") || (cleaned && (state != "cleaned" || ended != "stop")) {
		return false, nil
	}
	var endedSession bool
	err = c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN session_idempotency i ON i.session_id=s.id WHERE s.id=? AND i.principal_id='team-runtime' AND i.key=? AND s.state IN ('completed','failed','killed') AND COALESCE(s.pid,0)=0 AND NOT EXISTS(SELECT 1 FROM session_shims WHERE session_id=s.id))`, m.SessionID, "team-port:"+nonce).Scan(&endedSession)
	if err != nil || !endedSession {
		return false, err
	}
	var worker string
	if err = c.QueryRowContext(ctx, `SELECT worker_id FROM sessions WHERE id=?`, m.SessionID).Scan(&worker); err != nil {
		return false, err
	}
	if worker != "" {
		exitSequence, custodySequence, err := session.WorkerExitEvidence(ctx, c, m.SessionID, worker)
		if err != nil || exitSequence == 0 {
			return false, err
		}
		if cleaned && (observedSequence < exitSequence || observedSequence < custodySequence) {
			return false, nil
		}
	}
	var enrollmentActor, enrollmentState, enrollmentEnd string
	var bindingEnded bool
	var enrollmentRequest []byte
	var enrollmentNonce string
	err = c.QueryRowContext(ctx, `SELECT acquired_urn,state,ended,binding_ended,request,nonce FROM team_port_intents WHERE port_kind='enrollment' AND intent_key=?`, req.IdempotencyKey).Scan(&enrollmentActor, &enrollmentState, &enrollmentEnd, &bindingEnded, &enrollmentRequest, &enrollmentNonce)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var enrollment struct {
		IntentKey string
		Actor     string
		Provision teams.ProvisionRequest
	}
	if err = json.Unmarshal(enrollmentRequest, &enrollment); err != nil {
		return false, err
	}
	if enrollmentActor != string(m.Actor) || enrollmentNonce == "" || enrollment.IntentKey != req.IdempotencyKey || enrollment.Actor != string(req.Identity) || !reflect.DeepEqual(enrollment.Provision, req) {
		return false, nil
	}
	return !cleaned || (enrollmentState == "cleaned" && enrollmentEnd == mode && bindingEnded), nil
}

// FinishInterruptedDissolution atomically confirms cleanup receipts and session
// outcomes against the exact roster reserved before external cleanup. It patches
// only transition labels/version and run status, retaining extension metadata.
func (s *Store) FinishInterruptedDissolution(ctx context.Context, expected teams.Roster) (settled bool, err error) {
	err = s.fencedImmediate(ctx, func(c *sql.Conn) error {
		var raw []byte
		if err := c.QueryRowContext(ctx, `SELECT payload FROM team_rosters WHERE run_id=?`, expected.RunID).Scan(&raw); err != nil {
			return readError("team roster", err)
		}
		var current teams.Roster
		if err := decode(raw, &current); err != nil {
			return err
		}
		if !reflect.DeepEqual(current, expected) {
			return teams.ErrConflict
		}
		ok, err := s.interruptedProof(ctx, c, current, true)
		if err != nil || !ok {
			return err
		}
		if current.Version >= math.MaxInt64 {
			return teams.ErrConflict
		}
		for i, m := range current.Members {
			status := ""
			switch m.Status {
			case "stopping":
				status = "stopped"
			case "releasing":
				status = "released"
			}
			if status != "" {
				if err = c.QueryRowContext(ctx, `SELECT json_set(?, ?, ?)`, raw, fmt.Sprintf("$.members[%d].status", i), status).Scan(&raw); err != nil {
					return err
				}
			}
		}
		version := current.Version + 1
		if err = c.QueryRowContext(ctx, `SELECT json_set(?,'$.version',?)`, raw, version).Scan(&raw); err != nil {
			return err
		}
		if _, err = c.ExecContext(ctx, `UPDATE team_rosters SET payload=?,version=? WHERE run_id=?`, raw, version, current.RunID); err != nil {
			return err
		}
		if _, err = c.ExecContext(ctx, `INSERT INTO team_roster_snapshots(run_id,version,payload) VALUES(?,?,?)`, current.RunID, version, raw); err != nil {
			return err
		}
		if _, err = c.ExecContext(ctx, `UPDATE team_host_routing SET removed=1 WHERE run_id=?`, current.RunID); err != nil {
			return err
		}
		// Preserve a result/terminal decision committed concurrently with cleanup.
		if _, err = c.ExecContext(ctx, `UPDATE team_runs SET payload=json_set(payload,'$.status','dissolved') WHERE run_id=? AND json_extract(payload,'$.status') NOT IN ('dissolved','completed','failed','canceled','rejected')`, current.RunID); err != nil {
			return err
		}
		_, err = c.ExecContext(ctx, `UPDATE team_run_deadlines SET closed=1 WHERE run_id=?`, current.RunID)
		settled = err == nil
		return err
	})
	return
}
