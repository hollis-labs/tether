package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/session"
)

// SessionStateChange is the committed lifecycle change for daemon event emission.
type SessionStateChange struct {
	SessionID      string
	LogicalAgentID string
	From           string `json:"from"`
	To             string `json:"to"`
	Reason         string `json:"reason"`
}

// MarkSessionDetached preserves the live child PID and its credentials/bindings.
// It never stamps an exit code or ended_at: no exit was observed.
func (s *Store) MarkSessionDetached(id, reason string) (SessionStateChange, error) {
	return s.markSessionRecovery(id, session.StateDetached, reason)
}

// MarkSessionOrphaned clears the vanished process identity and explicitly revokes
// all credentials and binding generations in the same transaction. Orphaned is
// non-terminal, so the terminal-state credential trigger cannot do this for us.
func (s *Store) MarkSessionOrphaned(id, reason string) (SessionStateChange, error) {
	return s.markSessionRecovery(id, session.StateOrphaned, reason)
}

func (s *Store) markSessionRecovery(id string, to session.State, reason string) (SessionStateChange, error) {
	change := SessionStateChange{SessionID: id, To: string(to), Reason: reason}
	if reason == "" {
		return change, fmt.Errorf("session recovery requires a reason")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return change, err
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRow(`SELECT state, COALESCE(logical_agent_id,'') FROM sessions WHERE id=?`, id).Scan(&change.From, &change.LogicalAgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return change, ErrSessionNotFound
	}
	if err != nil {
		return change, err
	}
	switch session.State(change.From) {
	case session.StateLaunching, session.StateRunning, session.StateDetached:
	case session.StateOrphaned:
		if to != session.StateOrphaned {
			return change, fmt.Errorf("cannot move orphaned session %s to %s; resume creates a new session", id, to)
		}
	default:
		return change, fmt.Errorf("cannot move session %s from %s to %s", id, change.From, to)
	}
	now := time.Now().UTC()
	updated := now.Format(time.RFC3339)
	revoked := now.Format("2006-01-02T15:04:05.000Z")
	if _, err = tx.Exec(`UPDATE sessions SET state=?, updated_at=? WHERE id=?`, to, updated, id); err != nil {
		return change, err
	}
	if to == session.StateOrphaned {
		if _, err = tx.Exec(`UPDATE sessions SET pid=NULL, pid_started_at=NULL WHERE id=?`, id); err != nil {
			return change, err
		}
		if _, err = tx.Exec(`UPDATE principals SET revoked_at=COALESCE(revoked_at, ?) WHERE kind='session' AND session_id=?`, revoked, id); err != nil {
			return change, err
		}
		if _, err = tx.Exec(`UPDATE runtime_bindings SET revoked_at=?, updated_at=? WHERE session_id=? AND revoked_at IS NULL`, revoked, revoked, id); err != nil {
			return change, err
		}
	}
	return change, tx.Commit()
}

// DetachedSessionIDForAgent finds a still-live disconnected child that resume
// must not duplicate, even when the latest checkpoint came from another session.
func (s *Store) DetachedSessionIDForAgent(ctx context.Context, agentID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE logical_agent_id=? AND state='detached' ORDER BY created_at DESC LIMIT 1`, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
