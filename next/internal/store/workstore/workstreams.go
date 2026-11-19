package workstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
)

var (
	// ErrWorkstreamNotFound is returned for an unknown workstream id.
	ErrWorkstreamNotFound = errors.New("workstream not found")
	// ErrSessionNotFound is returned for an unknown session id (sessionstore has the same error; this
	// slice does not import it).
	ErrSessionNotFound = errors.New("session not found")
	// ErrInvalidWorkstreamStatus is returned for a status other than active or closed.
	ErrInvalidWorkstreamStatus = errors.New("invalid workstream status: want active or closed")
	// ErrWorkstreamProject is returned for a project association the registry does not allow.
	ErrWorkstreamProject = errors.New("invalid workstream project association")
	// ErrWorkstreamPage is returned for invalid pagination options or a cursor that does not match them.
	ErrWorkstreamPage = errors.New("invalid workstream pagination")
	// ErrNotAWorkstream is returned by WorkstreamTarget for an id that is not a workstream's.
	ErrNotAWorkstream = errors.New("id does not name a workstream")
)

// WorkstreamRow is one workstreams row with its project URNs (sorted). Empty Name and WorkflowID
// are NULL.
type WorkstreamRow struct {
	ProjectURNs                                        []string
	ID, Name, WorkflowID, Status, CreatedAt, UpdatedAt string
}

const workstreamColumns = `w.id, w.name, w.workflow_id, w.status, w.created_at, w.updated_at`

func scanWorkstream(scan func(...any) error) (WorkstreamRow, error) {
	var w WorkstreamRow
	var name, workflowID sql.NullString
	if err := scan(&w.ID, &name, &workflowID, &w.Status, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return WorkstreamRow{}, err
	}
	w.Name, w.WorkflowID = name.String, workflowID.String
	return w, nil
}

