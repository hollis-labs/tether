package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

func normalizeEnvironmentRequest(ctx context.Context, tx *sql.Tx, session string, req *EnvironmentRequest) error {
	sequence := fmt.Sprintf("%020d", req.SourceSequence)
	if req.Terminal {
		if _, err := tx.ExecContext(ctx, `INSERT INTO environment_request_turn_ends(session_id,turn_id,source_sequence) VALUES (?,?,?) ON CONFLICT(session_id,turn_id) DO UPDATE SET source_sequence=max(source_sequence,excluded.source_sequence)`, session, req.TurnID, sequence); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT source_sequence FROM environment_request_turn_ends WHERE session_id=? AND turn_id=?`, session, req.TurnID).Scan(&sequence); err != nil {
			return err
		}
		value, err := strconv.ParseUint(sequence, 10, 64)
		req.SourceSequence = value
		return err
	}
	if !req.Open {
		_, err := tx.ExecContext(ctx, `INSERT INTO environment_request_resolutions(session_id,turn_id,request_id,source_sequence) VALUES (?,?,?,?) ON CONFLICT(session_id,turn_id,request_id) DO UPDATE SET source_sequence=max(source_sequence,excluded.source_sequence)`, session, req.TurnID, req.RequestID, sequence)
		return err
	}
	var floor sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT max(source_sequence) FROM (SELECT source_sequence FROM environment_request_resolutions WHERE session_id=? AND turn_id=? AND request_id=? UNION ALL SELECT source_sequence FROM environment_request_turn_ends WHERE session_id=? AND turn_id=?)`, session, req.TurnID, req.RequestID, session, req.TurnID).Scan(&floor)
	if err != nil {
		return err
	}
	if floor.Valid && sequence <= floor.String {
		req.Open = false
		req.SourceSequence, err = strconv.ParseUint(floor.String, 10, 64)
	}
	return err
}
