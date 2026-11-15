// Package sessionstore is the class C store for sessions and their satellites (port slice S2b-3a,
// plan 13.1): sessions, launch plans, idempotency keys, session groups, refs, routes, MCP policy,
// provider mappings, checkpoints, client attachments, seat status and workspaces. It reaches its
// tables only through the class C handle and writes SQL that runs on SQLite and Postgres.
//
// The store speaks rows. What the old store did with domain types on the way in (resolving the
// launch plan's route, launch.Plan JSON) is the session slice's (S9): the caller passes the plan
// and route as JSON. Nullable columns that reference another table are stored NULL for an empty
// string, as the old store's NULL-able inserts did (foreign keys are off on both dialects, as in
// the live store; internal/sqlitefk).
package sessionstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// ErrSessionNotFound is returned for an unknown session id.
var ErrSessionNotFound = errors.New("session not found")

// ErrIdempotencyKeyTaken is returned by CreateSession when another create claimed the same
// (principal, key) first; nothing is written. The caller re-reads the key (GetSessionIdempotency)
// and replays or refuses, as for a key found before the create.
var ErrIdempotencyKeyTaken = errors.New("record idempotency key: key already claimed")

// Store is the sessions' class C store.
type Store struct {
	all *store.Stores
	c   *store.Handle
	// Now is the store's clock (time.Now outside tests).
	Now func() time.Time
}

// New returns the store over the class handles.
func New(s *store.Stores) *Store { return &Store{all: s, c: s.C(), Now: time.Now} }

func (s *Store) now() time.Time { return s.Now().UTC() }

// secondStamp is the sessions table's own time format: RFC 3339 to the second, as the old store
// wrote created_at, updated_at and ended_at (migrated rows keep it).
func (s *Store) secondStamp() string { return s.now().Format(time.RFC3339) }

func rebind(q gostore.Querier, query string) string { return q.Dialect().Rebind(query) }

func nullText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// SessionRow is one sessions row. Empty strings for the nullable columns mean NULL.
type SessionRow struct {
	ID, LaunchID, ProjectID, LogicalAgentID, ProviderID, WorkerID, ProviderKind, Workspace, State string
	PID, ExitCode                                                                                 *int64
	CreatedAt, UpdatedAt, EndedAt                                                                 string
	ParentSessionID, Intent, Publication, SessionGroupID, WorkstreamID                            string
	RefAttribution, RouteJSON, TaskID, App                                                        string
}

const sessionColumns = `id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, pid, exit_code,
 created_at, updated_at, ended_at, session_group_id, parent_session_id, intent, publication, workstream_id, ref_attribution,
 route_json, worker_id, task_id, app`

func scanSession(scan func(...any) error) (SessionRow, error) {
	var r SessionRow
	var pid, exit sql.NullInt64
	var ended, group, parent, ws, ref, route, task, app sql.NullString
	if err := scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.LogicalAgentID, &r.ProviderID, &r.ProviderKind, &r.Workspace, &r.State,
		&pid, &exit, &r.CreatedAt, &r.UpdatedAt, &ended, &group, &parent, &r.Intent, &r.Publication, &ws, &ref, &route,
		&r.WorkerID, &task, &app); err != nil {
		return SessionRow{}, err
	}
	if pid.Valid {
		r.PID = &pid.Int64
	}
	if exit.Valid {
		r.ExitCode = &exit.Int64
	}
	r.EndedAt, r.SessionGroupID, r.ParentSessionID, r.WorkstreamID = ended.String, group.String, parent.String, ws.String
	r.RefAttribution, r.RouteJSON, r.TaskID, r.App = ref.String, route.String, task.String, app.String
	return r, nil
}

// Idempotency operations and principals.
const (
	IdempotencyOpCreate = "create"
	IdempotencyOpResume = "resume"

	IdempotencyPrincipalAnonymous = "anonymous"
	IdempotencyPrincipalTeam      = "team-runtime"
	IdempotencyPrincipalLegacy    = "legacy"
)

// ErrIdempotencyConflict is returned when a key was already used with a different request.
var ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")

// SessionIdempotency is a session create/resume key.
type SessionIdempotency struct {
	PrincipalID, Key, Operation, RequestDigest, SessionID string
	ResolvedParentSessionID                               string
}