// workstreams runs a workstream query and loads each row's projects, on one querier.
func workstreams(ctx context.Context, q gostore.Querier, query string, args ...any) ([]WorkstreamRow, error) {
	rows, err := q.QueryContext(ctx, rebind(q, query), args...)
	if err != nil {
		return nil, err
	}
	var out []WorkstreamRow
	for rows.Next() {
		w, err := scanWorkstream(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return loadWorkstreamProjects(ctx, q, out)
}

// loadWorkstreamProjects fills ProjectURNs ([] when none). The old store passed the ids through
// json_each; an IN list is the same on both dialects (pages are bounded by MaxWorkstreamLimit+1).
func loadWorkstreamProjects(ctx context.Context, q gostore.Querier, items []WorkstreamRow) ([]WorkstreamRow, error) {
	if len(items) == 0 {
		return items, nil
	}
	args := make([]any, len(items))
	for i := range items {
		items[i].ProjectURNs = []string{}
		args[i] = items[i].ID
	}
	rows, err := q.QueryContext(ctx, rebind(q, `SELECT workstream_id, project_urn FROM workstream_projects WHERE workstream_id IN (`+placeholders(len(args))+`)
 ORDER BY workstream_id, project_urn`), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byID := map[string][]string{}
	for rows.Next() {
		var id, urn string
		if err := rows.Scan(&id, &urn); err != nil {
			return nil, err
		}
		byID[id] = append(byID[id], urn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range items {
		if projects, ok := byID[items[i].ID]; ok {
			items[i].ProjectURNs = projects
		}
	}
	return items, nil
}

// ValidProjectURN reports whether urn is a canonical project agent URN (prj_...).
func ValidProjectURN(urn string) bool {
	a, err := messaging.ParseURN(urn)
	return err == nil && a.Kind == messaging.KindAgent && strings.HasPrefix(a.ID, "prj_") && len(a.ID) > 4 && a.URN() == urn
}

// replaceWorkstreamProjects sets the workstream's projects to requested (sorted, deduplicated), each
// a registered project; a deprecated one only if it is already linked. It runs in the caller's
// transaction after the workstream row is written (and so locked), which serializes two
// replacements of one workstream.
func replaceWorkstreamProjects(ctx context.Context, tx store.Tx, id string, requested []string) ([]string, error) {
	if requested != nil {
		var probe int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM registry_entries LIMIT 1`).Scan(&probe); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("registry project lookup unavailable: %w", err)
		}
	}
	projects := append([]string{}, requested...)
	slices.Sort(projects)
	projects = slices.Compact(projects)
	for _, urn := range projects {
		if !ValidProjectURN(urn) {
			return nil, ErrWorkstreamProject
		}
		var kind, status string
		err := tx.QueryRowContext(ctx, rebind(tx, `SELECT kind, status FROM registry_entries WHERE urn = ?`), urn).Scan(&kind, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWorkstreamProject
		}
		if err != nil {
			return nil, fmt.Errorf("registry project lookup unavailable: %w", err)
		}
		if kind != "project" {
			return nil, ErrWorkstreamProject
		}
		if status == "deprecated" {
			var linked int64
			if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT COUNT(*) FROM workstream_projects WHERE workstream_id = ? AND project_urn = ?`), id, urn).Scan(&linked); err != nil {
				return nil, err
			}
			if linked == 0 {
				return nil, ErrWorkstreamProject
			}
		}
	}
	if _, err := tx.ExecContext(ctx, rebind(tx, `DELETE FROM workstream_projects WHERE workstream_id = ?`), id); err != nil {
		return nil, err
	}
	for _, urn := range projects {
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO workstream_projects (workstream_id, project_urn) VALUES (?, ?)`), id, urn); err != nil {
			return nil, err
		}
	}
	return projects, nil
}

// createWorkstreamTx inserts w (a UUIDv7 id when none, status active by default) and its projects.
func (s *Store) createWorkstreamTx(ctx context.Context, tx store.Tx, w WorkstreamRow) (WorkstreamRow, error) {
	if w.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return WorkstreamRow{}, fmt.Errorf("create workstream: uuid v7: %w", err)
		}
		w.ID = id.String()
	}
	if w.Status == "" {
		w.Status = "active"
	}
	w.CreatedAt = s.secondStamp()
	w.UpdatedAt = w.CreatedAt
	if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO workstreams (id, name, workflow_id, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`),
		w.ID, nullText(w.Name), nullText(w.WorkflowID), w.Status, w.CreatedAt, w.UpdatedAt); err != nil {
		return WorkstreamRow{}, fmt.Errorf("create workstream %q: %w", w.ID, err)
	}
	projects, err := replaceWorkstreamProjects(ctx, tx, w.ID, w.ProjectURNs)
	if err != nil {
		return WorkstreamRow{}, err
	}
	w.ProjectURNs = projects
	return w, nil
}

// CreateWorkstream inserts a workstream and its projects in one transaction.
func (s *Store) CreateWorkstream(ctx context.Context, w WorkstreamRow) (WorkstreamRow, error) {
	var out WorkstreamRow
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = s.createWorkstreamTx(ctx, tx, w)
		return err
	})
	return out, err
}

func getWorkstream(ctx context.Context, q gostore.Querier, id string) (WorkstreamRow, error) {
	items, err := workstreams(ctx, q, `SELECT `+workstreamColumns+` FROM workstreams w WHERE w.id = ?`, id)
	if err != nil {
		return WorkstreamRow{}, fmt.Errorf("get workstream %q: %w", id, err)
	}
	if len(items) == 0 {
		return WorkstreamRow{}, ErrWorkstreamNotFound
	}
	return items[0], nil
}

// GetWorkstream returns one workstream with its projects, read in one snapshot.
func (s *Store) GetWorkstream(ctx context.Context, id string) (WorkstreamRow, error) {
	var w WorkstreamRow
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		w, err = getWorkstream(ctx, q, id)
		return err
	})
	return w, err
}

// WorkstreamPatch is a partial update: a nil field is left alone, a non-nil one is written (a pointer
// to "" clears name or workflow_id). Status must be active or closed.
type WorkstreamPatch struct {
	ProjectURNs *[]string
	Name        *string
	WorkflowID  *string
	Status      *string
}

