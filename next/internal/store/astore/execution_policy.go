package astore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/tether/next/internal/execpolicy"
	"github.com/hollis-labs/tether/next/internal/store"
)

// GetExecutionPolicy returns the stored policy, or the zero (running, no caps) policy when none was ever
// set.
func (a *Store) GetExecutionPolicy(ctx context.Context) (execpolicy.Policy, error) {
	var raw string
	err := a.s.A().Reader().QueryRowContext(ctx, `SELECT policy_json FROM execution_policy WHERE id = 1`).Scan(&raw)
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
func (a *Store) SetExecutionPolicy(ctx context.Context, p execpolicy.Policy) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	at := a.now().UTC().Format(time.RFC3339Nano)
	return a.s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO execution_policy (id, policy_json, updated_at) VALUES (1, ?, ?)
			ON CONFLICT (id) DO UPDATE SET policy_json = excluded.policy_json, updated_at = excluded.updated_at`,
			string(raw), at)
		return err
	})
}

// ExecutionUsage counts sessions holding a runtime slot: launching, running, detached and orphaned (the
// last two are still live children). sessions is class C, so it is read through the C handle.
func (a *Store) ExecutionUsage(ctx context.Context) (execpolicy.Usage, error) {
	rows, err := a.s.C().Reader().QueryContext(ctx, `SELECT project_id, COUNT(*) FROM sessions
		WHERE state IN ('launching', 'running', 'detached', 'orphaned') GROUP BY project_id`)
	if err != nil {
		return execpolicy.Usage{}, err
	}
	defer func() { _ = rows.Close() }()
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
