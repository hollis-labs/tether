package workstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
)

// The two bookkeeping kinds eventstore.Insert refuses (ErrBookkeepingKind): their events are written
// here, with their rows, in one joint.
const (
	EnvironmentRequestKind = "session.request_status"
	WorkStateKind          = "session.work_state_changed"
)

// ErrEnvironmentRequestIdentity is returned for a request payload that does not identify one
// request (the old "incomplete environment request identity").
var ErrEnvironmentRequestIdentity = errors.New("incomplete environment request identity")

// EnvironmentRequest is a session.request_status payload, and an environment_request_status row.
type EnvironmentRequest struct {
	Terminal       bool   `json:"terminal,omitempty"`
	SessionID      string `json:"-"`
	TurnID         string `json:"turn_id"`
	RequestID      string `json:"request_id"`
	Kind           string `json:"request_kind,omitempty"`
	Open           bool   `json:"open"`
	SourceSequence uint64 `json:"source_sequence"`
}

// sequenceText is the fixed-width text a source sequence is stored and compared as.
func sequenceText(n uint64) string { return fmt.Sprintf("%020d", n) }

// greatest is the two-argument maximum: SQLite's scalar max(), Postgres's GREATEST.
func greatest(q gostore.Querier) string {
	if isPostgres(q) {
		return "GREATEST"
	}
	return "max"
}

// lockEnvironmentRequest is the Postgres (session, turn) lock RecordEnvironmentRequest takes. Tests
// switch it off to show the race it closes.
var lockEnvironmentRequest = true