// UpdateWorkstream applies patch and returns the refreshed row; updated_at is set to now.
func (s *Store) UpdateWorkstream(ctx context.Context, id string, patch WorkstreamPatch) (WorkstreamRow, error) {
	if patch.Status != nil && *patch.Status != "active" && *patch.Status != "closed" {
		return WorkstreamRow{}, ErrInvalidWorkstreamStatus
	}
	var setName, setWorkflow, setStatus int
	var nameVal, workflowVal, statusVal any
	if patch.Name != nil {
		setName, nameVal = 1, nullText(*patch.Name)
	}
	if patch.WorkflowID != nil {
		setWorkflow, workflowVal = 1, nullText(*patch.WorkflowID)
	}
	if patch.Status != nil {
		setStatus, statusVal = 1, *patch.Status
	}
	var out WorkstreamRow
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// The UPDATE locks the row first, so the project replacement below is serialized per workstream.
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE workstreams SET
 name = CASE WHEN ? = 1 THEN CAST(? AS TEXT) ELSE name END,
 workflow_id = CASE WHEN ? = 1 THEN CAST(? AS TEXT) ELSE workflow_id END,
 status = CASE WHEN ? = 1 THEN CAST(? AS TEXT) ELSE status END,
 updated_at = ? WHERE id = ?`), setName, nameVal, setWorkflow, workflowVal, setStatus, statusVal, s.secondStamp(), id)
		if err != nil {
			return fmt.Errorf("update workstream %q: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("update workstream %q: %w", id, err)
		} else if n == 0 {
			return ErrWorkstreamNotFound
		}
		if patch.ProjectURNs != nil {
			if _, err := replaceWorkstreamProjects(ctx, tx, id, *patch.ProjectURNs); err != nil {
				return err
			}
		}
		out, err = getWorkstream(ctx, tx, id)
		return err
	})
	if err != nil {
		return WorkstreamRow{}, err
	}
	return out, nil
}

// AssignSessionWorkstream sets the session's workstream (empty clears it). The workstream must exist
// (foreign keys are off, so a dangling id would read as unassigned). One guarded UPDATE: it writes
// only when the workstream exists at that moment, where the old store checked first and wrote after.
// An unknown workstream reports ErrWorkstreamNotFound before an unknown session, as before.
func (s *Store) AssignSessionWorkstream(ctx context.Context, sessionID, workstreamID string) error {
	if sessionID == "" {
		return errors.New("assign workstream: session id required")
	}
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE sessions SET workstream_id = ?, updated_at = ? WHERE id = ?
 AND (CAST(? AS TEXT) = '' OR EXISTS (SELECT 1 FROM workstreams WHERE id = ?))`),
			nullText(workstreamID), s.secondStamp(), sessionID, workstreamID, workstreamID)
		if err != nil {
			return fmt.Errorf("assign workstream to session %q: %w", sessionID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("assign workstream to session %q: %w", sessionID, err)
		} else if n > 0 {
			return nil
		}
		if workstreamID != "" {
			var exists int64
			if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT COUNT(*) FROM workstreams WHERE id = ?`), workstreamID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrWorkstreamNotFound
			}
		}
		return ErrSessionNotFound
	})
}

// lineageRow is the part of a session the lineage walk reads.
type lineageRow struct {
	ID, ParentSessionID, WorkstreamID string
}

// lockLineage is the Postgres row lock on each lineage row the ensure walk reads. Tests switch it
// off to show the race it closes.
var lockLineage = true

// sessionLineageTx returns sessionID and its ancestors, nearest first. On Postgres each row is locked
// (FOR UPDATE) as it is read: walks from different leaves of one tree lock their shared ancestors in
// the same leaf-to-root order, so they serialize without deadlocking, and a walk that waited reads
// the row as the other committed it. A missing ancestor ends the walk; a cycle is an error (foreign
// keys are off and nothing rejects one on write).
func sessionLineageTx(ctx context.Context, tx store.Tx, sessionID string) ([]lineageRow, error) {
	lock := ""
	if lockLineage && isPostgres(tx) {
		lock = " FOR UPDATE"
	}
	var chain []lineageRow
	visited := map[string]bool{}
	for id := sessionID; id != ""; {
		if visited[id] {
			return nil, fmt.Errorf("session lineage for %q: cycle at %q", sessionID, id)
		}
		visited[id] = true
		var row lineageRow
		var parent, ws sql.NullString
		err := tx.QueryRowContext(ctx, rebind(tx, `SELECT id, parent_session_id, workstream_id FROM sessions WHERE id = ?`+lock), id).Scan(&row.ID, &parent, &ws)
		if errors.Is(err, sql.ErrNoRows) {
			if id != sessionID {
				break
			}
			return nil, ErrSessionNotFound
		}
		if err != nil {
			return nil, err
		}
		row.ParentSessionID, row.WorkstreamID = parent.String, ws.String
		chain = append(chain, row)
		id = row.ParentSessionID
	}
	return chain, nil
}

// EnsureSessionWorkstream returns the workstream containing sessionID, creating one if its lineage
// has none: the session's own, else the nearest ancestor's, else a new one from seed. Every session in
// the lineage without a workstream gets the result; one already in a different workstream is never
// moved. The old store ran the walk, the create and each stamp as separate writes, so two concurrent
// ensures over one lineage could mint two workstreams; here it is one transaction, with the lineage
// rows locked on Postgres.
func (s *Store) EnsureSessionWorkstream(ctx context.Context, sessionID string, seed WorkstreamRow) (WorkstreamRow, error) {
	var out WorkstreamRow
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		chain, err := sessionLineageTx(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		found := false
		for _, row := range chain {
			if row.WorkstreamID == "" {
				continue
			}
			if out, err = getWorkstream(ctx, tx, row.WorkstreamID); err != nil {
				// A dangling pointer is corruption, not an empty lineage.
				return fmt.Errorf("session %q references workstream %q: %w", row.ID, row.WorkstreamID, err)
			}
			found = true
			break
		}
		if !found {
			if out, err = s.createWorkstreamTx(ctx, tx, seed); err != nil {
				return err
			}
		}
		now := s.secondStamp()
		for _, row := range chain {
			if row.WorkstreamID != "" {
				continue // already placed, here or elsewhere: never moved
			}
			if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE sessions SET workstream_id = ?, updated_at = ? WHERE id = ?
 AND (workstream_id IS NULL OR workstream_id = '')`), out.ID, now, row.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return WorkstreamRow{}, err
	}
	return out, nil
}

