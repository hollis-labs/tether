package eventstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// ProxyEvent is one proxy_events row: an MCP tool call forwarded through the gateway. The
// attribution and telemetry blocks are the domain's JSON, kept verbatim (callcontext.Snapshot and
// events.ToolCallDetails in the old tree).
type ProxyEvent struct {
	ID               int64
	SessionID        string
	Server           string
	ToolName         string
	ArgsSchemaFP     string
	DurationMs       int64
	OK               bool
	Error            string
	Timestamp        time.Time
	AttributionJSON  string
	ClaimedSessionID string
	TelemetryJSON    string
}

// ProxyEventFilter narrows QueryProxyEvents and CountProxyEvents.
type ProxyEventFilter struct {
	ServerID   string
	ToolName   string // a prefix, matched with LIKE as the old store did (case-insensitive; _ and % are wildcards)
	SessionID  string
	Limit      int
	Since      time.Time
	ErrorsOnly bool
}

// ProxyEventsQueryLimit caps one QueryProxyEvents page.
const ProxyEventsQueryLimit = 2000

func proxyWhere(d gostore.Driver, f ProxyEventFilter) (string, []any) {
	q := ` WHERE 1=1`
	var args []any
	if f.ErrorsOnly {
		q += " AND ok = 0"
	}
	if f.ServerID != "" {
		q, args = q+" AND server = ?", append(args, f.ServerID)
	}
	if f.ToolName != "" {
		// SQLite's LIKE folds ASCII case and Postgres's does not: lower() on both keeps the old
		// case-insensitive prefix match. On Postgres both sides are lowered under COLLATE "C" (ASCII
		// folding, as SQLite's lower) and ESCAPE '' makes \ literal, as SQLite's LIKE has no escape.
		if d == gostore.DriverPostgres {
			q += ` AND lower(tool_name COLLATE "C") LIKE lower(CAST(? AS text) COLLATE "C") ESCAPE ''`
		} else {
			q += " AND lower(tool_name) LIKE lower(?)"
		}
		args = append(args, f.ToolName+"%")
	}
	if f.SessionID != "" {
		q, args = q+" AND session_id = ?", append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		q, args = q+" AND timestamp > ?", append(args, formatTime(f.Since))
	}
	return q, args
}

// AppendProxyEvent stores one proxy event; a zero timestamp is the log's time.
func (s *Store) AppendProxyEvent(ctx context.Context, ev ProxyEvent) error {
	ok := int64(0)
	if ev.OK {
		ok = 1
	}
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = s.now()
	}
	attribution, telemetry := ev.AttributionJSON, ev.TelemetryJSON
	if attribution == "" {
		attribution = "{}"
	}
	if telemetry == "" {
		telemetry = "{}"
	}
	err := s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO proxy_events
 (session_id, server, tool_name, args_schema_fp, duration_ms, ok, error, timestamp, attribution_json, claimed_session_id, telemetry_json)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			nullText(ev.SessionID), ev.Server, ev.ToolName, nullText(ev.ArgsSchemaFP), ev.DurationMs, ok, nullText(ev.Error),
			formatTime(ts), attribution, ev.ClaimedSessionID, telemetry)
		return err
	})
	if err != nil {
		return fmt.Errorf("insert proxy event: %w", err)
	}
	return nil
}

// QueryProxyEvents returns matching proxy events oldest first (limit default 100, at most
// ProxyEventsQueryLimit; a negative limit means the maximum).
func (s *Store) QueryProxyEvents(ctx context.Context, f ProxyEventFilter) ([]ProxyEvent, error) {
	limit := f.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 0 || limit > ProxyEventsQueryLimit {
		limit = ProxyEventsQueryLimit
	}
	r := s.b.Reader()
	where, args := proxyWhere(r.Dialect().Name(), f)
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT id, session_id, server, tool_name, args_schema_fp, duration_ms, ok, error,
 timestamp, attribution_json, claimed_session_id, telemetry_json FROM proxy_events`+where+` ORDER BY id ASC LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("query proxy events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProxyEvent
	for rows.Next() {
		var ev ProxyEvent
		var session, fp, errStr sql.NullString
		var ok int64
		var ts string
		if err := rows.Scan(&ev.ID, &session, &ev.Server, &ev.ToolName, &fp, &ev.DurationMs, &ok, &errStr, &ts,
			&ev.AttributionJSON, &ev.ClaimedSessionID, &ev.TelemetryJSON); err != nil {
			return nil, fmt.Errorf("scan proxy event: %w", err)
		}
		ev.SessionID, ev.ArgsSchemaFP, ev.Error, ev.OK = session.String, fp.String, errStr.String, ok == 1
		// The old store accepted second-precision timestamps too, and left an unparseable one zero.
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ev.Timestamp = t
		} else if t, err := time.Parse(time.RFC3339, ts); err == nil {
			ev.Timestamp = t
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("proxy events rows: %w", err)
	}
	return out, nil
}

// CountProxyEvents counts matching proxy events.
func (s *Store) CountProxyEvents(ctx context.Context, f ProxyEventFilter) (int, error) {
	r := s.b.Reader()
	where, args := proxyWhere(r.Dialect().Name(), f)
	var n int
	if err := r.QueryRowContext(ctx, rebind(r, `SELECT COUNT(*) FROM proxy_events`+where), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count proxy events: %w", err)
	}
	return n, nil
}
