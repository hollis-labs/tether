package workstore

import (
	"context"
	"database/sql"
	"fmt"

	gostore "github.com/hollis-labs/libs/core/store"
)

// The session_refs kinds WorkstreamsForItem reads (sessionstore has the same constants; this slice
// does not import it).
const (
	KindTesseractItem     = "tesseract_item"
	KindTesseractRevision = "tesseract_revision"
)

// DigestSessionRow is the part of a session the workstream digest reads. Empty strings are NULL.
type DigestSessionRow struct {
	ID, LaunchID, ProjectID, LogicalAgentID, ProviderID, ProviderKind, Workspace, State string
	CreatedAt, UpdatedAt, EndedAt                                                       string
	SessionGroupID, ParentSessionID, Intent, Publication, WorkstreamID, RefAttribution  string
}

// SessionsInWorkstream returns the workstream's sessions, oldest first (ties by id). This and the
// session_refs listings (sessionstore) are the rows the workstream digest is built from; building
// it (grouping, totals, coverage, attribution) is the workstream slice's.
func (s *Store) SessionsInWorkstream(ctx context.Context, workstreamID string) ([]DigestSessionRow, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT id, launch_id, project_id, logical_agent_id, provider_id, provider_kind, workspace, state,
 created_at, updated_at, ended_at, session_group_id, parent_session_id, intent, publication, workstream_id, ref_attribution
 FROM sessions WHERE workstream_id = ? ORDER BY created_at ASC, id ASC`), workstreamID)
	if err != nil {
		return nil, fmt.Errorf("sessions in workstream %q: %w", workstreamID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []DigestSessionRow
	for rows.Next() {
		var d DigestSessionRow
		var ended, group, parent, ws, ref sql.NullString
		if err := rows.Scan(&d.ID, &d.LaunchID, &d.ProjectID, &d.LogicalAgentID, &d.ProviderID, &d.ProviderKind, &d.Workspace, &d.State,
			&d.CreatedAt, &d.UpdatedAt, &ended, &group, &parent, &d.Intent, &d.Publication, &ws, &ref); err != nil {
			return nil, fmt.Errorf("sessions in workstream %q: scan: %w", workstreamID, err)
		}
		d.EndedAt, d.SessionGroupID, d.ParentSessionID, d.WorkstreamID, d.RefAttribution = ended.String, group.String, parent.String, ws.String, ref.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// WorkstreamsForRef returns every workstream holding a session that touched the object, newest
// first (ties by id; the old query had none). It returns all matches and never picks one.
func (s *Store) WorkstreamsForRef(ctx context.Context, kind, refID string) ([]WorkstreamRow, error) {
	if kind == "" || refID == "" {
		return nil, fmt.Errorf("workstreams for ref: kind and ref_id are required")
	}
	var out []WorkstreamRow
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		out, err = workstreams(ctx, q, `SELECT DISTINCT `+workstreamColumns+` FROM session_refs r
 JOIN sessions s ON s.id = r.session_id JOIN workstreams w ON w.id = s.workstream_id
 WHERE r.kind = ? AND r.ref_id = ? ORDER BY w.created_at DESC, w.id DESC`, kind, refID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("workstreams for ref %s/%s: %w", kind, refID, err)
	}
	return out, nil
}

// WorkstreamsForItem is WorkstreamsForRef for a Tesseract item, also matching its connected
// revisions.
func (s *Store) WorkstreamsForItem(ctx context.Context, itemID string, connectedRevisionIDs ...string) ([]WorkstreamRow, error) {
	if itemID == "" {
		return nil, fmt.Errorf("workstreams for item: item_id is required")
	}
	if len(connectedRevisionIDs) == 0 {
		return s.WorkstreamsForRef(ctx, KindTesseractItem, itemID)
	}
	args := []any{KindTesseractItem, itemID, KindTesseractRevision}
	for _, rev := range connectedRevisionIDs {
		args = append(args, rev)
	}
	var out []WorkstreamRow
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		out, err = workstreams(ctx, q, `SELECT DISTINCT `+workstreamColumns+` FROM session_refs r
 JOIN sessions s ON s.id = r.session_id JOIN workstreams w ON w.id = s.workstream_id
 WHERE (r.kind = ? AND r.ref_id = ?) OR (r.kind = ? AND r.ref_id IN (`+placeholders(len(connectedRevisionIDs))+`))
 ORDER BY w.created_at DESC, w.id DESC`, args...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("workstreams for item %s: %w", itemID, err)
	}
	return out, nil
}
