package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Finding is one pre-check's result.
type Finding struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Precheck runs the cutover pre-checks on the source copy (migrate-from --precheck). It reads only, never
// changes anything, and reports each check; the runbook acts on a failed one before the final copy.
func (e *Engine) Precheck(ctx context.Context) ([]Finding, error) {
	var out []Finding
	checks := []struct {
		name string
		run  func(context.Context) (Finding, error)
	}{{"stuck_launches", e.checkStuckLaunches}, {"event_identity", e.checkEventIdentity}}
	for _, c := range checks {
		// Each check runs on its own: one that cannot run (a table missing in the source) fails with its
		// error and the others still report.
		f, err := c.run(ctx)
		if err != nil {
			f = Finding{Check: c.name, OK: false, Detail: "could not run: " + err.Error()}
		}
		out = append(out, f)
	}
	return out, nil
}

// checkStuckLaunches: no session in 'created' may hold an admitted or reserved workspace allocation
// (#593). Such a launch stopped between reservation or admission and placement; its reservation digest was written by the old code
// (nullable columns marshaled as objects), so it never matches the new digest: the launch fails every
// time, and the session stays 'created', counted live, blocking resume and recovery for its agent. Drain
// or kill these sessions on the old hub before the final copy.
func (e *Engine) checkStuckLaunches(ctx context.Context) (Finding, error) {
	f := Finding{Check: "stuck_launches"}
	rows, err := e.src.QueryContext(ctx, `SELECT s.id FROM sessions s JOIN workspace_scratch_allocations w ON w.session_id = s.id
		WHERE s.state = 'created' AND w.status IN ('reserved', 'admitted') ORDER BY s.id`)
	if err != nil {
		return f, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return f, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return f, err
	}
	f.OK = len(ids) == 0
	if f.OK {
		f.Detail = "no session in 'created' holds a reserved or admitted allocation"
		return f, nil
	}
	shown := ids
	if len(shown) > 20 {
		shown = shown[:20]
	}
	f.Detail = fmt.Sprintf("%d session(s) in 'created' hold a reserved or admitted allocation; drain or kill them on the old hub before the final copy: %s",
		len(ids), strings.Join(shown, ", "))
	return f, nil
}

// checkEventIdentity: the event identity's cursor and high-water mark are consistent (plan 13.7 B-1):
// no event above the frozen cutover lacks an event id, the backfill cursor is not above the cutover, and
// events' AUTOINCREMENT mark is at least the largest id and the cutover (it is carried, never re-derived,
// so an id a client holds is never reissued).
func (e *Engine) checkEventIdentity(ctx context.Context) (Finding, error) {
	f := Finding{Check: "event_identity"}
	var cutover, next, remaining int64
	err := e.src.QueryRowContext(ctx, `SELECT cutover, next_legacy_id, remaining FROM event_identity WHERE singleton = 1`).Scan(&cutover, &next, &remaining)
	if errors.Is(err, sql.ErrNoRows) {
		f.OK, f.Detail = true, "event identity not seeded (nothing to check)"
		return f, nil
	}
	if err != nil {
		return f, err
	}
	var maxID, seq sql.NullInt64
	var nullAbove int64
	if err := e.src.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&maxID); err != nil {
		return f, err
	}
	if err := e.src.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return f, err
	}
	if err := e.src.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id > ? AND event_id IS NULL`, cutover).Scan(&nullAbove); err != nil {
		return f, err
	}
	var problems []string
	if nullAbove > 0 {
		problems = append(problems, fmt.Sprintf("%d event(s) above the cutover %d have no event_id", nullAbove, cutover))
	}
	if next > cutover {
		problems = append(problems, fmt.Sprintf("the backfill cursor %d is above the cutover %d", next, cutover))
	}
	if remaining < 0 {
		problems = append(problems, fmt.Sprintf("remaining is negative (%d)", remaining))
	}
	if maxID.Valid && (!seq.Valid || seq.Int64 < maxID.Int64) {
		problems = append(problems, fmt.Sprintf("events' sqlite_sequence (%v) is below the largest id %d", seq.Int64, maxID.Int64))
	}
	if seq.Valid && seq.Int64 < cutover {
		problems = append(problems, fmt.Sprintf("events' sqlite_sequence %d is below the cutover %d", seq.Int64, cutover))
	}
	f.OK = len(problems) == 0
	if f.OK {
		f.Detail = fmt.Sprintf("cutover %d, backfill cursor %d, remaining %d, high water %d, largest id %d", cutover, next, remaining, seq.Int64, maxID.Int64)
		return f, nil
	}
	f.Detail = strings.Join(problems, "; ")
	return f, nil
}
