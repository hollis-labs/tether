package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors returned by the A2A task persistence methods.
var (
	ErrA2ATaskExists   = errors.New("a2a task already exists")
	ErrA2ATaskNotFound = errors.New("a2a task not found")
	// ErrA2ATaskConflict reports an update whose expected version no longer
	// matches the stored one.
	ErrA2ATaskConflict = errors.New("a2a task was modified concurrently")
)

// A2ATask is one persisted A2A task record. TaskJSON is the A2A SDK's own
// encoding of the task and is opaque to the store; State, ContextID and Owner
// are copies lifted out of it so they can be filtered on.
type A2ATask struct {
	BindingID string
	TaskID    string
	Owner     string
	ContextID string
	State     string
	// Version is the optimistic-concurrency counter: 1 on create, +1 per update.
	Version   int64
	TaskJSON  []byte
	UpdatedAt time.Time
}

const a2aTaskColumns = `binding_id, task_id, owner, context_id, state, version, task_json, updated_ns`

func scanA2ATask(row interface{ Scan(...any) error }) (A2ATask, error) {
	var (
		t    A2ATask
		js   string
		upNs int64
	)
	if err := row.Scan(&t.BindingID, &t.TaskID, &t.Owner, &t.ContextID, &t.State, &t.Version, &js, &upNs); err != nil {
		return A2ATask{}, err
	}
	t.TaskJSON = []byte(js)
	t.UpdatedAt = time.Unix(0, upNs).UTC()
	return t, nil
}

// CreateA2ATask inserts a new task at version 1 and returns that version.
// A task id already present for the binding is ErrA2ATaskExists.
func (s *Store) CreateA2ATask(t A2ATask) (int64, error) {
	now := time.Now().UnixNano()
	_, err := s.db.Exec(`INSERT INTO a2a_tasks
		(binding_id, task_id, owner, context_id, state, version, task_json, created_ns, updated_ns)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		t.BindingID, t.TaskID, t.Owner, t.ContextID, t.State, string(t.TaskJSON), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, ErrA2ATaskExists
		}
		return 0, fmt.Errorf("create a2a task: %w", err)
	}
	return 1, nil
}

// UpdateA2ATask replaces a task's record and returns its new version. The
// task must belong to t.Owner, else it is ErrA2ATaskNotFound (a task is
// invisible to another owner). When prevVersion is non-zero and is not the
// stored version, nothing is written and the result is ErrA2ATaskConflict.
func (s *Store) UpdateA2ATask(t A2ATask, prevVersion int64) (int64, error) {
	res, err := s.db.Exec(`UPDATE a2a_tasks
		SET context_id = ?, state = ?, task_json = ?, version = version + 1, updated_ns = ?
		WHERE binding_id = ? AND task_id = ? AND owner = ? AND (? = 0 OR version = ?)`,
		t.ContextID, t.State, string(t.TaskJSON), time.Now().UnixNano(),
		t.BindingID, t.TaskID, t.Owner, prevVersion, prevVersion)
	if err != nil {
		return 0, fmt.Errorf("update a2a task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var owner string
		err := s.db.QueryRow(`SELECT owner FROM a2a_tasks WHERE binding_id = ? AND task_id = ?`,
			t.BindingID, t.TaskID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != t.Owner) {
			return 0, ErrA2ATaskNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("update a2a task: %w", err)
		}
		return 0, ErrA2ATaskConflict
	}
	var v int64
	if err := s.db.QueryRow(`SELECT version FROM a2a_tasks WHERE binding_id = ? AND task_id = ?`,
		t.BindingID, t.TaskID).Scan(&v); err != nil {
		return 0, fmt.Errorf("update a2a task: %w", err)
	}
	return v, nil
}

// GetA2ATask returns one task of a binding, or ErrA2ATaskNotFound.
func (s *Store) GetA2ATask(bindingID, taskID string) (A2ATask, error) {
	t, err := scanA2ATask(s.db.QueryRow(`SELECT `+a2aTaskColumns+`
		FROM a2a_tasks WHERE binding_id = ? AND task_id = ?`, bindingID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return A2ATask{}, ErrA2ATaskNotFound
	}
	return t, err
}

// ListA2ATasks returns a binding's tasks for one owner, most recently updated
// first. A non-empty contextID or state narrows the result.
func (s *Store) ListA2ATasks(bindingID, owner, contextID, state string) ([]A2ATask, error) {
	q := `SELECT ` + a2aTaskColumns + ` FROM a2a_tasks WHERE binding_id = ? AND owner = ?`
	args := []any{bindingID, owner}
	if contextID != "" {
		q += ` AND context_id = ?`
		args = append(args, contextID)
	}
	if state != "" {
		q += ` AND state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY updated_ns DESC, task_id DESC`
	return s.queryA2ATasks(q, args...)
}

// ListA2ATasksInStates returns the tasks of every binding that are in one of
// states. It backs the startup sweep of tasks a restart left in flight.
func (s *Store) ListA2ATasksInStates(states ...string) ([]A2ATask, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, len(states))
	for i, st := range states {
		args[i] = st
	}
	q := `SELECT ` + a2aTaskColumns + ` FROM a2a_tasks WHERE state IN (?` +
		strings.Repeat(`,?`, len(states)-1) + `) ORDER BY binding_id, task_id`
	return s.queryA2ATasks(q, args...)
}

func (s *Store) queryA2ATasks(q string, args ...any) ([]A2ATask, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list a2a tasks: %w", err)
	}
	defer rows.Close()
	var out []A2ATask
	for rows.Next() {
		t, err := scanA2ATask(rows)
		if err != nil {
			return nil, fmt.Errorf("list a2a tasks: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
