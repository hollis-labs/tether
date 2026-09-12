package api

// digest.go — HTTP surface for the recovery and audit view.
//
// S5 of SP-20260912-0001 (CW-20260912-0063). Assembly and the reasoning behind
// the shape live in internal/store/digest.go; this file is the wire format and
// nothing else.
//
// Two grains, ONE RESPONSE SHAPE. GET /sessions/{id}/digest and
// GET /workstreams/{id}/digest return the same type, so a consumer writes one
// parser. They answer different questions -- the session grain is the everyday
// "what did I touch, what did I leave behind", the workstream grain is the
// recovery roll-up across a lineage -- but a caller should not need a second
// decoder to ask the second question.

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/hollis-labs/tether/internal/store"
)

// DigestStore is the storage seam for digest handlers. *store.Store satisfies
// it.
type DigestStore interface {
	SessionDigest(sessionID string, opts store.DigestOptions) (store.Digest, error)
	WorkstreamDigest(workstreamID string, opts store.DigestOptions) (store.Digest, error)
	WorkstreamsForRef(kind, refID string) ([]store.WorkstreamRow, error)
}

// DigestRefDTO is one ref in a digest.
type DigestRefDTO struct {
	SessionID string `json:"session_id"`
	RefID     string `json:"ref_id"`
	URI       string `json:"uri,omitempty"`
	Relation  string `json:"relation"`
	Source    string `json:"source"`
	At        string `json:"at"`
}

// DigestKindGroupDTO is one kind's refs, split by relation. Rendered as an
// ordered array rather than an object keyed by kind so the output is stable
// for the same input -- a digest handed to an agent as recovery context should
// not reshuffle between calls.
type DigestKindGroupDTO struct {
	Kind       string         `json:"kind"`
	Created    []DigestRefDTO `json:"created,omitempty"`
	Updated    []DigestRefDTO `json:"updated,omitempty"`
	Read       []DigestRefDTO `json:"read,omitempty"`
	Referenced []DigestRefDTO `json:"referenced,omitempty"`
}

