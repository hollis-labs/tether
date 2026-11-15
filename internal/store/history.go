package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// HistoryDeadline bounds one events history read (CW-20261114-0166). A read
// that runs out returns what it found and a cursor to continue from, instead
// of holding a connection (and a read transaction that blocks checkpoints).
const HistoryDeadline = 5 * time.Second

// EventPage is one page of events history, newest first. NextCursor, when
// non-zero, is the cursor for the next page (pass it as EventFilter.Cursor).
// Partial says the read ran out of time: Events is everything newer than
// NextCursor that matches, but there may be more past it.
type EventPage struct {
	Events     []events.Event
	NextCursor int64
	Partial    bool
}

// History windows: a read scans id ranges newest first, so a deadline always
// leaves a cursor at the edge of what was searched, even with nothing found.
// An empty window doubles the next one.
var (
	historyWindowMin int64 = 4096
	historyWindowMax int64 = 1 << 18
	// afterHistoryWindow, when set by a test, runs after each window.
	afterHistoryWindow func()
)

// historyDB is the read-only pool, or db on a store opened read-only.
func (s *Store) historyDB() *sql.DB {
	if s.reader != nil {
		return s.reader
	}
	return s.db
}

// QueryEventHistory is QueryEvents for history reads (GET /events,
// GET /sessions/{id}/events, tether_events_history): on the read-only pool,
// bounded by ctx, returning a partial page and its cursor when ctx runs out.
func (s *Store) QueryEventHistory(ctx context.Context, f EventFilter) (EventPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	db := s.historyDB()
	hi := f.Cursor
	if hi <= 0 {
		var top sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT max(id) FROM events`).Scan(&top); err != nil {
			if ctx.Err() != nil {
				return EventPage{Partial: true}, nil //nolint:nilerr // out of time before the first window: a partial (empty) page, not a failure
			}
			return EventPage{}, err
		}
		hi = top.Int64 + 1
	}
	floor := f.SinceSeq + 1 // ids >= floor
	if floor < 1 {
		floor = 1
	}
	query, args := historyQuery(f)
	page := EventPage{}
	window := historyWindowMin
	for hi > floor && len(page.Events) < limit {
		lo := max(hi-window, floor)
		got, err := scanHistory(ctx, db, query, append(append([]any(nil), args...), lo, hi, limit-len(page.Events)))
		page.Events = append(page.Events, got...)
		if err != nil {
			if ctx.Err() == nil {
				return EventPage{}, err
			}
			// Out of time inside this window: everything newer than the last row
			// returned (or, with none, than hi) has been searched.
			page.NextCursor, page.Partial = hi, true
			if len(got) > 0 {
				page.NextCursor = got[len(got)-1].Seq
			}
			return page, nil
		}
		if len(got) == 0 {
			window = min(window*2, historyWindowMax)
		}
		hi = lo
		if afterHistoryWindow != nil {
			afterHistoryWindow()
		}
		if ctx.Err() != nil && hi > floor && len(page.Events) < limit {
			page.NextCursor, page.Partial = hi, true
			return page, nil
		}
	}
	if len(page.Events) == limit {
		page.NextCursor = page.Events[len(page.Events)-1].Seq
	}
	return page, nil
}

// historyQuery is one window's query: the filter's predicates (args) and then
// the window's bounds and the row limit (id >= ?, id < ?, LIMIT ?).
func historyQuery(f EventFilter) (string, []any) {
	where, args := historyWhere(f)
	//nolint:gosec // G202: where holds fixed column predicates with ? placeholders; values are bound via args
	return `SELECT ` + eventReadColumns + ` FROM events WHERE ` + strings.Join(append(where, "id >= ?", "id < ?"), " AND ") + ` ORDER BY id DESC LIMIT ?`, args
}

// historyWhere is the filter's predicates other than the id bounds.
func historyWhere(f EventFilter) ([]string, []any) {
	var (
		where []string
		args  []any
	)
	if f.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if len(f.Scopes) > 0 {
		where = append(where, "scope IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(f.Scopes)), ", ")+")")
		for _, scope := range f.Scopes {
			args = append(args, scope)
		}
	}
	if len(f.Kinds) > 0 {
		where = append(where, "kind IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(f.Kinds)), ", ")+")")
		for _, kind := range f.Kinds {
			args = append(args, kind)
		}
	}
	return where, args
}

// scanHistory runs one window's query and returns the rows it read, also when
// ctx ends part-way (with the error): rows come newest first, so they are a
// correct prefix of the window.
func scanHistory(ctx context.Context, db *sql.DB, query string, args []any) ([]events.Event, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
