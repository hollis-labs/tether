package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hollis-labs/go-messaging/delivery"

	// Pure-Go SQLite driver registered by side-effect; used via database/sql.
	_ "modernc.org/sqlite"

	"github.com/hollis-labs/tether/internal/launch"
)

// ErrSessionNotFound is returned by GetSession when no row matches the
// given session ID. Callers should use errors.Is to check for this error
// rather than inspecting the error message string.
var ErrSessionNotFound = errors.New("session not found")

type Store struct {
	db *sql.DB
	// msgOnce + msgStore ensure MessagingStore() returns the same in-memory
	// fan-out instance on every call within a process so Subscribe/Send
	// cross-talk works. See messaging_store.go.
	msgOnce  sync.Once
	msgStore InboxStore
	// deliveryOnce + deliveryStoreVal mirror msgOnce/msgStore for the
	// go-messaging delivery-core singleton (T03). See delivery_store.go.
	deliveryOnce     sync.Once
	deliveryStoreVal delivery.Store
}

// Open opens (or creates) the SQLite store at path and runs migrations.
//
// The connection is configured with:
//   - WAL journal mode: allows concurrent readers + one writer without
//     blocking reads. Essential for the messaging fan-out path where
//     Subscribe goroutines read while Send goroutines write.
//   - busy_timeout=5000ms: SQLite retries a locked write for up to 5s
//     before returning SQLITE_BUSY. This prevents SQLITE_BUSY propagating
//     to callers under brief write contention (e.g. concurrent Inbox calls).
//   - MaxOpenConns=1: SQLite does not support concurrent writers; serializing
//     at the connection pool level prevents SQLITE_LOCKED races between
//     goroutines sharing the same *sql.DB.
//
// Note: foreign_keys enforcement is deliberately left OFF (see ADR-0008 and
// migration 0004_checkpoints.sql). FK declarations in the schema are
// documentation only until the project explicitly enables enforcement.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	// DSN parameters are supported by modernc.org/sqlite.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Intentionally set to a single connection to serialize all access — reads
	// and writes alike. Although SQLite WAL allows N readers + 1 writer, using
	// MaxOpenConns=1 avoids SQLITE_BUSY/SQLITE_LOCKED errors that occur when
	// multiple goroutines compete for write transactions. The embedded daemon
	// workload is not read-heavy enough to justify the complexity of a
	// separate read pool.
	db.SetMaxOpenConns(1)
	if _, err := Migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// go-messaging's own reliable-delivery schema (T03, messaging vNext).
	// ApplySQLiteSchema is idempotent (CREATE TABLE/INDEX IF NOT EXISTS plus
	// an ON CONFLICT-upserted version row) and owns its own tables, all
	// prefixed messaging_* -- no collision with Tether's own `messages`
	// table. Applied here rather than baked into a numbered Tether
	// migration file so the library remains the owner of its own schema
	// evolution, per the architecture's "reconcile the two current
	// contracts; do not create a third competing bus."
	if err := delivery.ApplySQLiteSchema(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply delivery schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB handle. Exposed for subsystems that
// need to attach their own typed storage layer over the same database
// (e.g. the registry service in internal/registry). Callers MUST NOT
// Close the returned handle — Store.Close owns the lifecycle.
func (s *Store) DB() *sql.DB { return s.db }

type SessionRow struct {
	ID             string
	LaunchID       string
	ProjectID      string
	LogicalAgentID string
	ProviderID     string
	// ProviderKind is the runtime family ("cli" | "api"). Added in migration
	// 0012 (ADR 0022 G3); empty for rows created before the migration.
	ProviderKind string
	Workspace    string
	State        string
	PID          sql.NullInt64
	ExitCode     sql.NullInt64
	CreatedAt    string
	UpdatedAt    string
	EndedAt      sql.NullString
	// ParentSessionID names the session this one continues from (set on
	// resume). Empty for a fresh boot with no lineage. Added migration 0019
	// (T02, messaging vNext) -- Tether had no session-lineage concept before
	// this; see planning/docs/messaging-vnext/T01-compatibility-contract.md §2.12.
	ParentSessionID sql.NullString
	// Intent records how this session came to exist: one of "preassigned",
	// "resume", "compact", "fresh", "fork" (mirrors agentkit's
	// SessionBootstrapIntent vocabulary; "compact"/"fork" are reserved for
	// launch paths that don't exist in Tether yet -- see T02 research, no
	// compaction or fork concept exists today). Empty defaults to "fresh".
	Intent string
	// Publication records whether this session was ever explicitly
	// published: "private-local" (default), "published-local", or
	// "tether-hosted". Empty defaults to "private-local".
	Publication    string
	SessionGroupID sql.NullString
}

func (s *Store) CreateSession(row SessionRow, plan *launch.Plan) error {
	now := time.Now().UTC().Format(time.RFC3339)
	row.CreatedAt = now
	row.UpdatedAt = now
	if row.Intent == "" {
		row.Intent = "fresh"
	}
	if row.Publication == "" {
		row.Publication = "private-local"
	}
	if _, err := s.db.Exec(`INSERT INTO sessions
		(id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, created_at, updated_at, parent_session_id, intent, publication)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.LaunchID, row.ProjectID, row.LogicalAgentID, row.ProviderID,
		row.ProviderKind, row.Workspace, row.State, row.CreatedAt, row.UpdatedAt,
		row.ParentSessionID, row.Intent, row.Publication); err != nil {
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

// SweepStaleSessions marks any session stuck in 'launching' or 'running'
// as 'failed'. Intended for call-once-on-daemon-start: after a crash, any
// session still in a live state is an orphan. Exit code -1 signals
// "daemon lost contact" rather than a natural process exit. Returns the
// number of rows updated.
func (s *Store) SweepStaleSessions(now string) (int, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET state='failed', exit_code=-1, ended_at=?, updated_at=?
		 WHERE state IN ('launching', 'running')`,
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("sweep stale sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// ListSessionsOptions narrows the ListSessions query. All fields are
// optional — the zero value matches the legacy "return every row,
// newest first" behavior.
//
// Cursor is an RFC3339 timestamp; the query returns rows strictly
// older than it. State, when non-empty, restricts to a single session
// state ('created', 'launching', 'running', 'completed', 'failed',
// 'killed'). GroupID, when non-empty, restricts to sessions in the
// named session group. Limit caps the page size; 0 falls back to the
// default (100) and values above the hard cap (1000) are clamped.
type ListSessionsOptions struct {
	Limit   int
	Cursor  string
	State   string
	GroupID string
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
	if opts.GroupID != "" {
		add("session_group_id = ?", opts.GroupID)
	}

	q := `SELECT id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, pid, exit_code, created_at, updated_at, ended_at, session_group_id, parent_session_id, intent, publication FROM sessions` + where + ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.LogicalAgentID, &r.ProviderID, &r.ProviderKind, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt, &r.UpdatedAt, &r.EndedAt, &r.SessionGroupID, &r.ParentSessionID, &r.Intent, &r.Publication); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetSession(id string) (*SessionRow, error) {
	var r SessionRow
	err := s.db.QueryRow(`SELECT id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, pid, exit_code, created_at, updated_at, ended_at, session_group_id, parent_session_id, intent, publication FROM sessions WHERE id=?`, id).
		Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.LogicalAgentID, &r.ProviderID, &r.ProviderKind, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt, &r.UpdatedAt, &r.EndedAt, &r.SessionGroupID, &r.ParentSessionID, &r.Intent, &r.Publication)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
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