// CreateSession stores a session, its launch plan JSON ("null" when there is none) and, when key is
// set, its idempotency key, in one class C transaction. created_at/updated_at are now; an empty
// intent is "fresh" and an empty publication "private-local". A session with a parent inherits the
// parent's workstream (when it names none) and app (always), read in the same transaction.
func (s *Store) CreateSession(ctx context.Context, row SessionRow, planJSON string, key *SessionIdempotency) (SessionRow, error) {
	if key != nil && (key.PrincipalID == "" || key.PrincipalID == IdempotencyPrincipalLegacy) {
		return SessionRow{}, fmt.Errorf("record idempotency key: principal %q cannot claim a key", key.PrincipalID)
	}
	if planJSON == "" {
		planJSON = "null"
	}
	row.CreatedAt, row.UpdatedAt = s.secondStamp(), s.secondStamp()
	if row.Intent == "" {
		row.Intent = "fresh"
	}
	if row.Publication == "" {
		row.Publication = "private-local"
	}
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if row.ParentSessionID != "" {
			var ws, app sql.NullString
			err := tx.QueryRowContext(ctx, rebind(tx, `SELECT workstream_id, app FROM sessions WHERE id = ?`), row.ParentSessionID).Scan(&ws, &app)
			switch {
			case err == nil:
				if row.WorkstreamID == "" {
					row.WorkstreamID = ws.String
				}
				row.App = app.String
			case errors.Is(err, sql.ErrNoRows):
				// The old store kept the row's own workstream and cleared app for a missing parent.
				row.App = ""
			default:
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO sessions
 (id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state, created_at, updated_at,
  parent_session_id, intent, publication, workstream_id, ref_attribution, route_json, worker_id, task_id, app)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			row.ID, row.LaunchID, row.ProjectID, row.LogicalAgentID, row.ProviderID, row.ProviderKind, row.Workspace, row.State,
			row.CreatedAt, row.UpdatedAt, nullText(row.ParentSessionID), row.Intent, row.Publication, nullText(row.WorkstreamID),
			nullText(row.RefAttribution), nullText(row.RouteJSON), row.WorkerID, nullText(row.TaskID), nullText(row.App)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO launch_plans (session_id, plan_json) VALUES (?, ?)`), row.ID, planJSON); err != nil {
			return err
		}
		if key != nil {
			if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_idempotency
 (principal_id, key, operation, request_digest, session_id, resolved_parent_session_id) VALUES (?, ?, ?, ?, ?, ?)`),
				key.PrincipalID, key.Key, key.Operation, key.RequestDigest, row.ID, nullText(key.ResolvedParentSessionID)); err != nil {
				if gostore.IsUniqueViolation(err) {
					return ErrIdempotencyKeyTaken
				}
				return fmt.Errorf("record idempotency key: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return SessionRow{}, err
	}
	return row, nil
}

// UpdateSessionState sets state and pid, and on an exit code also exit_code and ended_at (first
// one wins).
func (s *Store) UpdateSessionState(ctx context.Context, id, state string, pid int, exit *int) error {
	now := s.secondStamp()
	var ended, code any
	if exit != nil {
		ended, code = now, int64(*exit)
	}
	return s.exec(ctx, `UPDATE sessions SET state = ?, pid = ?, exit_code = ?, updated_at = ?, ended_at = COALESCE(?, ended_at) WHERE id = ?`,
		state, pid, code, now, ended, id)
}

// SetSessionProcessStart records the process start time of the session's current pid.
func (s *Store) SetSessionProcessStart(ctx context.Context, id string, pid int, startedAt string) error {
	return s.exec(ctx, `UPDATE sessions SET pid_started_at = ? WHERE id = ? AND pid = ?`, startedAt, id, pid)
}

func (s *Store) exec(ctx context.Context, q string, args ...any) error {
	_, err := s.execN(ctx, q, args...)
	return err
}

func (s *Store) execN(ctx context.Context, q string, args ...any) (int64, error) {
	var n int64
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, q), args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// ListSessionsOptions filters ListSessions.
type ListSessionsOptions struct {
	Limit                    int
	Cursor                   string // a created_at, or SessionCursor(created_at, id)
	State, GroupID, LaunchID string
}

// ListSessions returns sessions newest first (limit 1..1000, default 100), after the cursor.
func (s *Store) ListSessions(ctx context.Context, opts ListSessionsOptions) ([]SessionRow, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	var where []string
	var args []any
	if opts.Cursor != "" {
		createdAt, id, err := ParseSessionCursor(opts.Cursor)
		if err != nil {
			return nil, err
		}
		if id == "" {
			where, args = append(where, "created_at < ?"), append(args, createdAt)
		} else {
			where, args = append(where, "(created_at < ? OR (created_at = ? AND id < ?))"), append(args, createdAt, createdAt, id)
		}
	}
	for _, f := range []struct{ col, v string }{{"state", opts.State}, {"session_group_id", opts.GroupID}, {"launch_id", opts.LaunchID}} {
		if f.v != "" {
			where, args = append(where, f.col+" = ?"), append(args, f.v)
		}
	}
	q := `SELECT ` + sessionColumns + ` FROM sessions`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, q+` ORDER BY created_at DESC, id DESC LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SessionRow
	for rows.Next() {
		row, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// GetSession returns one session (ErrSessionNotFound when absent).
func (s *Store) GetSession(ctx context.Context, id string) (SessionRow, error) {
	r := s.c.Reader()
	row, err := scanSession(r.QueryRowContext(ctx, rebind(r, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`), id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return row, err
}

