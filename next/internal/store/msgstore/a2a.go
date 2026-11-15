package msgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

var (
	// ErrA2ATaskExists is returned when the (binding, task) is already stored.
	ErrA2ATaskExists = errors.New("a2a task already exists")
	// ErrA2ATaskNotFound is returned for an unknown task, or one owned by someone else.
	ErrA2ATaskNotFound = errors.New("a2a task not found")
	// ErrA2ATaskConflict is returned when the task changed since prevVersion.
	ErrA2ATaskConflict = errors.New("a2a task version conflict")
)

// A2ATask is one inbound A2A task (the optional A2A surface).
type A2ATask struct {
	BindingID, TaskID, Owner, ContextID, State string
	Version                                    int64
	TaskJSON                                   []byte
	UpdatedAt                                  time.Time
}

const a2aTaskColumns = `binding_id, task_id, owner, context_id, state, version, task_json, updated_ns`

func scanA2ATask(scan scanFunc) (A2ATask, error) {
	var t A2ATask
	var js string
	var ns int64
	if err := scan(&t.BindingID, &t.TaskID, &t.Owner, &t.ContextID, &t.State, &t.Version, &js, &ns); err != nil {
		return A2ATask{}, err
	}
	t.TaskJSON, t.UpdatedAt = []byte(js), time.Unix(0, ns).UTC()
	return t, nil
}

// CreateA2ATask stores a new task at version 1. The old store matched SQLite's "UNIQUE constraint
// failed" text; ON CONFLICT DO NOTHING plus the row count is the same test on both dialects.
func (s *Store) CreateA2ATask(ctx context.Context, t A2ATask) (int64, error) {
	now := s.now().UnixNano()
	n, err := s.execC(ctx, `INSERT INTO a2a_tasks (binding_id, task_id, owner, context_id, state, version, task_json, created_ns, updated_ns)
 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?) ON CONFLICT (binding_id, task_id) DO NOTHING`,
		t.BindingID, t.TaskID, t.Owner, t.ContextID, t.State, string(t.TaskJSON), now, now)
	if err != nil {
		return 0, fmt.Errorf("create a2a task: %w", err)
	}
	if n == 0 {
		return 0, ErrA2ATaskExists
	}
	return 1, nil
}

// UpdateA2ATask updates the owner's task and returns the new version. prevVersion > 0 makes it
// conditional (ErrA2ATaskConflict when the task moved on).
func (s *Store) UpdateA2ATask(ctx context.Context, t A2ATask, prevVersion int64) (int64, error) {
	var version int64
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE a2a_tasks SET context_id = ?, state = ?, task_json = ?, version = version + 1, updated_ns = ?
 WHERE binding_id = ? AND task_id = ? AND owner = ? AND (? = 0 OR version = ?)`),
			t.ContextID, t.State, string(t.TaskJSON), s.now().UnixNano(), t.BindingID, t.TaskID, t.Owner, prevVersion, prevVersion)
		if err != nil {
			return fmt.Errorf("update a2a task: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var owner string
			err := tx.QueryRowContext(ctx, rebind(tx, `SELECT owner FROM a2a_tasks WHERE binding_id = ? AND task_id = ?`), t.BindingID, t.TaskID).Scan(&owner)
			switch {
			case errors.Is(err, sql.ErrNoRows) || (err == nil && owner != t.Owner):
				return ErrA2ATaskNotFound
			case err != nil:
				return fmt.Errorf("update a2a task: %w", err)
			}
			return ErrA2ATaskConflict
		}
		return tx.QueryRowContext(ctx, rebind(tx, `SELECT version FROM a2a_tasks WHERE binding_id = ? AND task_id = ?`), t.BindingID, t.TaskID).Scan(&version)
	})
	if err != nil {
		return 0, err
	}
	return version, nil
}

// GetA2ATask returns one task.
func (s *Store) GetA2ATask(ctx context.Context, bindingID, taskID string) (A2ATask, error) {
	r := s.c.Reader()
	t, err := scanA2ATask(r.QueryRowContext(ctx, rebind(r, `SELECT `+a2aTaskColumns+` FROM a2a_tasks WHERE binding_id = ? AND task_id = ?`), bindingID, taskID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return A2ATask{}, ErrA2ATaskNotFound
	}
	return t, err
}

// ListA2ATasks returns an owner's tasks on a binding, newest first, narrowed by context and state
// when set.
func (s *Store) ListA2ATasks(ctx context.Context, bindingID, owner, contextID, state string) ([]A2ATask, error) {
	q := `binding_id = ? AND owner = ?`
	args := []any{bindingID, owner}
	if contextID != "" {
		q, args = q+` AND context_id = ?`, append(args, contextID)
	}
	if state != "" {
		q, args = q+` AND state = ?`, append(args, state)
	}
	return s.a2aTasks(ctx, q+` ORDER BY updated_ns DESC, task_id DESC`, args...)
}

// ListA2ATasksInStates returns every task in the states, ordered by binding and task.
func (s *Store) ListA2ATasksInStates(ctx context.Context, states ...string) ([]A2ATask, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, len(states))
	for i, st := range states {
		args[i] = st
	}
	return s.a2aTasks(ctx, `state IN (`+placeholders(len(states))+`) ORDER BY binding_id, task_id`, args...)
}

func (s *Store) a2aTasks(ctx context.Context, where string, args ...any) ([]A2ATask, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT `+a2aTaskColumns+` FROM a2a_tasks WHERE `+where), args...)
	if err != nil {
		return nil, fmt.Errorf("list a2a tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []A2ATask
	for rows.Next() {
		t, err := scanA2ATask(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list a2a tasks: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
