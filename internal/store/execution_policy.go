package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/tether/internal/execpolicy"
)

// GetExecutionPolicy returns the stored policy, or the zero (running, no caps)
// policy when none was ever set.
func (s *Store) GetExecutionPolicy() (execpolicy.Policy, error) {
	var raw string
	err := s.db.QueryRow(`SELECT policy_json FROM execution_policy WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return execpolicy.Policy{}, nil
	}
	if err != nil {
		return execpolicy.Policy{}, err
	}
	var p execpolicy.Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return execpolicy.Policy{}, err
	}
	return p, nil
}

// SetExecutionPolicy replaces the stored policy.
func (s *Store) SetExecutionPolicy(p execpolicy.Policy) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO execution_policy(id, policy_json, updated_at) VALUES(1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET policy_json=excluded.policy_json, updated_at=excluded.updated_at`,
		string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ExecutionUsage counts sessions holding a runtime slot: launching, running,
// detached and orphaned (the last two are still live children).
func (s *Store) ExecutionUsage() (execpolicy.Usage, error) {
	rows, err := s.db.Query(`SELECT project_id, COUNT(*) FROM sessions WHERE state IN ('launching','running','detached','orphaned') GROUP BY project_id`)
	if err != nil {
		return execpolicy.Usage{}, err
	}
	defer rows.Close()
	u := execpolicy.Usage{ByProject: map[string]int{}}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return execpolicy.Usage{}, err
		}
		u.ByProject[id] = n
		u.Total += n
	}
	return u, rows.Err()
}
