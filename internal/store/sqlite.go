package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/chrispian/agent-mux/internal/launch"
)

//go:embed schema.sql
var schema string

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type SessionRow struct {
	ID         string
	LaunchID   string
	ProjectID  string
	AgentID    string
	ProviderID string
	Workspace  string
	State      string
	PID        sql.NullInt64
	ExitCode   sql.NullInt64
	CreatedAt  string
	UpdatedAt  string
	EndedAt    sql.NullString
}

func (s *Store) CreateSession(row SessionRow, plan *launch.Plan) error {
	now := time.Now().UTC().Format(time.RFC3339)
	row.CreatedAt = now
	row.UpdatedAt = now
	if _, err := s.db.Exec(`INSERT INTO sessions
		(id, launch_id, project_id, agent_id, provider_id, workspace, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.LaunchID, row.ProjectID, row.AgentID, row.ProviderID,
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

func (s *Store) ListSessions() ([]SessionRow, error) {
	rows, err := s.db.Query(`SELECT id, launch_id, project_id, agent_id, provider_id, workspace, state, pid, exit_code, created_at, updated_at, ended_at FROM sessions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.AgentID, &r.ProviderID, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt, &r.UpdatedAt, &r.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetSession(id string) (*SessionRow, error) {
	var r SessionRow
	err := s.db.QueryRow(`SELECT id, launch_id, project_id, agent_id, provider_id, workspace, state, pid, exit_code, created_at, updated_at, ended_at FROM sessions WHERE id=?`, id).
		Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.AgentID, &r.ProviderID, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt, &r.UpdatedAt, &r.EndedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) LogEvent(sessionID, kind, payload string) error {
	_, err := s.db.Exec(`INSERT INTO events (session_id, at, kind, payload) VALUES (?, ?, ?, ?)`,
		sessionID, time.Now().UTC().Format(time.RFC3339), kind, payload)
	return err
}
