package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/hollis-labs/tether/internal/events"
)

// SetEventTxPublisher connects store-owned atomic producers to the same stages
// as other bus producers. It must use the caller's transaction, never Publish
// or another connection. The application installs OutboxBus.PublishTx.
func (s *Store) SetEventTxPublisher(fn func(context.Context, events.Tx, events.Event) (events.Event, error)) {
	if fn == nil {
		s.eventTxPublisher.Store(nil)
		return
	}
	s.eventTxPublisher.Store(&fn)
}

// publishOutboxEvent is for store-owned business events, not the bus persister.
// Read known dimensions on the existing transaction because the bus's Rows
// stage is deliberately not TxSafe. No actor or generation is inferred from
// payloads, session names or workspace paths.
func (s *Store) publishOutboxEvent(ctx context.Context, tx events.Tx, e events.Event) error {
	if e.SessionID != "" {
		d := &e.Envelope.Dims
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(worker_id,''),COALESCE(project_id,''),COALESCE(workstream_id,''),COALESCE(task_id,''),COALESCE(launch_id,''),COALESCE(logical_agent_id,'') FROM sessions WHERE id=?`, e.SessionID).Scan(&d.WorkerID, &d.ProjectID, &d.WorkstreamID, &d.TaskID, &d.LaunchID, &d.LogicalAgentID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		e.LogicalAgentID = d.LogicalAgentID
	}
	if fn := s.eventTxPublisher.Load(); fn != nil {
		_, err := (*fn)(ctx, tx, e)
		return err
	}
	// Bare stores remain usable by migration/fixture/library callers without a
	// running application bus. Runtime composition always installs the hook.
	_, err := s.InsertEventTx(ctx, tx, e)
	return err
}