// DigestSessionDTO is one session in the span.
//
// ref_attribution has NO omitempty. It is always meaningful -- "unknown" is an
// answer, not a missing value -- and omitting it when empty would let a
// consumer read its absence as "not applicable", which is the one reading the
// field exists to prevent.
type DigestSessionDTO struct {
	ID              string `json:"id"`
	Intent          string `json:"intent"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	State           string `json:"state"`
	CreatedAt       string `json:"created_at"`
	EndedAt         string `json:"ended_at,omitempty"`
	RefAttribution  string `json:"ref_attribution"`
	RefCount        int    `json:"ref_count"`
}

// DigestSpanDTO is which sessions the digest covers. Always rendered, and
// always with the full roster -- including at session count 1.
//
// A one-session workstream is a first-class answer, not a degenerate case: a
// container that has not compacted yet is simply one that has not compacted
// yet. But rendering it identically to a real roll-up would hide the single
// fact a reviewer needs, which is whether the lineage roll-up exercised at
// all. Hence spans_lineage beside the count.
type DigestSpanDTO struct {
	SessionCount int                `json:"session_count"`
	SpansLineage bool               `json:"spans_lineage"`
	Sessions     []DigestSessionDTO `json:"sessions"`
}

// DigestTotalsDTO counts what the digest actually contains, after filters and
// after the limit.
type DigestTotalsDTO struct {
	Refs       int            `json:"refs"`
	ByRelation map[string]int `json:"by_relation"`
	BySource   map[string]int `json:"by_source"`
}

// DigestCoverageDTO is what the digest did and did not see.
type DigestCoverageDTO struct {
	Limit     int  `json:"limit"`
	Truncated bool `json:"truncated"`
	// Attribution counts the span's sessions by ref_attribution.
	Attribution map[string]int `json:"attribution"`
	// ProxyAttributable is how many of them could produce a source=proxy ref
	// at all. Zero means an empty proxy column says nothing about what the
	// agent did -- see the note field.
	ProxyAttributable int `json:"proxy_attributable"`
	// Note is prose for a human reading the digest directly, present only when
	// the numbers above need qualifying. Machine consumers should read the
	// fields, not this.
	Note string `json:"note,omitempty"`
}

// DigestResponse is the wire shape at both grains.
type DigestResponse struct {
	Grain      string               `json:"grain"`
	SessionID  string               `json:"session_id,omitempty"`
	Workstream *WorkstreamDTO       `json:"workstream,omitempty"`
	Span       DigestSpanDTO        `json:"span"`
	LeftBehind []DigestKindGroupDTO `json:"left_behind"`
	Touched    []DigestKindGroupDTO `json:"touched"`
	Totals     DigestTotalsDTO      `json:"totals"`
	Coverage   DigestCoverageDTO    `json:"coverage"`
}

func digestRefsToDTO(refs []store.DigestRef) []DigestRefDTO {
	if len(refs) == 0 {
		return nil
	}
	out := make([]DigestRefDTO, 0, len(refs))
	for _, r := range refs {
		out = append(out, DigestRefDTO{
			SessionID: r.SessionID, RefID: r.RefID, URI: r.URI,
			Relation: r.Relation, Source: r.Source, At: r.At,
		})
	}
	return out
}

func digestGroupsToDTO(groups []store.DigestKindGroup) []DigestKindGroupDTO {
	// Never nil: an empty section renders as [] rather than null, so a
	// consumer does not have to handle two encodings of "nothing here".
	out := make([]DigestKindGroupDTO, 0, len(groups))
	for _, g := range groups {
		out = append(out, DigestKindGroupDTO{
			Kind:       g.Kind,
			Created:    digestRefsToDTO(g.Created),
			Updated:    digestRefsToDTO(g.Updated),
			Read:       digestRefsToDTO(g.Read),
			Referenced: digestRefsToDTO(g.Referenced),
		})
	}
	return out
}

// coverageNote qualifies an empty proxy column so nobody reads it as a
// finding.
//
// This is the operational end of the discipline migration 0024 states and
// migration 0025 makes measurable: absent must never imply forged, and it must
// never imply idle either. A digest with no proxy-sourced refs over a span
// where nothing COULD produce one is reporting a configuration, not an
// activity level, and saying so costs one string.
func coverageNote(span store.DigestSpan, cov store.DigestCoverage) string {
	if span.SessionCount == 0 {
		return "this workstream contains no sessions, so there is nothing to report on rather than nothing having happened"
	}
	if cov.ProxyAttributable > 0 {
		return ""
	}
	if cov.Attribution[store.RefAttributionUnlaunched] == span.SessionCount {
		return "no session here was launched by Tether, so no proxy carries its id: the absence of proxy-observed refs is a fact about attribution, not about what these sessions did"
	}
	return "no session here could produce a proxy-observed ref, because ref extraction was not enabled for it (CW-20260912-0112): an empty proxy column says nothing about what these sessions did"
}

func digestToDTO(d store.Digest) DigestResponse {
	resp := DigestResponse{
		Grain:      d.Grain,
		SessionID:  d.SessionID,
		LeftBehind: digestGroupsToDTO(d.LeftBehind),
		Touched:    digestGroupsToDTO(d.Touched),
		Totals: DigestTotalsDTO{
			Refs:       d.Totals.Refs,
			ByRelation: d.Totals.ByRelation,
			BySource:   d.Totals.BySource,
		},
		Coverage: DigestCoverageDTO{
			Limit:             d.Coverage.Limit,
			Truncated:         d.Coverage.Truncated,
			Attribution:       d.Coverage.Attribution,
			ProxyAttributable: d.Coverage.ProxyAttributable,
			Note:              coverageNote(d.Span, d.Coverage),
		},
	}
	if d.Workstream != nil {
		dto := workstreamToDTO(*d.Workstream)
		resp.Workstream = &dto
	}
	sessions := make([]DigestSessionDTO, 0, len(d.Span.Sessions))
	for _, s := range d.Span.Sessions {
		sessions = append(sessions, DigestSessionDTO{
			ID: s.ID, Intent: s.Intent, ParentSessionID: s.ParentSessionID,
			State: s.State, CreatedAt: s.CreatedAt, EndedAt: s.EndedAt,
			RefAttribution: s.RefAttribution, RefCount: s.RefCount,
		})
	}
	resp.Span = DigestSpanDTO{
		SessionCount: d.Span.SessionCount,
		SpansLineage: d.Span.SpansLineage,
		Sessions:     sessions,
	}
	return resp
}

// digestOptions reads the shared filter query parameters.
func digestOptions(r *http.Request) store.DigestOptions {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return store.DigestOptions{
		Kind:     r.URL.Query().Get("kind"),
		Relation: r.URL.Query().Get("relation"),
		Source:   r.URL.Query().Get("source"),
		Since:    r.URL.Query().Get("since"),
		Limit:    limit,
	}
}

// handleSessionDigest services GET /sessions/{id}/digest.
func (s *Server) handleSessionDigest(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.Digests == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "digests are not enabled on this server")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	d, err := s.Digests.SessionDigest(sessionID, digestOptions(r))
	if err != nil {
		writeDigestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, digestToDTO(d))
}

// handleWorkstreamDigest services GET /workstreams/{id}/digest.
func (s *Server) handleWorkstreamDigest(w http.ResponseWriter, r *http.Request, workstreamID string) {
	if s.Digests == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "digests are not enabled on this server")
		return
	}
	d, err := s.Digests.WorkstreamDigest(workstreamID, digestOptions(r))
	if err != nil {
		writeDigestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, digestToDTO(d))
}

func writeDigestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrWorkstreamNotFound), errors.Is(err, store.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}
