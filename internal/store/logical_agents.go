package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/chrispian/agent-mux/internal/agent"
)

// LogicalAgentRow mirrors the logical_agents table. All nullable TEXT
// columns surface as Go strings (empty string ↔ SQL NULL) so callers
// don't have to reach through sql.NullString for v0.0.2 ergonomics; the
// nullable fields carry no semantics beyond "not populated" today.
type LogicalAgentRow struct {
	ID               string
	Role             string
	Name             string
	Responsibilities string
	Capabilities     string
	MemoryScopes     string
	PoliciesJSON     string
	PermittedTools   string
	EscalationRules  string
	CheckpointPolicy string
	HotColdPolicy    string
	CreatedAt        string
	UpdatedAt        string
}

// UpsertLogicalAgent inserts or updates a logical_agents row keyed by
// id. On insert, both created_at and updated_at are set to now; on
// update, created_at is preserved and updated_at is set to now. Only
// id/role/name are overwritten on update — nullable policy columns
// remain untouched so future operator-set values aren't clobbered by a
// startup reseed. now must be an RFC3339 timestamp (caller formats).
func (s *Store) UpsertLogicalAgent(a agent.LogicalAgent, now string) error {
	if a.ID == "" {
		return errors.New("logical agent id required")
	}
	_, err := s.db.Exec(
		`INSERT INTO logical_agents (id, role, name, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET
            role = excluded.role,
            name = excluded.name,
            updated_at = excluded.updated_at`,
		a.ID, nullIfEmpty(a.Role), nullIfEmpty(a.Name), now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert logical_agent %q: %w", a.ID, err)
	}
	return nil
}

// GetLogicalAgent fetches a single row by id. Returns sql.ErrNoRows if
// no such row exists.
func (s *Store) GetLogicalAgent(id string) (*LogicalAgentRow, error) {
	var (
		r                                                                                                LogicalAgentRow
		role, name, resp, caps, mem, pol, tools, esc, chkpt, hotCold                                     sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT id, role, name, responsibilities, capabilities, memory_scopes,
                policies_json, permitted_tools, escalation_rules,
                checkpoint_policy, hot_cold_policy, created_at, updated_at
           FROM logical_agents WHERE id=?`,
		id,
	).Scan(&r.ID, &role, &name, &resp, &caps, &mem, &pol, &tools, &esc, &chkpt, &hotCold, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.Role = role.String
	r.Name = name.String
	r.Responsibilities = resp.String
	r.Capabilities = caps.String
	r.MemoryScopes = mem.String
	r.PoliciesJSON = pol.String
	r.PermittedTools = tools.String
	r.EscalationRules = esc.String
	r.CheckpointPolicy = chkpt.String
	r.HotColdPolicy = hotCold.String
	return &r, nil
}

// ListLogicalAgents returns all rows ordered by id.
func (s *Store) ListLogicalAgents() ([]LogicalAgentRow, error) {
	rows, err := s.db.Query(
		`SELECT id, role, name, responsibilities, capabilities, memory_scopes,
                policies_json, permitted_tools, escalation_rules,
                checkpoint_policy, hot_cold_policy, created_at, updated_at
           FROM logical_agents ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list logical_agents: %w", err)
	}
	defer rows.Close()
	var out []LogicalAgentRow
	for rows.Next() {
		var (
			r                                                                                                LogicalAgentRow
			role, name, resp, caps, mem, pol, tools, esc, chkpt, hotCold                                     sql.NullString
		)
		if err := rows.Scan(&r.ID, &role, &name, &resp, &caps, &mem, &pol, &tools, &esc, &chkpt, &hotCold, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan logical_agent: %w", err)
		}
		r.Role = role.String
		r.Name = name.String
		r.Responsibilities = resp.String
		r.Capabilities = caps.String
		r.MemoryScopes = mem.String
		r.PoliciesJSON = pol.String
		r.PermittedTools = tools.String
		r.EscalationRules = esc.String
		r.CheckpointPolicy = chkpt.String
		r.HotColdPolicy = hotCold.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