// WorkstreamTargetOptions selects the namespace a workstream workspace lives under.
type WorkstreamTargetOptions struct {
	Project, Owner, Tail string
}

// WorkstreamNamespace is a workstream's workspace namespace.
type WorkstreamNamespace struct {
	Namespace    string `json:"namespace"`
	WorkstreamID string `json:"workstream_id"`
}

// SessionWorkstreamNamespace is the workspace namespace of the session's workstream; the project
// defaults to the session's own.
func (s *Store) SessionWorkstreamNamespace(ctx context.Context, sessionID string, opts ...WorkstreamTargetOptions) (WorkstreamNamespace, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return WorkstreamNamespace{}, errors.New("workstream namespace: session id required")
	}
	r := s.c.Reader()
	var projectID string
	var ws sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT project_id, workstream_id FROM sessions WHERE id = ?`), sessionID).Scan(&projectID, &ws)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrSessionNotFound
	}
	if err != nil {
		return WorkstreamNamespace{}, fmt.Errorf("workstream namespace for session %q: %w", sessionID, err)
	}
	if ws.String == "" {
		return WorkstreamNamespace{}, fmt.Errorf("session %q belongs to no workstream: call EnsureSessionWorkstream first (%w)", sessionID, ErrWorkstreamNotFound)
	}
	var opt WorkstreamTargetOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if strings.TrimSpace(opt.Project) == "" && strings.TrimSpace(opt.Owner) == "" {
		if strings.TrimSpace(projectID) == "" {
			return WorkstreamNamespace{}, errors.New("workstream namespace: session has no declared project: specify explicit project or owner")
		}
		opt.Project = strings.TrimSpace(projectID)
	}
	return s.WorkstreamTarget(ctx, ws.String, opt)
}

// WorkstreamTarget is the workspace namespace "<owner>|project/<project>/workspace/<tail>" of an
// existing workstream (tail default "scratch"). A session id is refused with ErrNotAWorkstream.
func (s *Store) WorkstreamTarget(ctx context.Context, workstreamID string, opts ...WorkstreamTargetOptions) (WorkstreamNamespace, error) {
	workstreamID = strings.TrimSpace(workstreamID)
	if workstreamID == "" {
		return WorkstreamNamespace{}, errors.New("workstream target: workstream id required")
	}
	r := s.c.Reader()
	var isWorkstream, isSession int64
	if err := r.QueryRowContext(ctx, rebind(r, `SELECT (SELECT COUNT(*) FROM workstreams WHERE id = ?), (SELECT COUNT(*) FROM sessions WHERE id = ?)`),
		workstreamID, workstreamID).Scan(&isWorkstream, &isSession); err != nil {
		return WorkstreamNamespace{}, err
	}
	if isWorkstream == 0 {
		if isSession > 0 {
			return WorkstreamNamespace{}, fmt.Errorf("%q is a session id, not a workstream id: containment keys on the workstream so it survives a compaction, which creates a new session row (%w)", workstreamID, ErrNotAWorkstream)
		}
		return WorkstreamNamespace{}, fmt.Errorf("workstream %q: %w", workstreamID, ErrNotAWorkstream)
	}
	var opt WorkstreamTargetOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	tail := strings.TrimSpace(opt.Tail)
	if tail == "" {
		tail = "scratch"
	}
	if tail = strings.Trim(tail, "/"); tail == "" {
		return WorkstreamNamespace{}, errors.New("workstream target: workspace requires a non-empty tail segment")
	}
	var head string
	switch {
	case strings.TrimSpace(opt.Owner) != "":
		head = strings.Trim(strings.TrimSpace(opt.Owner), "/")
	case strings.TrimSpace(opt.Project) != "":
		head = "project/" + strings.Trim(strings.TrimPrefix(strings.TrimSpace(opt.Project), "project/"), "/")
	default:
		return WorkstreamNamespace{}, errors.New("workstream target: declared project or owner is required")
	}
	return WorkstreamNamespace{Namespace: head + "/workspace/" + tail, WorkstreamID: workstreamID}, nil
}

// Workstream pages.
const (
	DefaultWorkstreamLimit = 100
	MaxWorkstreamLimit     = 1000
)

// ListWorkstreamsOptions selects a page in descending (created_at, id) order. Ref ("<kind>:<ref_id>")
// selects workstreams holding a session that touched the object, and excludes the other filters.
type ListWorkstreamsOptions struct {
	ProjectURN, Status, WorkflowID, Ref string
	Limit                               int
	Cursor                              string
}

// WorkstreamPage is one page and the cursor of the next.
type WorkstreamPage struct {
	Workstreams []WorkstreamRow
	NextCursor  string
	HasMore     bool
	Limit       int
}

type workstreamCursor struct {
	ProjectURN string `json:"project_urn,omitempty"`
	Version    int    `json:"v"`
	CreatedAt  string `json:"at"`
	ID         string `json:"id"`
	Status     string `json:"status"`
	WorkflowID string `json:"workflow_id"`
	Ref        string `json:"ref"`
}

// ParseRefSelector splits "<kind>:<ref_id>".
func ParseRefSelector(selector string) (kind, refID string, err error) {
	kind, refID, found := strings.Cut(selector, ":")
	if !found {
		return "", "", fmt.Errorf("ref selector %q: want <kind>:<ref_id>, for example torque_task:CW-20260912-0063", selector)
	}
	if kind == "" || refID == "" {
		return "", "", fmt.Errorf("ref selector %q: both kind and ref_id are required", selector)
	}
	return kind, refID, nil
}

// ValidateWorkstreamPage checks and defaults the options (limit default 100, capped at 1000).
func ValidateWorkstreamPage(opts ListWorkstreamsOptions) (ListWorkstreamsOptions, error) {
	if opts.ProjectURN != "" && !ValidProjectURN(opts.ProjectURN) {
		return opts, ErrWorkstreamPage
	}
	if opts.Limit < 0 || (opts.Status != "" && opts.Status != "active" && opts.Status != "closed") {
		return opts, ErrWorkstreamPage
	}
	if opts.Limit == 0 {
		opts.Limit = DefaultWorkstreamLimit
	}
	if opts.Limit > MaxWorkstreamLimit {
		opts.Limit = MaxWorkstreamLimit
	}
	if opts.Ref != "" {
		if opts.Status != "" || opts.WorkflowID != "" || opts.ProjectURN != "" {
			return opts, ErrWorkstreamPage
		}
		if _, _, err := ParseRefSelector(opts.Ref); err != nil {
			return opts, ErrWorkstreamPage
		}
	}
	if _, err := decodeWorkstreamCursor(opts); err != nil {
		return opts, err
	}
	return opts, nil
}

func decodeWorkstreamCursor(opts ListWorkstreamsOptions) (workstreamCursor, error) {
	var c workstreamCursor
	if opts.Cursor == "" {
		return c, nil
	}
	if len(opts.Cursor) > 4096 {
		return c, ErrWorkstreamPage
	}
	b, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
	if err != nil || json.Unmarshal(b, &c) != nil || c.Version != 1 || c.ID == "" || c.Status != opts.Status || c.WorkflowID != opts.WorkflowID || c.Ref != opts.Ref || c.ProjectURN != opts.ProjectURN {
		return c, ErrWorkstreamPage
	}
	if _, err := time.Parse(time.RFC3339Nano, c.CreatedAt); err != nil {
		return c, ErrWorkstreamPage
	}
	return c, nil
}

// ListWorkstreamsPage returns one page, read in one snapshot. The cursor is the old one (version 1),
// so a cursor issued by the old binary still pages; both dialects compare created_at and id as bytes.
func (s *Store) ListWorkstreamsPage(ctx context.Context, opts ListWorkstreamsOptions) (WorkstreamPage, error) {
	opts, err := ValidateWorkstreamPage(opts)
	if err != nil {
		return WorkstreamPage{}, err
	}
	c, err := decodeWorkstreamCursor(opts)
	if err != nil {
		return WorkstreamPage{}, err
	}
	kind, refID := "", ""
	if opts.Ref != "" {
		if kind, refID, err = ParseRefSelector(opts.Ref); err != nil {
			return WorkstreamPage{}, ErrWorkstreamPage
		}
	}
	var items []WorkstreamRow
	err = s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		items, err = workstreams(ctx, q, `SELECT `+workstreamColumns+` FROM workstreams w
 WHERE (CAST(? AS TEXT) = '' OR w.status = ?) AND (CAST(? AS TEXT) = '' OR w.workflow_id = ?)
 AND (CAST(? AS TEXT) = '' OR EXISTS (SELECT 1 FROM session_refs r JOIN sessions s ON s.id = r.session_id
  WHERE s.workstream_id = w.id AND r.kind = ? AND r.ref_id = ?))
 AND (CAST(? AS TEXT) = '' OR EXISTS (SELECT 1 FROM workstream_projects wp WHERE wp.workstream_id = w.id AND wp.project_urn = ?))
 AND (CAST(? AS TEXT) = '' OR w.created_at < ? OR (w.created_at = ? AND w.id < ?))
 ORDER BY w.created_at DESC, w.id DESC LIMIT ?`,
			opts.Status, opts.Status, opts.WorkflowID, opts.WorkflowID, opts.Ref, kind, refID, opts.ProjectURN, opts.ProjectURN,
			opts.Cursor, c.CreatedAt, c.CreatedAt, c.ID, opts.Limit+1)
		return err
	})
	if err != nil {
		return WorkstreamPage{}, err
	}
	page := WorkstreamPage{Workstreams: items, Limit: opts.Limit}
	if len(items) > opts.Limit {
		page.HasMore = true
		page.Workstreams = items[:opts.Limit]
		last := page.Workstreams[len(page.Workstreams)-1]
		b, err := json.Marshal(workstreamCursor{Version: 1, CreatedAt: last.CreatedAt, ID: last.ID, Status: opts.Status, WorkflowID: opts.WorkflowID, Ref: opts.Ref, ProjectURN: opts.ProjectURN})
		if err != nil {
			return WorkstreamPage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}

// ListWorkstreams is ListWorkstreamsPage's rows.
func (s *Store) ListWorkstreams(ctx context.Context, opts ListWorkstreamsOptions) ([]WorkstreamRow, error) {
	page, err := s.ListWorkstreamsPage(ctx, opts)
	return page.Workstreams, err
}
