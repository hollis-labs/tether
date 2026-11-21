package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type WorkerExitReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// WorkerExitEvidence binds the latest lifecycle outcome to the session's
// recorded worker. Null exit codes are valid worker exits. Later custody signs
// require a fresh connected-worker absence observation, never hub absence alone.
func WorkerExitEvidence(ctx context.Context, q WorkerExitReader, id, worker string) (exitSequence, custodySequence int64, err error) {
	var at, payload, scopeWorker, state, ended string
	err = q.QueryRowContext(ctx, `SELECT e.id,e.at,e.payload_json,COALESCE(e.scope_worker_id,''),s.state,s.ended_at
 FROM sessions s JOIN events e ON e.id=(SELECT id FROM events WHERE session_id=s.id AND kind='session.state_changed' ORDER BY id DESC LIMIT 1)
 WHERE s.id=? AND s.worker_id=? AND s.state IN ('completed','failed','killed') AND COALESCE(s.pid,0)=0 AND COALESCE(s.ended_at,'')<>''`, id, worker).Scan(&exitSequence, &at, &payload, &scopeWorker, &state, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var event struct {
		Reason   string `json:"reason"`
		To       string `json:"to"`
		WorkerID string `json:"worker_id"`
		Session  struct {
			WorkerID string `json:"worker_id"`
		} `json:"session"`
	}
	if err = json.Unmarshal([]byte(payload), &event); err != nil {
		return 0, 0, err
	}
	if event.Reason != "worker_exit" || event.To != state {
		return 0, 0, nil
	}
	identified := false
	for _, recorded := range []string{scopeWorker, event.WorkerID, event.Session.WorkerID} {
		if recorded != "" {
			if recorded != worker {
				return 0, 0, nil
			}
			identified = true
		}
	}
	if !identified {
		return 0, 0, nil
	}
	observed, parseErr := time.Parse(time.RFC3339Nano, at)
	endTime, endErr := time.Parse(time.RFC3339Nano, ended)
	if err := errors.Join(parseErr, endErr); err != nil {
		return 0, 0, err
	}
	if observed.Before(endTime) {
		return 0, 0, nil
	}
	err = q.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE session_id=? AND id>? AND kind IN ('session.state_changed','session.kill_ordered','session.output','session.turn_output','session.activity','session.work_state_changed','session.launch_failed')`, id, exitSequence).Scan(&custodySequence)
	return
}
