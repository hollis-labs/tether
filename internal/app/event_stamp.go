package app

import (
	"context"
	"errors"

	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/store"
)

// stampSessions gives the stamp stage read access to session rows.
type stampSessions struct{ db *store.Store }

func (s stampSessions) Session(ctx context.Context, id string) (eventstamp.SessionRow, bool, error) {
	row, err := s.db.GetSessionContext(ctx, id)
	if errors.Is(err, store.ErrSessionNotFound) {
		return eventstamp.SessionRow{}, false, nil
	}
	if err != nil {
		return eventstamp.SessionRow{}, false, err
	}
	return eventstamp.SessionRow{
		WorkerID: row.WorkerID, ProjectID: row.ProjectID, WorkstreamID: row.WorkstreamID.String,
		TaskID: row.TaskID.String, LaunchID: row.LaunchID, LogicalAgentID: row.LogicalAgentID,
	}, true, nil
}

// sessionFields are the session-owned fields carried on session.state_changed
// (the event stream contract's session block): who runs the session, where, and
// how it came to exist. All low-cardinality ids, read from the session's own
// row. Usage and health are not here: they change on every turn and belong on
// dedicated kinds.
type sessionFields struct {
	ProviderID      string `json:"provider_id,omitempty"`
	ProviderKind    string `json:"provider_kind,omitempty"`
	WorkerID        string `json:"worker_id,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	GroupID         string `json:"group_id,omitempty"`
	Intent          string `json:"intent,omitempty"`
}

// sessionFieldsFor reads the fields for one session; nil when the row is gone
// (the event is still published without them).
func sessionFieldsFor(db *store.Store, id string) *sessionFields {
	if db == nil || id == "" {
		return nil
	}
	row, err := db.GetSession(id)
	if err != nil || row == nil {
		return nil
	}
	return &sessionFields{
		ProviderID: row.ProviderID, ProviderKind: row.ProviderKind, WorkerID: row.WorkerID,
		ParentSessionID: row.ParentSessionID.String, GroupID: row.SessionGroupID.String, Intent: row.Intent,
	}
}
