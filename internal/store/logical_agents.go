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
	// LaunchID is the most recently used launch profile for this agent.
	// Set by LaunchSession; used by the resume endpoint to start a new session
	// with the same catalog configuration. Empty until the agent's first launch.
	LaunchID string
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
		r                                                                      LogicalAgentRow
		role, name, resp, caps, mem, pol, tools, esc, chkpt, hotCold, launchID sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT id, role, name, responsibilities, capabilities, memory_scopes,
                policies_json, permitted_tools, escalation_rules,
                checkpoint_policy, hot_cold_policy, created_at, updated_at, launch_id
           FROM logical_agents WHERE id=?`,
		id,
	).Scan(&r.ID, &role, &name, &resp, &caps, &mem, &pol, &tools, &esc, &chkpt, &hotCold, &r.CreatedAt, &r.UpdatedAt, &launchID)
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
	r.LaunchID = launchID.String
	return &r, nil
}

// SetLogicalAgentLaunchID stores the launch profile ID most recently used
// to start a session for this agent. Used by the resume endpoint to start
// a new session with the same catalog config. Safe to call on every launch —
// it overwrites any prior value (last-write-wins is correct; the latest
// launch profile is the most meaningful one for resume).
func (s *Store) SetLogicalAgentLaunchID(agentID, launchID string) error {
	res, err := s.db.Exec(
		`UPDATE logical_agents SET launch_id=? WHERE id=?`,
		nullIfEmpty(launchID), agentID,
	)
	if err != nil {
		return fmt.Errorf("set launch_id on logical_agent %q: %w", agentID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("set launch_id: no logical_agents row with id %q", agentID)
	}
	return nil
}

// ListLogicalAgents returns all rows ordered by id.
func (s *Store) ListLogicalAgents() ([]LogicalAgentRow, error) {
	rows, err := s.db.Query(
		`SELECT id, role, name, responsibilities, capabilities, memory_scopes,
                policies_json, permitted_tools, escalation_rules,
                checkpoint_policy, hot_cold_policy, created_at, updated_at, launch_id
           FROM logical_agents ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list logical_agents: %w", err)
	}
	defer rows.Close()
	var out []LogicalAgentRow
	for rows.Next() {
		var (
			r                                                                      LogicalAgentRow
			role, name, resp, caps, mem, pol, tools, esc, chkpt, hotCold, launchID sql.NullString
		)
		if err := rows.Scan(&r.ID, &role, &name, &resp, &caps, &mem, &pol, &tools, &esc, &chkpt, &hotCold, &r.CreatedAt, &r.UpdatedAt, &launchID); err != nil {
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
		r.LaunchID = launchID.String
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

// SetClaudeSessionID stores the claude CLI `session_id` observed on a
// claudestream turn's `system/init` event so a later resume can pass
// `--resume <id>` to the subprocess. No-op with nil error if id is ""
// (avoids clobbering a valid cached id with a transient empty signal).
// Does NOT touch updated_at — this field is an opaque resume token,
// not user-visible state; mutating updated_at here would reorder UI
// lists on every claudestream turn for no user-meaningful reason.
func (s *Store) SetClaudeSessionID(logicalAgentID, claudeSessionID string) error {
	if logicalAgentID == "" {
		return errors.New("logical agent id required")
	}
	if claudeSessionID == "" {
		return nil
	}
	res, err := s.db.Exec(
		`UPDATE logical_agents SET claude_session_id = ? WHERE id = ?`,
		claudeSessionID, logicalAgentID,
	)
	if err != nil {
		return fmt.Errorf("set claude_session_id for %q: %w", logicalAgentID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set claude_session_id for %q: rows affected: %w", logicalAgentID, err)
	}
	if n == 0 {
		return fmt.Errorf("set claude_session_id: no logical_agents row with id %q", logicalAgentID)
	}
	return nil
}

// GetClaudeSessionID returns the last-seen claude CLI session_id for
// the given logical agent, or "" if unset (the column is nullable and
// defaults to NULL until the first system/init event persists it).
// Returns sql.ErrNoRows if the logical_agents row itself is missing.
func (s *Store) GetClaudeSessionID(logicalAgentID string) (string, error) {
	var sid sql.NullString
	err := s.db.QueryRow(
		`SELECT claude_session_id FROM logical_agents WHERE id = ?`,
		logicalAgentID,
	).Scan(&sid)
	if err != nil {
		return "", err
	}
	return sid.String, nil
}
