package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite driver registered by side-effect; used via database/sql.
	_ "modernc.org/sqlite"

	"github.com/chrispian/agent-mux/internal/launch"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := Migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type SessionRow struct {
	ID             string
	LaunchID       string
	ProjectID      string
	LogicalAgentID string
	ProviderID     string
	Workspace      string
	State          string
	PID            sql.NullInt64
	ExitCode       sql.NullInt64
	CreatedAt      string
	UpdatedAt      string
	EndedAt        sql.NullString
}

func (s *Store) CreateSession(row SessionRow, plan *launch.Plan) error {
	now := time.Now().UTC().Format(time.RFC3339)
	row.CreatedAt = now
	row.UpdatedAt = now
	if _, err := s.db.Exec(`INSERT INTO sessions
		(id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.LaunchID, row.ProjectID, row.LogicalAgentID, row.ProviderID,
		row.Workspace, row.State, row.CreatedAt, row.UpdatedAt); err != nil {
		return err
	}
	pb, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO launch_plans (session_id, plan_json) VALUES (?, ?)`, row.ID, string(pb))
	return err
}

func (s *Store) UpdateSessionState(id, state string, pid int, exit *int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var ended sql.NullString
	if exit != nil {
		ended = sql.NullString{String: now, Valid: true}
	}
	var exitArg sql.NullInt64
	if exit != nil {
		exitArg = sql.NullInt64{Int64: int64(*exit), Valid: true}
	}
	_, err := s.db.Exec(`UPDATE sessions SET state=?, pid=?, exit_code=?, updated_at=?, ended_at=COALESCE(?, ended_at) WHERE id=?`,
		state, pid, exitArg, now, ended, id)
	return err
}

// ListSessionsOptions narrows the ListSessions query. All fields are
// optional — the zero value matches the legacy "return every row,
// newest first" behavior.
//
// Cursor is an RFC3339 timestamp; the query returns rows strictly
// older than it. State, when non-empty, restricts to a single session
// state ('created', 'launching', 'running', 'completed', 'failed',
// 'killed'). Limit caps the page size; 0 falls back to the default
// (100) and values above the hard cap (1000) are clamped.
type ListSessionsOptions struct {
	Limit  int
	Cursor string
	State  string
}

const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

func (s *Store) ListSessions(opts ListSessionsOptions) ([]SessionRow, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	// Compose WHERE dynamically so a zero-valued opts runs the exact
	// same query as before (no filter). Keeping the SQL simple avoids
	// surprising query-planner decisions on the small v0.0.2 table.
	where := ""
	args := []any{}
	add := func(clause string, v any) {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += clause
		args = append(args, v)
	}
	if opts.Cursor != "" {
		add("created_at < ?", opts.Cursor)
	}
	if opts.State != "" {
		add("state = ?", opts.State)
	}

	q := `SELECT id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, pid, exit_code, created_at, updated_at, ended_at FROM sessions` + where + ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.LogicalAgentID, &r.ProviderID, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt, &r.UpdatedAt, &r.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetSession(id string) (*SessionRow, error) {
	var r SessionRow
	err := s.db.QueryRow(`SELECT id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, pid, exit_code, created_at, updated_at, ended_at FROM sessions WHERE id=?`, id).
		Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.LogicalAgentID, &r.ProviderID, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt, &r.UpdatedAt, &r.EndedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetLaunchPlan rehydrates the launch.Plan persisted alongside the
// session row at creation time. Used by app.Service.LaunchSession to
// replay a prepared-but-not-started session without asking the caller
// to re-resolve from the catalog (which may have changed).
func (s *Store) GetLaunchPlan(sessionID string) (*launch.Plan, error) {
	var planJSON string
	err := s.db.QueryRow(`SELECT plan_json FROM launch_plans WHERE session_id=?`, sessionID).Scan(&planJSON)
	if err != nil {
		return nil, err
	}
	var plan launch.Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, fmt.Errorf("unmarshal launch plan: %w", err)
	}
	return &plan, nil
}