// RecordEnvironmentRequest stores a session.request_status event and its bookkeeping rows in one
// transaction, the allowlisted joint "insertEnvironmentRequestEnvelope" (class B event, class C
// fences and status): the old insertEnvironmentRequestEnvelope. r carries the scope, session and
// payload; the kind is set here, and the event gets a fresh id (this path keeps its own
// bookkeeping, as before). The payload is normalized against the fences (an open request already
// resolved, or past its turn's end, is stored closed), and the stored event carries the normalized
// payload.
//
// On Postgres the transaction first takes an advisory lock on (session, turn): the fences are read
// and then the status written, and a resolution or turn end committing in between would otherwise
// leave a resolved request open. On SQLite the one writer already orders them.
func (s *Store) RecordEnvironmentRequest(ctx context.Context, r eventstore.Row) (eventstore.Row, error) {
	var req EnvironmentRequest
	if err := json.Unmarshal([]byte(r.PayloadJSON), &req); err != nil {
		return eventstore.Row{}, err
	}
	sessionID := r.SessionID
	if (req.Terminal && (req.Open || req.RequestID != "" || req.Kind != "" || req.SourceSequence == 0)) || sessionID == "" || req.TurnID == "" ||
		(!req.Terminal && req.RequestID == "") || (req.Open && req.Kind != "question" && req.Kind != "approval") {
		return eventstore.Row{}, ErrEnvironmentRequestIdentity
	}
	var stored eventstore.Row
	err := s.all.Joint(ctx, "insertEnvironmentRequestEnvelope", func(ctx context.Context, tx store.Tx) error {
		if lockEnvironmentRequest && isPostgres(tx) {
			if _, err := tx.ExecContext(ctx, rebind(tx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`),
				fmt.Sprintf("environment_request:%d:%s:%s", len(sessionID), sessionID, req.TurnID)); err != nil {
				return err
			}
		}
		req := req
		if err := normalizeEnvironmentRequest(ctx, tx, sessionID, &req); err != nil {
			return err
		}
		raw, err := json.Marshal(req)
		if err != nil {
			return err
		}
		// A resolution cannot invent an observed request, or resolve a different turn.
		if !req.Open && !req.Terminal {
			err := tx.QueryRowContext(ctx, rebind(tx, `SELECT request_kind FROM environment_request_status WHERE session_id = ? AND turn_id = ? AND request_id = ?`),
				sessionID, req.TurnID, req.RequestID).Scan(&req.Kind)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		ev := r
		ev.Kind, ev.PayloadJSON, ev.EventID = EnvironmentRequestKind, string(raw), ""
		if stored, _, err = s.Events.InsertTx(ctx, tx, ev, false); err != nil {
			return err
		}
		seq := sequenceText(req.SourceSequence)
		if req.Terminal {
			if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE environment_request_status SET is_open = 0, event_seq = ?, source_sequence = ?
 WHERE session_id = ? AND turn_id = ? AND source_sequence <= ?`), stored.Seq, seq, sessionID, req.TurnID, seq); err != nil {
				return err
			}
		}
		if req.Kind == "question" || req.Kind == "approval" {
			open := 0
			if req.Open {
				open = 1
			}
			if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO environment_request_status (session_id, turn_id, request_id, request_kind, is_open, event_seq, source_sequence)
 VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (session_id, turn_id, request_id) DO UPDATE SET is_open = excluded.is_open, event_seq = excluded.event_seq,
 source_sequence = excluded.source_sequence WHERE excluded.source_sequence >= environment_request_status.source_sequence`),
				sessionID, req.TurnID, req.RequestID, req.Kind, open, stored.Seq, seq); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return eventstore.Row{}, err
	}
	return stored, nil
}

// normalizeEnvironmentRequest records a terminal or resolution fence, or closes an open request
// that a fence already passed (the old normalizeEnvironmentRequest).
func normalizeEnvironmentRequest(ctx context.Context, tx store.Tx, session string, req *EnvironmentRequest) error {
	sequence := sequenceText(req.SourceSequence)
	if req.Terminal {
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO environment_request_turn_ends (session_id, turn_id, source_sequence) VALUES (?, ?, ?)
 ON CONFLICT (session_id, turn_id) DO UPDATE SET source_sequence = `+greatest(tx)+`(environment_request_turn_ends.source_sequence, excluded.source_sequence)`),
			session, req.TurnID, sequence); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT source_sequence FROM environment_request_turn_ends WHERE session_id = ? AND turn_id = ?`),
			session, req.TurnID).Scan(&sequence); err != nil {
			return err
		}
		value, err := strconv.ParseUint(sequence, 10, 64)
		req.SourceSequence = value
		return err
	}
	if !req.Open {
		_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO environment_request_resolutions (session_id, turn_id, request_id, source_sequence) VALUES (?, ?, ?, ?)
 ON CONFLICT (session_id, turn_id, request_id) DO UPDATE SET source_sequence = `+greatest(tx)+`(environment_request_resolutions.source_sequence, excluded.source_sequence)`),
			session, req.TurnID, req.RequestID, sequence)
		return err
	}
	var floor sql.NullString
	err := tx.QueryRowContext(ctx, rebind(tx, `SELECT max(source_sequence) FROM (
 SELECT source_sequence FROM environment_request_resolutions WHERE session_id = ? AND turn_id = ? AND request_id = ?
 UNION ALL SELECT source_sequence FROM environment_request_turn_ends WHERE session_id = ? AND turn_id = ?) fences`),
		session, req.TurnID, req.RequestID, session, req.TurnID).Scan(&floor)
	if err != nil {
		return err
	}
	if floor.Valid && sequence <= floor.String {
		req.Open = false
		req.SourceSequence, err = strconv.ParseUint(floor.String, 10, 64)
	}
	return err
}

// EnvironmentWorkState is a session's current work state.
type EnvironmentWorkState struct {
	Value string
	At    time.Time
}

