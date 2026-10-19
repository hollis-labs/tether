package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	// Pure-Go SQLite driver registered by side-effect; used via database/sql.
	_ "modernc.org/sqlite"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

// ErrSessionNotFound is returned by GetSession when no row matches the
// given session ID. Callers should use errors.Is to check for this error
// rather than inspecting the error message string.
var ErrSessionNotFound = errors.New("session not found")

type Store struct {
	// eventsCommitted is called after a transaction that wrote outbox event rows
	// commits (see events_outbox.go).
	eventsCommitted  atomic.Pointer[func()]
	eventTxPublisher atomic.Pointer[func(context.Context, events.Tx, events.Event) (events.Event, error)]
	db               *sql.DB
	stateDBPath      string
	channelAuthMu    sync.RWMutex
	channelAuth      channels.Authorization
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
	if err := seedOpenTemplate(path); err != nil {
		return nil, err
	}
	return openDB(path)
}

// openDB opens path and brings its schema up to date. Open adds the directory
// and the optional test template in front of it.
func openDB(path string) (*Store, error) {
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
	// Fence pre-activation library history once. On a fresh database the high
	// water is zero; reopening never moves an established checkpoint.
	if _, err := db.Exec(`UPDATE message_event_checkpoints SET position=(SELECT coalesce(max(id),0) FROM messaging_transitions) WHERE source='delivery' AND position=-1`); err != nil {
		db.Close()
		return nil, fmt.Errorf("activate message events: %w", err)
	}
	if err := installReplacementDeliveryFences(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("install replacement delivery fences: %w", err)
	}
	return &Store{db: db, stateDBPath: path}, nil
}

// useTemplate is false in production. A test package's TestMain turns it on
// (UseMigratedTemplate) and the first Open then builds openTemplate, the bytes
// of a closed, fully migrated empty database, which later Opens of a path that
// does not exist yet start from instead of replaying every migration. Under
// -race the replay costs ~1.8s per Open (SQLite re-parses the schema after each
// statement through the race detector's pointer checks) and the test suites open
// hundreds of databases. Built lazily, because a test binary is also re-executed
// as a helper subprocess that runs TestMain but may never open a store.
var (
	useTemplate  bool
	templateOnce sync.Once
	openTemplate []byte
	templateErr  error
)

// seedOpenTemplate copies the template to path when path does not exist yet.
// With the template off, or an existing file, it does nothing, so Open behaves
// exactly as before. Open still runs Migrate and the delivery schema afterwards,
// which are no-ops on the template.
func seedOpenTemplate(path string) error {
	if !useTemplate {
		return nil
	}
	if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	templateOnce.Do(func() { openTemplate, templateErr = buildTemplate() })
	if templateErr != nil {
		return fmt.Errorf("migrated template: %w", templateErr)
	}
	return os.WriteFile(path, openTemplate, 0o600)
}

func buildTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "store-template-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "template.db")
	s, err := openDB(path)
	if err != nil {
		return nil, err
	}
	// Closing the last connection checkpoints the WAL into the main file.
	if err := s.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path) //nolint:gosec // G304: the file this function just created
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
	WorkerID       string
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
	// WorkstreamID names the durable container this session's work belongs
	// to. Nullable: a session is not required to have one. Set explicitly,
	// or inherited from parent_session_id on a resume/compact/fork -- see
	// inheritWorkstreamID in workstreams.go. Added migration 0023
	// (S1, CW-20260912-0059).
	WorkstreamID sql.NullString
	// RefAttribution records whether this session's proxy could ever produce
	// a source='proxy' ref: "proxy", "none", "unlaunched", or empty for a row
	// that predates the stamp (read as "unknown"). Written at the planting
	// site from the value the planting itself produced, never inferred from
	// launch_id -- that inference is false for every session that exists
	// today. Added migration 0025 (S5, CW-20260912-0063).
	RefAttribution sql.NullString
	// RouteJSON is the resolved launch route; NULL means routing is disabled.
	RouteJSON sql.NullString
	// TaskID is the Torque task the launcher named when it created the session
	// (CW-20261012-0062a): the bare Torque id, never transformed, set only at
	// creation by a launcher principal and never rebound. NULL when none was named.
	// Migration 0079.
	TaskID sql.NullString
}