// LaunchPlanJSON returns the session's stored launch plan (sql.ErrNoRows when absent, as before).
func (s *Store) LaunchPlanJSON(ctx context.Context, sessionID string) (string, error) {
	r := s.c.Reader()
	var plan string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT plan_json FROM launch_plans WHERE session_id = ?`), sessionID).Scan(&plan)
	return plan, err
}

// GetSessionIdempotency returns a principal's key (nil when unknown, and always for the legacy
// principal).
func (s *Store) GetSessionIdempotency(ctx context.Context, principal, key string) (*SessionIdempotency, error) {
	if principal == IdempotencyPrincipalLegacy {
		return nil, nil
	}
	r := s.c.Reader()
	var rec SessionIdempotency
	var parent sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT principal_id, key, operation, request_digest, session_id, resolved_parent_session_id
 FROM session_idempotency WHERE principal_id = ? AND key = ?`), principal, key).
		Scan(&rec.PrincipalID, &rec.Key, &rec.Operation, &rec.RequestDigest, &rec.SessionID, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.ResolvedParentSessionID = parent.String
	return &rec, nil
}

// ReleaseSessionIdempotency forgets a key.
func (s *Store) ReleaseSessionIdempotency(ctx context.Context, principal, key string) error {
	if err := s.exec(ctx, `DELETE FROM session_idempotency WHERE principal_id = ? AND key = ?`, principal, key); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}

// SessionHasIdempotencyKey reports whether any key names the session.
func (s *Store) SessionHasIdempotencyKey(ctx context.Context, sessionID string) (bool, error) {
	r := s.c.Reader()
	var n int
	err := r.QueryRowContext(ctx, rebind(r, `SELECT COUNT(1) FROM session_idempotency WHERE session_id = ?`), sessionID).Scan(&n)
	return n > 0, err
}

// SessionCursor encodes a (created_at, id) paging position.
func SessionCursor(createdAt, id string) string {
	b, _ := json.Marshal([2]string{createdAt, id})
	return "v1." + base64.RawURLEncoding.EncodeToString(b)
}

// ParseSessionCursor decodes a cursor: a bare RFC 3339 created_at (older clients) or a v1 pair.
func ParseSessionCursor(cursor string) (createdAt, id string, err error) {
	if _, err := time.Parse(time.RFC3339Nano, cursor); err == nil {
		return cursor, "", nil
	}
	invalid := errors.New("invalid session cursor")
	if len(cursor) < 4 || cursor[:3] != "v1." {
		return "", "", invalid
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor[3:])
	if err != nil {
		return "", "", invalid
	}
	var parts []string
	if json.Unmarshal(b, &parts) != nil || len(parts) != 2 || parts[1] == "" {
		return "", "", invalid
	}
	if _, err := time.Parse(time.RFC3339Nano, parts[0]); err != nil {
		return "", "", invalid
	}
	return parts[0], parts[1], nil
}
