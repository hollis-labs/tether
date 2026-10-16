package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/events"
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

func (s *Store) insertEnvironmentRequest(ctx context.Context, scope, sessionID, payload string) (int64, time.Time, string, error) {
	stored, err := s.insertEnvironmentRequestEnvelope(ctx, events.Event{Scope: scope, SessionID: sessionID, Kind: EnvironmentRequestKind, PayloadJSON: payload})
	return stored.Seq, stored.At, stored.EventID, err
}

func (s *Store) insertEnvironmentRequestEnvelope(ctx context.Context, e events.Event) (events.Event, error) {
	sessionID, payload := e.SessionID, e.PayloadJSON

	var req EnvironmentRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return events.Event{}, err
	}
	if sessionID == "" || req.TurnID == "" || req.RequestID == "" || (req.Open && req.Kind != "question" && req.Kind != "approval") {
		return events.Event{}, fmt.Errorf("incomplete environment request identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return events.Event{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// A resolution cannot invent an observed request, or resolve a different
	// turn. ParentID correlation is normalized by the producer before here.
	if !req.Open {
		err = tx.QueryRowContext(ctx, `SELECT request_kind FROM environment_request_status WHERE session_id=? AND turn_id=? AND request_id=?`, sessionID, req.TurnID, req.RequestID).Scan(&req.Kind)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return events.Event{}, err
		}
	}
	at := time.Now().UTC()
	stored, existing, err := insertEventEnvelopeRow(ctx, tx, e, at, e.EventID, false)
	if err != nil {
		return events.Event{}, err
	}
	if !existing && (req.Kind == "question" || req.Kind == "approval") {
		_, err = tx.ExecContext(ctx, `INSERT INTO environment_request_status(session_id,turn_id,request_id,request_kind,is_open,event_seq,source_sequence) VALUES(?,?,?,?,?,?,?) ON CONFLICT(session_id,turn_id,request_id) DO UPDATE SET is_open=excluded.is_open,event_seq=excluded.event_seq,source_sequence=excluded.source_sequence WHERE excluded.source_sequence>=environment_request_status.source_sequence`, sessionID, req.TurnID, req.RequestID, req.Kind, req.Open, stored.Seq, fmt.Sprintf("%020d", req.SourceSequence))
		if err != nil {
			return events.Event{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return events.Event{}, err
	}
	return stored, nil
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
