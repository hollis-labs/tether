package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const EnvironmentRequestKind = "session.request_status"

type EnvironmentRequest struct {
	SessionID      string `json:"-"`
	TurnID         string `json:"turn_id"`
	RequestID      string `json:"request_id"`
	Kind           string `json:"request_kind,omitempty"`
	Open           bool   `json:"open"`
	SourceSequence uint64 `json:"source_sequence"`
}

func (s *Store) insertEnvironmentRequest(ctx context.Context, scope, sessionID, payload string) (int64, time.Time, error) {
	var req EnvironmentRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return 0, time.Time{}, err
	}
	if sessionID == "" || req.TurnID == "" || req.RequestID == "" || (req.Open && req.Kind != "question" && req.Kind != "approval") {
		return 0, time.Time{}, fmt.Errorf("incomplete environment request identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// A resolution cannot invent an observed request, or resolve a different
	// turn. ParentID correlation is normalized by the producer before here.
	if !req.Open {
		err = tx.QueryRowContext(ctx, `SELECT request_kind FROM environment_request_status WHERE session_id=? AND turn_id=? AND request_id=?`, sessionID, req.TurnID, req.RequestID).Scan(&req.Kind)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, time.Time{}, err
		}
	}
	at := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `INSERT INTO events(scope,session_id,at,kind,payload_json) VALUES(?,?,?,?,?)`, scope, sessionID, at.Format(time.RFC3339Nano), EnvironmentRequestKind, payload)
	if err != nil {
		return 0, time.Time{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, time.Time{}, err
	}
	if req.Kind == "question" || req.Kind == "approval" {
		_, err = tx.ExecContext(ctx, `INSERT INTO environment_request_status(session_id,turn_id,request_id,request_kind,is_open,event_seq,source_sequence) VALUES(?,?,?,?,?,?,?) ON CONFLICT(session_id,turn_id,request_id) DO UPDATE SET is_open=excluded.is_open,event_seq=excluded.event_seq,source_sequence=excluded.source_sequence WHERE excluded.source_sequence>=environment_request_status.source_sequence`, sessionID, req.TurnID, req.RequestID, req.Kind, req.Open, seq, fmt.Sprintf("%020d", req.SourceSequence))
		if err != nil {
			return 0, time.Time{}, err
		}
	}
	return seq, at, tx.Commit()
}

func environmentRequests(ctx context.Context, tx *sql.Tx, head int64, sessionID string) ([]EnvironmentRequest, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id,turn_id,request_id,request_kind,is_open,source_sequence FROM environment_request_status WHERE event_seq<=? AND (?='' OR session_id=?) ORDER BY session_id,turn_id,request_id`, head, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnvironmentRequest
	for rows.Next() {
		var req EnvironmentRequest
		var sequence string
		if err := rows.Scan(&req.SessionID, &req.TurnID, &req.RequestID, &req.Kind, &req.Open, &sequence); err != nil {
			return nil, err
		}
		if _, err := fmt.Sscan(sequence, &req.SourceSequence); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}