func (s *Store) CreateSession(row SessionRow, plan *launch.Plan) error {
	return s.CreateSessionKeyed(row, plan, nil)
}

// CreateSessionKeyed is CreateSession plus an optional idempotency record,
// written in the same transaction as the session row, so a key is never bound
// to a session that was not persisted and a session is never persisted
// without the key its request carried. A nil key writes no record.
func (s *Store) CreateSessionKeyed(row SessionRow, plan *launch.Plan, key *SessionIdempotency) error {
	now := time.Now().UTC().Format(time.RFC3339)
	row.CreatedAt = now
	row.UpdatedAt = now
	if row.Intent == "" {
		row.Intent = "fresh"
	}
	if row.Publication == "" {
		row.Publication = "private-local"
	}
	// Lineage inheritance happens HERE, not in the callers. CreateSession is
	// the single path every session row reaches the DB through (the launch
	// path, resume, and /sessions/bootstrap all land here), so a
	// resume/compact/fork child inherits its parent's workstream whether or
	// not the caller thought about it. That is the whole point of S1: a
	// compaction creates a new session row, and anything that has to be
	// re-attached by hand is orphaned by the exact event it was meant to
	// survive. See workstreams.go.
	row.WorkstreamID = s.inheritWorkstreamID(row)
	// ref_attribution is INSERTED, not only updated later. A caller that knows
	// at creation time that nothing will be planted for this session -- the
	// /sessions/bootstrap path -- says so on the row, and leaving the column
	// out of this statement would silently discard that, which is the
	// declared-but-never-assigned shape this sprint has already produced twice.
	// The launch path leaves it NULL here and stamps it after planting, since
	// what was planted is not known until it has been.
	var storedPlan *launch.Plan
	row.RouteJSON = sql.NullString{}
	if plan != nil {
		route, err := launchprofile.ResolveRoute(plan.Route)
		if err != nil {
			return err
		}
		snapshot := *plan
		snapshot.Route = route
		storedPlan = &snapshot
		if route != nil {
			raw, err := json.Marshal(route)
			if err != nil {
				return err
			}
			row.RouteJSON = sql.NullString{String: string(raw), Valid: true}
		}
	}
	pb, err := json.Marshal(storedPlan)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO sessions
		(id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, created_at, updated_at, parent_session_id, intent, publication, workstream_id, ref_attribution, route_json, worker_id, task_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.LaunchID, row.ProjectID, row.LogicalAgentID, row.ProviderID,
		row.ProviderKind, row.Workspace, row.State, row.CreatedAt, row.UpdatedAt,
		row.ParentSessionID, row.Intent, row.Publication, row.WorkstreamID, row.RefAttribution, row.RouteJSON, row.WorkerID, row.TaskID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO launch_plans (session_id, plan_json) VALUES (?, ?)`, row.ID, string(pb)); err != nil {
		return err
	}
	if key != nil {
		if err := insertSessionIdempotency(tx, row.ID, *key); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// StaleSession carries the state and process identity needed by startup
// recovery for a launching, running or detached session.
type StaleSession struct {
	ID        string
	State     string
	PID       int
	CreatedAt string
	// PIDStartedAt is the start time the OS reported for PID when it was
	// recorded (RFC3339, UTC, second precision), or "" for a session
	// launched before migration 0033.
	PIDStartedAt string
}

// SweepStaleSessions settles every session the previous daemon left in
// 'launching', 'running' or 'detached'. A session spare approves keeps its
// state. Other launching/running rows become 'failed' with exit_code -1,
// which means "swept at daemon
// start", not an observed exit (CW-20260912-0085). spare may be nil, which
// spares none. Intended for call-once-on-daemon-start. Returns the swept and
// spared session ids. Detached sessions not explicitly spared become orphaned
// with reason shim_reconcile_disabled; orphaned rows are left alone.
func (s *Store) SweepStaleSessions(now string, spare func(StaleSession) bool) (swept, spared []string, err error) {
	rows, err := s.db.Query(`SELECT id, state, COALESCE(pid, 0), created_at, COALESCE(pid_started_at, '') FROM sessions WHERE worker_id='' AND state IN ('launching', 'running', 'detached') ORDER BY created_at`)
	if err != nil {
		return nil, nil, fmt.Errorf("sweep stale sessions: list: %w", err)
	}
	var stale []StaleSession
	for rows.Next() {
		var r StaleSession
		if err := rows.Scan(&r.ID, &r.State, &r.PID, &r.CreatedAt, &r.PIDStartedAt); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("sweep stale sessions: scan: %w", err)
		}
		stale = append(stale, r)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, r := range stale {
		if spare != nil && spare(r) {
			spared = append(spared, r.ID)
			continue
		}
		if r.State == "detached" {
			if _, err := s.MarkSessionOrphaned(r.ID, "shim_reconcile_disabled"); err != nil {
				return swept, spared, err
			}
			swept = append(swept, r.ID)
			continue
		}
		res, err := s.db.Exec(
			`UPDATE sessions SET state='failed', exit_code=-1, ended_at=?, updated_at=?
			 WHERE id=? AND state IN ('launching', 'running')`,
			now, now, r.ID,
		)
		if err != nil {
			return swept, spared, fmt.Errorf("sweep stale sessions: fail %s: %w", r.ID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			swept = append(swept, r.ID)
		}
	}
	return swept, spared, nil
}

// SetSessionProcessStart records the OS start time of the process now
// running session id under pid (see StaleSession.PIDStartedAt). It writes
// only while the row still names that pid, so a late call cannot attach one
// process's start time to another.
func (s *Store) SetSessionProcessStart(id string, pid int, startedAt string) error {
	_, err := s.db.Exec(`UPDATE sessions SET pid_started_at=? WHERE id=? AND pid=?`, startedAt, id, pid)
	return err
}

// ListSessionsOptions narrows the ListSessions query. All fields are
// optional — the zero value matches the legacy "return every row,
// newest first" behavior.
//
// Cursor is an opaque created_at+id continuation token (legacy RFC3339
// timestamps remain supported). The query returns rows strictly after it
// in descending created_at, id order. State restricts to a single session
// state ('created', 'launching', 'running', 'completed', 'failed',
// 'killed', 'ready', 'detached', 'orphaned'). GroupID, when non-empty,
// restricts to sessions in the named session group. Limit caps the page size; 0 falls back to the
// default (100) and values above the hard cap (1000) are clamped.
type ListSessionsOptions struct {
	Limit   int
	Cursor  string
	State   string
	GroupID string
	// LaunchID, when set, restricts the list to sessions created from that launch.
	LaunchID string
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
		createdAt, id, err := ParseSessionCursor(opts.Cursor)
		if err != nil {
			return nil, err
		}
		if id == "" {
			add("created_at < ?", createdAt)
		} else {
			add("(created_at, id) < (?, ?)", createdAt)
			args = append(args, id)
		}
	}
	if opts.State != "" {
		add("state = ?", opts.State)
	}
	if opts.GroupID != "" {
		add("session_group_id = ?", opts.GroupID)
	}
	if opts.LaunchID != "" {
		add("launch_id = ?", opts.LaunchID)
	}

	q := `SELECT id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, pid, exit_code, created_at, updated_at, ended_at, session_group_id, parent_session_id, intent, publication, workstream_id, ref_attribution, route_json, worker_id, task_id FROM sessions` + where + ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.LogicalAgentID, &r.ProviderID, &r.ProviderKind, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt, &r.UpdatedAt, &r.EndedAt, &r.SessionGroupID, &r.ParentSessionID, &r.Intent, &r.Publication, &r.WorkstreamID, &r.RefAttribution, &r.RouteJSON, &r.WorkerID, &r.TaskID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetSession(id string) (*SessionRow, error) {
	return s.GetSessionContext(context.Background(), id)
}

func (s *Store) GetSessionContext(ctx context.Context, id string) (*SessionRow, error) {
	var r SessionRow
	err := s.db.QueryRowContext(ctx, `SELECT id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, pid, exit_code, created_at, updated_at, ended_at, session_group_id, parent_session_id, intent, publication, workstream_id, ref_attribution, route_json, worker_id, task_id FROM sessions WHERE id=?`, id).
		Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.LogicalAgentID, &r.ProviderID, &r.ProviderKind, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt, &r.UpdatedAt, &r.EndedAt, &r.SessionGroupID, &r.ParentSessionID, &r.Intent, &r.Publication, &r.WorkstreamID, &r.RefAttribution, &r.RouteJSON, &r.WorkerID, &r.TaskID)
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
