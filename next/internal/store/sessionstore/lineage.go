package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	gostore "github.com/hollis-labs/libs/core/store"
)

// ErrSessionGroupNotFound is returned for an unknown session group id (sql.ErrNoRows in the old
// store).
var ErrSessionGroupNotFound = errors.New("session group not found")

// SessionGroupRow is one session_groups row. Empty Name and WorkflowID mean NULL.
type SessionGroupRow struct {
	ID, Name, WorkflowID, CreatedAt, UpdatedAt string
}

const refsGroupColumns = `id, name, workflow_id, created_at, updated_at`

func refsScanGroup(scan func(...any) error) (SessionGroupRow, error) {
	var g SessionGroupRow
	var name, workflow sql.NullString
	if err := scan(&g.ID, &name, &workflow, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return SessionGroupRow{}, err
	}
	g.Name, g.WorkflowID = name.String, workflow.String
	return g, nil
}

// CreateSessionGroup inserts a session group; the caller supplies the timestamps, as before.
func (s *Store) CreateSessionGroup(ctx context.Context, g SessionGroupRow) error {
	if g.ID == "" {
		return errors.New("session group id required")
	}
	if err := s.exec(ctx, `INSERT INTO session_groups (id, name, workflow_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		g.ID, nullText(g.Name), nullText(g.WorkflowID), g.CreatedAt, g.UpdatedAt); err != nil {
		return fmt.Errorf("create session_group %q: %w", g.ID, err)
	}
	return nil
}

// GetSessionGroup returns one group (ErrSessionGroupNotFound when unknown).
func (s *Store) GetSessionGroup(ctx context.Context, id string) (SessionGroupRow, error) {
	r := s.c.Reader()
	g, err := refsScanGroup(r.QueryRowContext(ctx, rebind(r, `SELECT `+refsGroupColumns+` FROM session_groups WHERE id = ?`), id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionGroupRow{}, ErrSessionGroupNotFound
	}
	return g, err
}

// ListSessionGroups returns every group, newest first (ties by id, descending; the old order had
// no tiebreak).
func (s *Store) ListSessionGroups(ctx context.Context) ([]SessionGroupRow, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, `SELECT `+refsGroupColumns+` FROM session_groups ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list session_groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionGroupRow
	for rows.Next() {
		g, err := refsScanGroup(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan session_group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AddSessionToGroup sets the session's session_group_id (ErrSessionNotFound when the session is
// unknown; the group is not checked, as before).
func (s *Store) AddSessionToGroup(ctx context.Context, sessionID, groupID string) error {
	n, err := s.execN(ctx, `UPDATE sessions SET session_group_id = ? WHERE id = ?`, groupID, sessionID)
	if err != nil {
		return fmt.Errorf("add session %q to group %q: %w", sessionID, groupID, err)
	}
	if n == 0 {
		return fmt.Errorf("add session to group: %w: %q", ErrSessionNotFound, sessionID)
	}
	return nil
}

// ListSessionsByGroup returns the group's sessions, newest first.
func (s *Store) ListSessionsByGroup(ctx context.Context, groupID string) ([]SessionRow, error) {
	return s.ListSessions(ctx, ListSessionsOptions{GroupID: groupID})
}

// ChildSessionIDs returns the ids of the sessions whose parent is id, oldest first.
func (s *Store) ChildSessionIDs(ctx context.Context, id string) ([]string, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT id FROM sessions WHERE parent_session_id = ? ORDER BY created_at, id`), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// refsMaxLineageWalk bounds SessionLineageDepth so a corrupt cycle cannot loop forever.
const refsMaxLineageWalk = 1024

// SessionLineageDepth counts id's ancestors by walking parent_session_id (a root has depth 0). The
// walk reads one snapshot (ReadTx), where the old store read each step separately.
func (s *Store) SessionLineageDepth(ctx context.Context, id string) (int, error) {
	depth := 0
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		depth = 0
		for cur := id; depth < refsMaxLineageWalk; depth++ {
			var parent sql.NullString
			if err := q.QueryRowContext(ctx, rebind(q, `SELECT parent_session_id FROM sessions WHERE id = ?`), cur).Scan(&parent); err != nil {
				return fmt.Errorf("lineage depth %s: %w", cur, err)
			}
			if parent.String == "" {
				return nil
			}
			cur = parent.String
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return depth, nil
}

// LiveChildCount counts id's direct children that have not ended (created, ready, launching,
// running, detached or orphaned); ended children do not count against a parent's child cap.
func (s *Store) LiveChildCount(ctx context.Context, id string) (int, error) {
	return s.refsCount(ctx, `SELECT COUNT(*) FROM sessions WHERE parent_session_id = ?
 AND state IN ('created','ready','launching','running','detached','orphaned')`, id)
}

// ChildCountSince counts id's direct children (any state) created at or after cutoff, a UTC RFC
// 3339 timestamp compared as text.
func (s *Store) ChildCountSince(ctx context.Context, id, cutoff string) (int, error) {
	return s.refsCount(ctx, `SELECT COUNT(*) FROM sessions WHERE parent_session_id = ? AND created_at >= ?`, id, cutoff)
}

func (s *Store) refsCount(ctx context.Context, q string, args ...any) (int, error) {
	r := s.c.Reader()
	var n int
	err := r.QueryRowContext(ctx, rebind(r, q), args...).Scan(&n)
	return n, err
}

// LatestSessionForLogicalAgent returns the newest session of a logical agent in any state ("" when
// it has none). created_at has one-second resolution; the old store broke ties on rowid (insertion
// order), which Postgres does not have, so ties break on id, descending. Session ids are random,
// so two sessions of one agent created in the same second may now resolve differently.
func (s *Store) LatestSessionForLogicalAgent(ctx context.Context, logicalAgentID string) (string, error) {
	r := s.c.Reader()
	var id string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT id FROM sessions WHERE logical_agent_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`),
		logicalAgentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// SessionRouteJSON returns the session's stored route_json ("" for no route, including sessions
// that predate routing; ErrSessionNotFound for an unknown session). Decoding and validating the
// route (launchprofile.ValidateResolvedRoute in the old tree) is the session slice's.
func (s *Store) SessionRouteJSON(ctx context.Context, id string) (string, error) {
	return SessionRouteJSONTx(ctx, s.c.Reader(), id)
}

// SessionRouteJSONTx reads the route through the caller's querier: a class C transaction or a
// joint's (old SessionRouteTx).
func SessionRouteJSONTx(ctx context.Context, q gostore.Querier, id string) (string, error) {
	if q == nil {
		return "", errors.New("session route: querier required")
	}
	var raw sql.NullString
	err := q.QueryRowContext(ctx, rebind(q, `SELECT route_json FROM sessions WHERE id = ?`), id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return raw.String, err
}