// ParseWorkState reads a session.work_state_changed payload ({"to": "idle"|"processing", "at"}).
func ParseWorkState(payload string) (EnvironmentWorkState, bool) {
	var p struct {
		To string `json:"to"`
		At string `json:"at"`
	}
	if payload == "" || json.Unmarshal([]byte(payload), &p) != nil || (p.To != "idle" && p.To != "processing") {
		return EnvironmentWorkState{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, p.At)
	return EnvironmentWorkState{Value: p.To, At: at}, true
}

// RecordWorkState stores a session.work_state_changed event and its current-value row in one
// transaction, the allowlisted joint "insertWorkStateEnvelope" (the old insertWorkStateEnvelope):
// either both exist or neither does. A payload that names no known value is still stored (the log
// never rejects a kind) but leaves the row alone. The row only moves to a newer event (event_seq),
// so a replayed or late event never overwrites a newer value.
func (s *Store) RecordWorkState(ctx context.Context, r eventstore.Row) (eventstore.Row, error) {
	w, ok := ParseWorkState(r.PayloadJSON)
	r.Kind = WorkStateKind
	var stored eventstore.Row
	err := s.all.Joint(ctx, "insertWorkStateEnvelope", func(ctx context.Context, tx store.Tx) error {
		var err error
		if stored, _, err = s.Events.InsertTx(ctx, tx, r, false); err != nil {
			return err
		}
		if !ok || r.SessionID == "" {
			return nil
		}
		since := w.At
		if since.IsZero() {
			since = stored.At
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO environment_work_state (session_id, value, since, event_seq) VALUES (?, ?, ?, ?)
 ON CONFLICT (session_id) DO UPDATE SET value = excluded.value, since = excluded.since, event_seq = excluded.event_seq
 WHERE excluded.event_seq > environment_work_state.event_seq`), r.SessionID, w.Value, since.UTC().Format(time.RFC3339Nano), stored.Seq); err != nil {
			return fmt.Errorf("record work-state: %w", err)
		}
		return nil
	})
	if err != nil {
		return eventstore.Row{}, err
	}
	return stored, nil
}

// LatestWorkState returns the session's current work state.
func (s *Store) LatestWorkState(ctx context.Context, sessionID string) (EnvironmentWorkState, bool, error) {
	r := s.c.Reader()
	var value, since string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT value, since FROM environment_work_state WHERE session_id = ?`), sessionID).Scan(&value, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return EnvironmentWorkState{}, false, nil
	}
	if err != nil {
		return EnvironmentWorkState{}, false, err
	}
	at, _ := time.Parse(time.RFC3339Nano, since)
	return EnvironmentWorkState{Value: value, At: at}, true, nil
}

// EnvironmentWindow is a contiguous page of the event log after a cursor, or the reason there is
// none (GapReason "ahead", "purged" or "too_large").
type EnvironmentWindow struct {
	HighWater         int64
	EarliestAvailable int64
	Events            []eventstore.Row
	GapReason         string
}

// environmentBounds is the log's head and earliest stored id. Plan 13.1: the head is read from the
// table (MAX(id)), never from sqlite_sequence as the old query also did.
func environmentBounds(ctx context.Context, q gostore.Querier) (head, earliest int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT COALESCE((SELECT MAX(id) FROM events), 0), COALESCE((SELECT MIN(id) FROM events), 0)`).Scan(&head, &earliest)
	if earliest == 0 {
		earliest = head + 1
	}
	return
}

// EnvironmentEvents returns the events in (after, head], at most limit, only when they are
// contiguous: a hole reads as purged, more than limit as too_large, a cursor past the head as ahead.
// The bounds are read in one snapshot and the page is bounded by that head; if retention purges in
// between, the contiguity check reports it.
//
// Postgres caveat: event ids there come from an identity, which a rolled-back or conflicting insert
// spends and concurrent inserts can commit out of order. A hole is then not always a purge. The
// window keeps the old rule (parity on SQLite); see the slice notes.
func (s *Store) EnvironmentEvents(ctx context.Context, after int64, limit int) (EnvironmentWindow, error) {
	if after < 0 || limit < 1 || limit > 10000 {
		return EnvironmentWindow{}, fmt.Errorf("invalid environment event window")
	}
	var w EnvironmentWindow
	if err := s.b.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		w.HighWater, w.EarliestAvailable, err = environmentBounds(ctx, q)
		return err
	}); err != nil {
		return w, err
	}
	switch {
	case after > w.HighWater:
		w.GapReason = "ahead"
		return w, nil
	case after == w.HighWater:
		return w, nil
	case after < w.EarliestAvailable-1:
		w.GapReason = "purged"
		return w, nil
	}
	evs, err := s.Events.Page(ctx, eventstore.Filter{}, after, w.HighWater, limit+1)
	if err != nil {
		return w, err
	}
	next := after + 1
	for _, e := range evs {
		if e.Seq != next {
			w.GapReason = "purged"
			return w, nil
		}
		next++
	}
	if len(evs) > limit {
		w.GapReason = "too_large"
		return w, nil
	}
	if next != w.HighWater+1 {
		w.GapReason = "purged"
		return w, nil
	}
	w.Events = evs
	return w, nil
}

// EnvironmentSessionRow is one session in the environment snapshot.
type EnvironmentSessionRow struct {
	ID, LogicalAgentID, ProviderKind, State, UpdatedAt, RouteJSON string
	LastEventAt                                                   string
}

// EnvironmentSnapshot is the environment's state at one head: sessions, open and closed requests
// recorded at or before the head, and current work states.
type EnvironmentSnapshot struct {
	HighWater         int64
	EarliestAvailable int64
	Sessions          []EnvironmentSessionRow
	Requests          []EnvironmentRequest
	WorkStates        map[string]EnvironmentWorkState
}

// EnvironmentSnapshot reads the snapshot for one session, or every session when sessionID is empty,
// in one read snapshot.
func (s *Store) EnvironmentSnapshot(ctx context.Context, sessionID string) (EnvironmentSnapshot, error) {
	var snap EnvironmentSnapshot
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		if snap.HighWater, snap.EarliestAvailable, err = environmentBounds(ctx, q); err != nil {
			return err
		}
		if snap.Sessions, err = environmentSessions(ctx, q, sessionID); err != nil {
			return err
		}
		if snap.Requests, err = environmentRequests(ctx, q, snap.HighWater, sessionID); err != nil {
			return err
		}
		snap.WorkStates, err = environmentWorkStates(ctx, q, sessionID)
		return err
	})
	return snap, err
}

func environmentSessions(ctx context.Context, q gostore.Querier, sessionID string) ([]EnvironmentSessionRow, error) {
	rows, err := q.QueryContext(ctx, rebind(q, `SELECT id, logical_agent_id, provider_kind, state, updated_at, COALESCE(route_json, ''),
 COALESCE((SELECT MAX(at) FROM events WHERE session_id = sessions.id), '') FROM sessions WHERE (CAST(? AS TEXT) = '' OR id = ?) ORDER BY id`), sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EnvironmentSessionRow
	for rows.Next() {
		var row EnvironmentSessionRow
		if err := rows.Scan(&row.ID, &row.LogicalAgentID, &row.ProviderKind, &row.State, &row.UpdatedAt, &row.RouteJSON, &row.LastEventAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func environmentRequests(ctx context.Context, q gostore.Querier, head int64, sessionID string) ([]EnvironmentRequest, error) {
	rows, err := q.QueryContext(ctx, rebind(q, `SELECT session_id, turn_id, request_id, request_kind, is_open, source_sequence FROM environment_request_status
 WHERE event_seq <= ? AND (CAST(? AS TEXT) = '' OR session_id = ?) ORDER BY session_id, turn_id, request_id`), head, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EnvironmentRequest
	for rows.Next() {
		var req EnvironmentRequest
		var open int64
		var sequence string
		if err := rows.Scan(&req.SessionID, &req.TurnID, &req.RequestID, &req.Kind, &open, &sequence); err != nil {
			return nil, err
		}
		value, err := strconv.ParseUint(sequence, 10, 64)
		if err != nil {
			return nil, err
		}
		req.Open, req.SourceSequence = open != 0, value
		out = append(out, req)
	}
	return out, rows.Err()
}

func environmentWorkStates(ctx context.Context, q gostore.Querier, sessionID string) (map[string]EnvironmentWorkState, error) {
	rows, err := q.QueryContext(ctx, rebind(q, `SELECT session_id, value, since FROM environment_work_state WHERE (CAST(? AS TEXT) = '' OR session_id = ?) ORDER BY session_id`),
		sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]EnvironmentWorkState{}
	for rows.Next() {
		var id, value, since string
		if err := rows.Scan(&id, &value, &since); err != nil {
			return nil, err
		}
		at, _ := time.Parse(time.RFC3339Nano, since)
		out[id] = EnvironmentWorkState{Value: value, At: at}
	}
	return out, rows.Err()
}
