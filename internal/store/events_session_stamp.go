package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
)

// stampStoredSession fills missing attribution for the event's subject session,
// including background producers with no authenticated caller snapshot. Read on
// the writer itself: a caller's uncommitted session/mapping is visible and a
// single-connection store cannot deadlock trying to reacquire the pool.
// No payload or claimed caller identity contributes attribution.
func stampStoredSession(ctx context.Context, w eventWriter, e *events.Event) error {
	if e.SessionID == "" {
		return nil
	}
	// Match eventstamp.Rows: an unresolved caller may only be claiming the
	// subject ID. Only absent (daemon) or resolved snapshots permit inference.
	if snap, ok := callcontext.FromContext(ctx); ok && !snap.SessionResolved {
		return nil
	}
	var task, workstream, run, native string
	err := w.QueryRowContext(ctx, `SELECT COALESCE(s.task_id,''), COALESCE(s.workstream_id,''),
 COALESCE(r.run_id,''), COALESCE(m.native_session_id,'')
 FROM sessions s
 LEFT JOIN team_runs r ON r.session_group_id=s.session_group_id
 LEFT JOIN session_provider_mappings m ON m.session_id=s.id AND m.owner='tether'
 AND m.provider=COALESCE(NULLIF(?,''),s.provider_id)
 WHERE s.id=?`, e.Envelope.Process.Provider, e.SessionID).Scan(&task, &workstream, &run, &native)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stamp event session: %w", err)
	}
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	fill(&e.Envelope.Dims.TaskID, task)
	fill(&e.Envelope.Dims.WorkstreamID, workstream)
	fill(&e.Envelope.Dims.RunID, run)
	fill(&e.Envelope.Process.ProviderSessionID, native)
	return nil
}
