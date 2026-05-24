package store

import (
	"database/sql"
	"fmt"
	"time"
)

// proxyEventsMaxRows is the ring-buffer capacity for the proxy_events table.
// When the row count exceeds this after an insert, the oldest rows are pruned.
const proxyEventsMaxRows = 2000

// ProxyEvent is one recorded MCP proxy tool call event, persisted to the
// proxy_events table. Mirrors mcpadapter.ToolCallEvent but lives in the store
// package to avoid an import cycle.
type ProxyEvent struct {
	ID           int64
	SessionID    string
	Server       string
	ToolName     string
	ArgsSchemaFP string
	DurationMs   int64
	OK           bool
	Error        string
	Timestamp    time.Time
}

// ProxyEventFilter narrows which events are returned by QueryProxyEvents.
// All fields are optional; empty/zero values match everything.
type ProxyEventFilter struct {
	ServerID   string
	ToolName   string
	SessionID  string
	Limit      int
	Since      time.Time
	ErrorsOnly bool
}

func proxyEventWhereClause(f ProxyEventFilter) (string, []any) {
	q := ` WHERE 1=1`
	var args []any

	if f.ErrorsOnly {
		q += " AND ok = 0"
	}
	if f.ServerID != "" {
		q += " AND server = ?"
		args = append(args, f.ServerID)
	}
	if f.ToolName != "" {
		q += " AND tool_name LIKE ?"
		args = append(args, f.ToolName+"%")
	}
	if f.SessionID != "" {
		q += " AND session_id = ?"
		args = append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		q += " AND timestamp > ?"
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	return q, args
}

// AppendProxyEvent inserts ev into the proxy_events table and trims old rows
// when the table exceeds proxyEventsMaxRows. The ID field of ev is ignored.
func (s *Store) AppendProxyEvent(ev ProxyEvent) error {
	ok := 1
	if !ev.OK {
		ok = 0
	}
	ts := ev.Timestamp.UTC().Format(time.RFC3339Nano)
	if ts == "" || ev.Timestamp.IsZero() {
		ts = time.Now().UTC().Format(time.RFC3339Nano)
	}

	_, err := s.db.Exec(
		`INSERT INTO proxy_events
		    (session_id, server, tool_name, args_schema_fp, duration_ms, ok, error, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullIfEmpty(ev.SessionID),
		ev.Server,
		ev.ToolName,
		nullIfEmpty(ev.ArgsSchemaFP),
		ev.DurationMs,
		ok,
		nullIfEmpty(ev.Error),
		ts,
	)
	if err != nil {
		return fmt.Errorf("insert proxy event: %w", err)
	}

	// Trim oldest rows when ring buffer is full.
	_, err = s.db.Exec(
		`DELETE FROM proxy_events WHERE id IN (
		     SELECT id FROM proxy_events ORDER BY id ASC
		     LIMIT MAX(0, (SELECT COUNT(*) FROM proxy_events) - ?)
		 )`,
		proxyEventsMaxRows,
	)
	if err != nil {
		return fmt.Errorf("trim proxy events: %w", err)
	}
	return nil
}

// QueryProxyEvents returns proxy events matching f, oldest-to-newest.
// Limit defaults to 100 when f.Limit == 0; capped at 500.
func (s *Store) QueryProxyEvents(f ProxyEventFilter) ([]ProxyEvent, error) {
	limit := f.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 0 || limit > proxyEventsMaxRows {
		limit = proxyEventsMaxRows
	}

	q := `SELECT id, session_id, server, tool_name, args_schema_fp,
	             duration_ms, ok, error, timestamp
	      FROM proxy_events`
	where, args := proxyEventWhereClause(f)
	// Query fragments come only from proxyEventWhereClause's fixed clauses.
	//nolint:gosec // controlled SQL assembly; user input remains parameterized
	q += where

	q += " ORDER BY id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query proxy events: %w", err)
	}
	defer rows.Close()

	var out []ProxyEvent
	for rows.Next() {
		var ev ProxyEvent
		var sessionID sql.NullString
		var argsSchemaFP sql.NullString
		var errStr sql.NullString
		var okInt int
		var tsStr string

		if err := rows.Scan(
			&ev.ID,
			&sessionID,
			&ev.Server,
			&ev.ToolName,
			&argsSchemaFP,
			&ev.DurationMs,
			&okInt,
			&errStr,
			&tsStr,
		); err != nil {
			return nil, fmt.Errorf("scan proxy event: %w", err)
		}

		ev.SessionID = sessionID.String
		ev.ArgsSchemaFP = argsSchemaFP.String
		ev.Error = errStr.String
		ev.OK = okInt == 1

		ts, parseErr := time.Parse(time.RFC3339Nano, tsStr)
		if parseErr != nil {
			// Fallback to RFC3339 without nanoseconds.
			ts, _ = time.Parse(time.RFC3339, tsStr)
		}
		ev.Timestamp = ts

		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("proxy events rows: %w", err)
	}
	return out, nil
}

// CountProxyEvents returns the number of proxy events matching f before any
// QueryProxyEvents limit is applied.
func (s *Store) CountProxyEvents(f ProxyEventFilter) (int, error) {
	q := `SELECT COUNT(*) FROM proxy_events`
	where, args := proxyEventWhereClause(f)
	// Query fragments come only from proxyEventWhereClause's fixed clauses.
	//nolint:gosec // controlled SQL assembly; user input remains parameterized
	q += where
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count proxy events: %w", err)
	}
	return n, nil
}
