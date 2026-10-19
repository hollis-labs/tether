package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

const DefaultWorkstreamLimit = 100
const MaxWorkstreamLimit = 1000

var ErrWorkstreamPage = errors.New("invalid workstream pagination")

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

// ValidateWorkstreamPage rejects malformed and cross-filter cursors and normalizes limits.
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

// ListWorkstreamsPage uses one lookahead row, so HasMore is exact for this query.
// It is a live keyset scan, not a snapshot spanning subsequent requests.
func (s *Store) ListWorkstreamsPage(opts ListWorkstreamsOptions) (WorkstreamPage, error) {
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
		kind, refID, err = ParseRefSelector(opts.Ref)
		if err != nil {
			return WorkstreamPage{}, ErrWorkstreamPage
		}
	}
	rows, err := s.db.Query(`SELECT w.id, w.name, w.workflow_id, w.status, w.created_at, w.updated_at FROM workstreams w
 WHERE (? = '' OR w.status = ?) AND (? = '' OR w.workflow_id = ?)
 AND (? = '' OR EXISTS (SELECT 1 FROM session_refs r JOIN sessions s ON s.id = r.session_id
 WHERE s.workstream_id = w.id AND r.kind = ? AND r.ref_id = ?))
 AND (? = '' OR EXISTS (SELECT 1 FROM workstream_projects wp WHERE wp.workstream_id=w.id AND wp.project_urn=?))
 AND (? = '' OR (w.created_at, w.id) < (?, ?))
 ORDER BY w.created_at DESC, w.id DESC LIMIT ?`,
		opts.Status, opts.Status, opts.WorkflowID, opts.WorkflowID, opts.Ref, kind, refID, opts.ProjectURN, opts.ProjectURN, opts.Cursor, c.CreatedAt, c.ID, opts.Limit+1)
	if err != nil {
		return WorkstreamPage{}, err
	}
	defer rows.Close()
	items, err := s.scanWorkstreams(rows, "list workstreams page")
	if err != nil {
		return WorkstreamPage{}, err
	}
	page := WorkstreamPage{Workstreams: items, Limit: opts.Limit}
	if len(items) > opts.Limit {
		page.HasMore = true
		page.Workstreams = items[:opts.Limit]
		last := page.Workstreams[len(page.Workstreams)-1]
		c = workstreamCursor{Version: 1, CreatedAt: last.CreatedAt, ID: last.ID, Status: opts.Status, WorkflowID: opts.WorkflowID, Ref: opts.Ref, ProjectURN: opts.ProjectURN}
		b, err := json.Marshal(c)
		if err != nil {
			return WorkstreamPage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}
