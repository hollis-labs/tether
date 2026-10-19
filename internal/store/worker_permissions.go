package store

import (
	"context"
	"database/sql"
	"errors"
)

type WorkerPermissionParser struct {
	Sequence int64
	Spans    string
	Discard  bool
}

func (s *Store) WorkerPermissionParser(ctx context.Context, id string) (WorkerPermissionParser, error) {
	state := WorkerPermissionParser{Spans: "[]"}
	err := s.db.QueryRowContext(ctx, `SELECT last_seq,spans_json,discard FROM worker_permission_parser WHERE session_id=?`, id).Scan(&state.Sequence, &state.Spans, &state.Discard)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return state, err
}
func (s *Store) SaveWorkerPermissionParser(ctx context.Context, id string, state WorkerPermissionParser) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO worker_permission_parser(session_id,last_seq,spans_json,discard) VALUES (?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET last_seq=excluded.last_seq,spans_json=excluded.spans_json,discard=excluded.discard WHERE excluded.last_seq>worker_permission_parser.last_seq`, id, state.Sequence, state.Spans, state.Discard)
	return err
}
