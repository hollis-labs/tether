package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// Session states the recovery and reaper paths read and write (session.State in the old tree).
const (
	StateLaunching = "launching"
	StateRunning   = "running"
	StateDetached  = "detached"
	StateOrphaned  = "orphaned"
)

// SessionStateChange is the committed lifecycle change, for the caller's event emission.
type SessionStateChange struct {
	SessionID      string
	LogicalAgentID string
	From           string `json:"from"`
	To             string `json:"to"`
	Reason         string `json:"reason"`
}

// MarkSessionDetached moves a live session to detached, keeping its child PID, credentials and
// bindings. It never stamps an exit code or ended_at: no exit was observed.
func (s *Store) MarkSessionDetached(ctx context.Context, id, reason string) (SessionStateChange, error) {
	return s.markSessionRecovery(ctx, id, StateDetached, reason)
}

// MarkSessionOrphaned moves a live (or already orphaned) session to orphaned, clears the vanished
// process identity and revokes its session credentials (class A) and binding generations in the same
// transaction: orphaned is not terminal, so the terminal-state credential trigger does not do it.
func (s *Store) MarkSessionOrphaned(ctx context.Context, id, reason string) (SessionStateChange, error) {
	return s.markSessionRecovery(ctx, id, StateOrphaned, reason)
}

// markSessionRecovery runs as the allowlisted joint "markSessionRecovery" (sessions in C, principals
// in A). The session row is locked before its state is read (FOR UPDATE on Postgres; SQLite's one
// writer already serializes), so a concurrent transition cannot slip between the check and the write.
func (s *Store) markSessionRecovery(ctx context.Context, id, to, reason string) (SessionStateChange, error) {
	change := SessionStateChange{SessionID: id, To: to, Reason: reason}
	if reason == "" {
		return change, fmt.Errorf("session recovery requires a reason")
	}
	err := s.all.Joint(ctx, "markSessionRecovery", func(ctx context.Context, tx store.Tx) error {
		var agent sql.NullString
		err := tx.QueryRowContext(ctx, rebind(tx, `SELECT state, logical_agent_id FROM sessions WHERE id = ?`+repForUpdate(tx)), id).Scan(&change.From, &agent)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		change.LogicalAgentID = agent.String
		switch change.From {
		case StateLaunching, StateRunning, StateDetached:
		case StateOrphaned:
			if to != StateOrphaned {
				return fmt.Errorf("cannot move orphaned session %s to %s; resume creates a new session", id, to)
			}
		default:
			return fmt.Errorf("cannot move session %s from %s to %s", id, change.From, to)
		}
		updated, revoked := s.secondStamp(), s.polMillis()
		if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE sessions SET state = ?, updated_at = ? WHERE id = ?`), to, updated, id); err != nil {
			return err
		}
		if to != StateOrphaned {
			return nil
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE sessions SET pid = NULL, pid_started_at = NULL WHERE id = ?`), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE kind = 'session' AND session_id = ?`), revoked, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, rebind(tx, `UPDATE runtime_bindings SET revoked_at = ?, updated_at = ? WHERE session_id = ? AND revoked_at IS NULL`), revoked, revoked, id)
		return err
	})
	return change, err
}

// DetachedSessionIDForAgent finds a still-live disconnected child that resume must not duplicate,
// even when the latest checkpoint came from another session ("" when none). The old query had no
// tie-break on created_at (one-second resolution) and in practice returned the first inserted of a
// same-second tie; SQLite keeps that (rowid ASC). Postgres has no insertion order and breaks ties by
// id DESC, as latestSessionOrder does (conversion notes: an insertion-order column before class C
// moves).
func (s *Store) DetachedSessionIDForAgent(ctx context.Context, agentID string) (string, error) {
	r := s.c.Reader()
	order := `created_at DESC, rowid ASC`
	if r.Dialect().Name() == gostore.DriverPostgres {
		order = `created_at DESC, id DESC`
	}
	var id string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT id FROM sessions WHERE logical_agent_id = ? AND state = 'detached' ORDER BY `+order+` LIMIT 1`), agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// repForUpdate is the row-lock suffix for a SELECT that a write in the same transaction depends on:
// FOR UPDATE on Postgres, nothing on SQLite (its one writer already serializes the transaction).
func repForUpdate(q gostore.Querier) string {
	if q.Dialect().Name() == gostore.DriverPostgres {
		return ` FOR UPDATE`
	}
	return ""
}
