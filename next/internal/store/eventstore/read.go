package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Filter narrows a read. Empty fields do not filter.
type Filter struct {
	Scopes    []string
	Kinds     []string
	SessionID string
	SinceSeq  int64 // id > SinceSeq
	Cursor    int64 // id < Cursor (newest-first paging)
	Limit     int
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Row, error) {
	r := s.b.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, q), args...)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// ListBySession returns a session's events newest first, before cursor when it is > 0
// (limit 1..1000, default 100).
func (s *Store) ListBySession(ctx context.Context, sessionID string, limit int, cursor int64) ([]Row, error) {
	limit = clamp(limit, 100, 1000)
	if cursor > 0 {
		return s.query(ctx, `SELECT `+readColumns+` FROM events WHERE session_id = ? AND id < ? ORDER BY id DESC LIMIT ?`, sessionID, cursor, limit)
	}
	return s.query(ctx, `SELECT `+readColumns+` FROM events WHERE session_id = ? ORDER BY id DESC LIMIT ?`, sessionID, limit)
}

// MaxSeq is the highest event id, 0 when the log is empty. (Plan 13.1: the high-water mark is read
// from the table, never from sqlite_sequence.)
func (s *Store) MaxSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.b.Reader().QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&seq)
	return seq, err
}

// Count is the number of stored events.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.b.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}

// Since returns every event after sinceSeq, oldest first.
func (s *Store) Since(ctx context.Context, sinceSeq int64) ([]Row, error) {
	return s.query(ctx, `SELECT `+readColumns+` FROM events WHERE id > ? ORDER BY id ASC`, sinceSeq)
}

// StreamBounds is the lowest and highest stored id (0, 0 when empty).
func (s *Store) StreamBounds(ctx context.Context) (earliest, head int64, err error) {
	var lo, hi sql.NullInt64
	if err = s.b.Reader().QueryRowContext(ctx, `SELECT (SELECT MIN(id) FROM events), (SELECT MAX(id) FROM events)`).Scan(&lo, &hi); err != nil {
		return 0, 0, err
	}
	return lo.Int64, hi.Int64, nil
}

// Page returns events in (afterSeq, upToSeq] matching f's scopes, session and kinds, oldest first
// (limit default 512).
func (s *Store) Page(ctx context.Context, f Filter, afterSeq, upToSeq int64, limit int) ([]Row, error) {
	if limit <= 0 {
		limit = 512
	}
	var q strings.Builder
	q.WriteString(`SELECT ` + readColumns + ` FROM events WHERE id > ? AND id <= ?`)
	args := []any{afterSeq, upToSeq}
	if len(f.Scopes) > 0 {
		q.WriteString(` AND scope IN (` + placeholders(len(f.Scopes)) + `)`)
		for _, v := range f.Scopes {
			args = append(args, v)
		}
	}
	if f.SessionID != "" {
		q.WriteString(` AND session_id = ?`)
		args = append(args, f.SessionID)
	}
	if len(f.Kinds) > 0 {
		q.WriteString(` AND kind IN (` + placeholders(len(f.Kinds)) + `)`)
		for _, v := range f.Kinds {
			args = append(args, v)
		}
	}
	q.WriteString(` ORDER BY id ASC LIMIT ?`)
	args = append(args, limit)
	return s.query(ctx, q.String(), args...)
}

// Recent returns the newest events (limit 1..1000, default 200).
func (s *Store) Recent(ctx context.Context, limit int) ([]Row, error) {
	return s.query(ctx, `SELECT `+readColumns+` FROM events ORDER BY id DESC LIMIT ?`, clamp(limit, 200, 1000))
}

// Query returns events matching f, newest first (limit 1..1000, default 100).
func (s *Store) Query(ctx context.Context, f Filter) ([]Row, error) {
	var where []string
	var args []any
	if f.SessionID != "" {
		where, args = append(where, "session_id = ?"), append(args, f.SessionID)
	}
	if f.SinceSeq > 0 {
		where, args = append(where, "id > ?"), append(args, f.SinceSeq)
	}
	if f.Cursor > 0 {
		where, args = append(where, "id < ?"), append(args, f.Cursor)
	}
	if len(f.Scopes) > 0 {
		where = append(where, "scope IN ("+placeholders(len(f.Scopes))+")")
		for _, v := range f.Scopes {
			args = append(args, v)
		}
	}
	if len(f.Kinds) > 0 {
		where = append(where, "kind IN ("+placeholders(len(f.Kinds))+")")
		for _, v := range f.Kinds {
			args = append(args, v)
		}
	}
	q := `SELECT ` + readColumns + ` FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, clamp(f.Limit, 100, 1000))
	return s.query(ctx, q, args...)
}

// ByEventID returns the event with that id.
func (s *Store) ByEventID(ctx context.Context, eventID string) (Row, bool, error) {
	r := s.b.Reader()
	row, err := scanRow(r.QueryRowContext(ctx, rebind(r, `SELECT `+readColumns+` FROM events WHERE event_id = ?`), eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	return row, true, nil
}

// HasPublishedProviderResult reports whether a session already has a turn output whose payload's
// provider_result_id is resultID. The old query used json_extract guarded by json_valid; a JSON cast
// on Postgres fails on an invalid payload instead of skipping it, so the database narrows by an
// indexed (session_id, kind) range plus a LIKE on the id, and Go checks the JSON exactly.
func (s *Store) HasPublishedProviderResult(ctx context.Context, sessionID, resultID string) (bool, error) {
	if resultID == "" {
		return false, nil
	}
	r := s.b.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT COALESCE(payload_json,'') FROM events
 WHERE session_id = ? AND kind = ? AND payload_json LIKE ? ESCAPE '\'`), sessionID, TurnOutputKind, "%"+likeEscape(resultID)+"%")
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return false, err
		}
		var p struct {
			ProviderResultID *string `json:"provider_result_id"`
		}
		if json.Unmarshal([]byte(payload), &p) == nil && p.ProviderResultID != nil && *p.ProviderResultID == resultID {
			return true, nil
		}
	}
	return false, rows.Err()
}

// likeEscape escapes LIKE wildcards with backslash (ESCAPE '\' on both dialects).
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
