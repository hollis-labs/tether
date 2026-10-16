package sessionrefs

import (
	"errors"
	"github.com/hollis-labs/tether/internal/store"
)

// RecordStore is the storage seam for session-ref handlers.
// *store.Store satisfies it.
type RecordStore interface {
	AttachSessionRef(ref store.SessionRefRow) (store.AttachRefResult, error)
	ListSessionRefs(sessionID string, opts store.ListSessionRefsOptions) ([]store.SessionRefRow, error)
	ListWorkstreamRefs(workstreamID string, opts store.ListSessionRefsOptions) ([]store.SessionRefRow, error)
}

// SessionRefDTO is the wire shape for one ref.
type SessionRefDTO struct {
	ID           int64  `json:"id"`
	SessionID    string `json:"session_id"`
	Kind         string `json:"kind"`
	RefID        string `json:"ref_id"`
	URI          string `json:"uri,omitempty"`
	Relation     string `json:"relation"`
	Source       string `json:"source"`
	At           string `json:"at"`
	ParentItemID string `json:"parent_item_id,omitempty"`
}

// SessionRefListResponse is the collection response for a ref listing.
type SessionRefListResponse struct {
	Refs []SessionRefDTO `json:"refs"`
}

// SessionRefAttachRequest carries an explicit capture. The HTTP boundary
// derives Source from verified identity; callers cannot choose provenance.
type SessionRefAttachRequest struct {
	Kind         string `json:"kind"`
	RefID        string `json:"ref_id"`
	URI          string `json:"uri,omitempty"`
	Relation     string `json:"relation,omitempty"`
	Source       string `json:"source,omitempty"`
	ParentItemID string `json:"parent_item_id,omitempty"`
}

// SessionRefAttachResponse reports what the write did. Both flags false is a
// success, not a failure: re-attaching is a no-op so a hook that runs twice
// does not fail.
//
// Upgraded is true when an existing row's source was raised to proxy —
// better evidence for the same unchanged fact, which is the one thing a repeat
// is allowed to revise.
type SessionRefAttachResponse struct {
	Inserted bool          `json:"inserted"`
	Upgraded bool          `json:"upgraded"`
	Ref      SessionRefDTO `json:"ref"`
}

func sessionRefToDTO(r store.SessionRefRow) SessionRefDTO {
	return SessionRefDTO{
		ID:           r.ID,
		SessionID:    r.SessionID,
		Kind:         r.Kind,
		RefID:        r.RefID,
		URI:          r.URI,
		Relation:     r.Relation,
		Source:       r.Source,
		At:           r.At,
		ParentItemID: r.ParentItemID,
	}
}

// Filters carries decoded application query fields, independently of storage.
type Filters struct {
	Kind     string
	Relation string
	Source   string
}

func (f Filters) storageOptions() store.ListSessionRefsOptions {
	return store.ListSessionRefsOptions{Kind: f.Kind, Relation: f.Relation, Source: f.Source}
}

// Records owns explicit captures and DTO conversion behind narrow CRUD seams.
type Records struct{ rows RecordStore }

func NewRecords(rows RecordStore) *Records { return &Records{rows: rows} }
func (s *Records) Attach(sessionID string, req SessionRefAttachRequest) (SessionRefAttachResponse, error) {
	if req.Kind == "" || req.RefID == "" {
		return SessionRefAttachResponse{}, errors.New("kind and ref_id are required")
	}
	source := req.Source
	if source == "" {
		source = store.SourceAPI
	}
	// The HTTP boundary derives Source from verified identity before calling
	// this storage operation. Source is not an authentication input here.
	row := store.SessionRefRow{
		SessionID:    sessionID,
		Kind:         req.Kind,
		RefID:        req.RefID,
		URI:          req.URI,
		Relation:     req.Relation,
		Source:       source,
		ParentItemID: req.ParentItemID,
	}
	res, err := s.rows.AttachSessionRef(row)
	if err != nil {
		return SessionRefAttachResponse{}, err
	}
	if row.Relation == "" {
		row.Relation = store.RelationReferenced
	}
	row.Source = source
	return SessionRefAttachResponse{
		Inserted: res.Inserted, Upgraded: res.Upgraded, Ref: sessionRefToDTO(row),
	}, nil
}
func (s *Records) ListSession(sessionID string, opts Filters) (SessionRefListResponse, error) {
	rows, err := s.rows.ListSessionRefs(sessionID, opts.storageOptions())
	if err != nil {
		return SessionRefListResponse{}, err
	}
	return recordsToDTO(rows), nil
}
func (s *Records) ListWorkstream(workstreamID string, opts Filters) (SessionRefListResponse, error) {
	rows, err := s.rows.ListWorkstreamRefs(workstreamID, opts.storageOptions())
	if err != nil {
		return SessionRefListResponse{}, err
	}
	return recordsToDTO(rows), nil
}
func recordsToDTO(rows []store.SessionRefRow) SessionRefListResponse {
	out := make([]SessionRefDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionRefToDTO(row))
	}
	return SessionRefListResponse{Refs: out}
}
