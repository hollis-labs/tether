package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SessionGroupRow mirrors the session_groups table.
type SessionGroupRow struct {
	ID         string
	Name       string
	WorkflowID string
	CreatedAt  string
	UpdatedAt  string
}

// CreateSessionGroup inserts a new session group row.
func (s *Store) CreateSessionGroup(g SessionGroupRow) error {
	if g.ID == "" {
		return errors.New("session group id required")
	}
	_, err := s.db.Exec(
		`INSERT INTO session_groups (id, name, workflow_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		g.ID, nullIfEmpty(g.Name), nullIfEmpty(g.WorkflowID), g.CreatedAt, g.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create session_group %q: %w", g.ID, err)
	}
	return nil
}

// GetSessionGroup fetches a single session group by id.
// Returns sql.ErrNoRows if not found.
func (s *Store) GetSessionGroup(id string) (*SessionGroupRow, error) {
	var g SessionGroupRow
	var name, workflowID sql.NullString
	err := s.db.QueryRow(
		`SELECT id, name, workflow_id, created_at, updated_at FROM session_groups WHERE id=?`,
		id,
	).Scan(&g.ID, &name, &workflowID, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return nil, err
	}
	g.Name = name.String
	g.WorkflowID = workflowID.String
	return &g, nil
}

// ListSessionGroups returns all groups ordered by created_at descending.
func (s *Store) ListSessionGroups() ([]SessionGroupRow, error) {
	rows, err := s.db.Query(
		`SELECT id, name, workflow_id, created_at, updated_at FROM session_groups ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list session_groups: %w", err)
	}
	defer rows.Close()
	var out []SessionGroupRow
	for rows.Next() {
		var g SessionGroupRow
		var name, workflowID sql.NullString
		if err := rows.Scan(&g.ID, &name, &workflowID, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan session_group: %w", err)
		}
		g.Name = name.String
		g.WorkflowID = workflowID.String
		out = append(out, g)
	}
	return out, rows.Err()
}

// AddSessionToGroup sets session_group_id on the given session row.
func (s *Store) AddSessionToGroup(sessionID, groupID string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET session_group_id=? WHERE id=?`,
		groupID, sessionID,
	)
	if err != nil {
		return fmt.Errorf("add session %q to group %q: %w", sessionID, groupID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("add session to group: no session row with id %q", sessionID)
	}
	return nil
}

// ListSessionsByGroup returns all sessions in the given group, newest first.
func (s *Store) ListSessionsByGroup(groupID string) ([]SessionRow, error) {
	return s.ListSessions(ListSessionsOptions{GroupID: groupID})
}
