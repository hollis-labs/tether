package astore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

// LogicalAgentRow mirrors the logical_agents table. Nullable text columns surface as Go strings (empty
// string for NULL), as in the old store. The policy logic over CheckpointPolicy and PoliciesJSON
// (normalize, decode, merge) belongs to the agent domain, which the slice porting it adds on top of
// these row operations.
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
	// LaunchID is the launch profile most recently used for this agent; empty until its first launch.
	LaunchID string
}

// agentTime is the logical_agents timestamp format the old callers used.
func (a *Store) agentTime() string { return a.now().UTC().Format(time.RFC3339) }

// UpsertLogicalAgent inserts or updates the row keyed by id. On insert, created_at and updated_at are now;
// on update, created_at is kept, and only role, name and updated_at change, so a startup reseed never
// clobbers the policy columns an operator set.
func (a *Store) UpsertLogicalAgent(ctx context.Context, id, role, name string) error {
	if id == "" {
		return errors.New("logical agent id required")
	}
	now := a.agentTime()
	err := a.s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO logical_agents (id, role, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET role = excluded.role, name = excluded.name, updated_at = excluded.updated_at`,
			id, nullIfEmpty(role), nullIfEmpty(name), now, now)
		return err
	})
	if err != nil {
		return fmt.Errorf("upsert logical_agent %q: %w", id, err)
	}
	return nil
}

const logicalAgentColumns = `id, role, name, responsibilities, capabilities, memory_scopes, policies_json, permitted_tools,
	escalation_rules, checkpoint_policy, hot_cold_policy, created_at, updated_at, launch_id`

type scanner interface{ Scan(dest ...any) error }

func scanLogicalAgent(sc scanner) (LogicalAgentRow, error) {
	var r LogicalAgentRow
	var role, name, resp, caps, mem, pol, tools, esc, chkpt, hotCold, launchID sql.NullString
	if err := sc.Scan(&r.ID, &role, &name, &resp, &caps, &mem, &pol, &tools, &esc, &chkpt, &hotCold, &r.CreatedAt, &r.UpdatedAt, &launchID); err != nil {
		return LogicalAgentRow{}, err
	}
	r.Role, r.Name, r.Responsibilities, r.Capabilities = role.String, name.String, resp.String, caps.String
	r.MemoryScopes, r.PoliciesJSON, r.PermittedTools, r.EscalationRules = mem.String, pol.String, tools.String, esc.String
	r.CheckpointPolicy, r.HotColdPolicy, r.LaunchID = chkpt.String, hotCold.String, launchID.String
	return r, nil
}

// GetLogicalAgent fetches one row by id; sql.ErrNoRows when there is none.
func (a *Store) GetLogicalAgent(ctx context.Context, id string) (*LogicalAgentRow, error) {
	r, err := scanLogicalAgent(a.s.A().Reader().QueryRowContext(ctx, `SELECT `+logicalAgentColumns+` FROM logical_agents WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListLogicalAgents returns every row, ordered by id.
func (a *Store) ListLogicalAgents(ctx context.Context) ([]LogicalAgentRow, error) {
	rows, err := a.s.A().Reader().QueryContext(ctx, `SELECT `+logicalAgentColumns+` FROM logical_agents ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list logical_agents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LogicalAgentRow
	for rows.Next() {
		r, err := scanLogicalAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan logical_agent: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// updateOne runs one UPDATE of a logical_agents row and fails when no row has the id.
func (a *Store) updateOne(ctx context.Context, what, id, q string, args ...any) error {
	return a.s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("%s %q: %w", what, id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("%s %q: rows affected: %w", what, id, err)
		}
		if n == 0 {
			return fmt.Errorf("%s: no logical_agents row with id %q", what, id)
		}
		return nil
	})
}

// SetLogicalAgentPolicyColumns stores an already normalized checkpoint policy and policies JSON (the
// agent domain computes both) and stamps updated_at.
func (a *Store) SetLogicalAgentPolicyColumns(ctx context.Context, id, checkpointPolicy, policiesJSON string) error {
	return a.updateOne(ctx, "update logical_agent policy", id,
		`UPDATE logical_agents SET checkpoint_policy = ?, policies_json = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(checkpointPolicy), nullIfEmpty(policiesJSON), a.agentTime(), id)
}

// SetLogicalAgentLaunchID stores the launch profile most recently used for this agent (last write wins).
func (a *Store) SetLogicalAgentLaunchID(ctx context.Context, id, launchID string) error {
	return a.updateOne(ctx, "set launch_id on logical_agent", id,
		`UPDATE logical_agents SET launch_id = ? WHERE id = ?`, nullIfEmpty(launchID), id)
}

// SetClaudeSessionID stores the claude CLI session_id seen on a turn's system/init event, for a later
// --resume. An empty id is a no-op (a transient empty signal must not clobber a valid id). It does not
// touch updated_at: the id is an opaque resume token, not user-visible state.
func (a *Store) SetClaudeSessionID(ctx context.Context, id, claudeSessionID string) error {
	if id == "" {
		return errors.New("logical agent id required")
	}
	if claudeSessionID == "" {
		return nil
	}
	return a.updateOne(ctx, "set claude_session_id", id,
		`UPDATE logical_agents SET claude_session_id = ? WHERE id = ?`, claudeSessionID, id)
}

// GetClaudeSessionID returns the last-seen claude CLI session_id, or "" when unset; sql.ErrNoRows when the
// row itself is missing.
func (a *Store) GetClaudeSessionID(ctx context.Context, id string) (string, error) {
	var sid sql.NullString
	if err := a.s.A().Reader().QueryRowContext(ctx, `SELECT claude_session_id FROM logical_agents WHERE id = ?`, id).Scan(&sid); err != nil {
		return "", err
	}
	return sid.String, nil
}
