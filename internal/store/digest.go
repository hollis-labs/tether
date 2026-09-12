package store

// digest.go — the recovery and audit view: what a session, or a whole
// workstream across its lineage, actually touched and actually left behind.
//
// S5 of SP-20260912-0001 (CW-20260912-0063); design record CW-20260912-0023.
// This is the task the four before it were plumbing for, so most of what is
// here is assembly rather than new storage. Three properties are load-bearing
// and are the reason this is a type rather than a rendering concern:
//
//   1. RELATION IS SPLIT, NOT FLATTENED. "Touched 14 tasks" is noise. The
//      split that matters to a reader is created/updated (LeftBehind) against
//      read/referenced (Touched) -- what this work produced versus what it
//      consulted.
//   2. SOURCE RIDES ON EVERY REF. An operator distinguishing what the proxy
//      observed from what an agent claimed needs it at the item, not summed in
//      a total. See migration 0024 for what `source` does and does not prove.
//   3. ABSENCE IS QUALIFIED. Every session in the span carries its
//      RefAttribution, so "no refs" is never rendered as a bare empty set when
//      the real answer is "this session could not produce one". Migration 0025.

import (
	"fmt"
	"sort"
	"strings"
)

// Digest grains.
const (
	// GrainSession — one session's own refs. The everyday view: what did I
	// touch, what did I leave behind.
	GrainSession = "session"
	// GrainWorkstream — every session in the container, rolled up. The
	// recovery view, and the only grain where a compaction becomes visible.
	GrainWorkstream = "workstream"
)

// digestDefaultLimit bounds a digest's ref set.
//
// It is reported rather than applied silently -- see DigestCoverage. A digest
// that quietly drops refs would let a reader conclude work did not happen
// because the list ended, which is the same absent-implies-something failure
// the rest of this feature is built to avoid.
const digestDefaultLimit = 500

// DigestOptions filters the refs a digest assembles. Zero value takes
// everything within the default limit.
type DigestOptions struct {
	Kind     string
	Relation string
	Source   string
	// Since is an RFC3339 UTC lower bound on a ref's `at`.
	Since string
	Limit int
}

func (o DigestOptions) limit() int {
	if o.Limit <= 0 {
		return digestDefaultLimit
	}
	return o.Limit
}

func (o DigestOptions) listOptions(limit int) ListSessionRefsOptions {
	return ListSessionRefsOptions{
		Kind: o.Kind, Relation: o.Relation, Source: o.Source,
		Since: o.Since, Limit: limit,
	}
}

// DigestSession is one session in a digest's span.
//
// RefAttribution is present on every entry and never omitted when empty: a
// missing field reads as "not applicable", and the whole point of the value is
// that it is always applicable. NULL normalises to "unknown", which is a
// distinct answer from "none" -- see migration 0025.
type DigestSession struct {
	ID              string
	Intent          string
	ParentSessionID string
	State           string
	CreatedAt       string
	EndedAt         string
	RefAttribution  string
	// RefCount is how many of the digest's refs came from this session.
	RefCount int
}

// DigestSpan is which sessions the digest covers.
type DigestSpan struct {
	SessionCount int
	// SpansLineage is true iff some session in the span has a parent that is
	// ALSO in the span.
	//
	// NOT merely SessionCount > 1. A workstream assembled by three manual
	// assigns with no parent links has not crossed a compaction, and reporting
	// true for it would hide the single fact this field exists to expose. The
	// sprint's premise is that a compaction creates a new session row and
	// orphans anything keyed on the old one; "the roll-up crossed that
	// boundary" and "the roll-up is large" are different claims and only the
	// first one is evidence the container did its job.
	SpansLineage bool
	Sessions     []DigestSession
}

// DigestRef is one ref as a digest renders it. Same data as SessionRefRow with
// the session id kept, because at workstream grain a reader needs to know
// which session in the lineage produced a given ref.
type DigestRef struct {
	SessionID string
	RefID     string
	URI       string
	Relation  string
	Source    string
	At        string
}

// DigestKindGroup is one kind's refs, split by relation.
//
// A slice of these rather than a map keyed by kind: JSON objects have no
// defined order, and a digest meant to be handed to an agent as recovery
// context should be byte-stable for the same input. Groups are ordered by
// their most recent activity, so what happened last reads first.
type DigestKindGroup struct {
	Kind string
	// Created and Updated are populated in LeftBehind; Read and Referenced in
	// Touched. Each group appears in exactly one of the two sections.
	Created    []DigestRef
	Updated    []DigestRef
	Read       []DigestRef
	Referenced []DigestRef
}

// DigestTotals counts the refs the digest actually contains -- after filters
// and after the limit, so it always matches what was rendered rather than what
// the store holds.
type DigestTotals struct {
	Refs       int
	ByRelation map[string]int
	BySource   map[string]int
}

// DigestCoverage is what the digest did and did not see. It exists so a reader
// never has to infer completeness from the absence of a marker.
type DigestCoverage struct {
	Limit     int
	Truncated bool
	// Attribution counts sessions in the span by ref_attribution value. A span
	// whose sessions are all "none" or "unknown" CANNOT contain a proxy-sourced
	// ref, and a consumer reading an empty BySource["proxy"] must check here
	// before concluding anything about what the agent did.
	Attribution map[string]int
	// ProxyAttributable is how many sessions in the span could produce a
	// source='proxy' ref at all. Zero is the expected value today
	// (CW-20260912-0112: nothing can turn extraction on), and a digest
	// rendering it must say so rather than showing an empty proxy column.
	ProxyAttributable int
}

// Digest is the assembled answer at either grain.
type Digest struct {
	Grain string
	// Workstream is the container. Nil at session grain when the session
	// belongs to none -- in which case there is no roll-up to escalate to,
	// which is itself worth seeing.
	Workstream *WorkstreamRow
	// SessionID is set at session grain, empty at workstream grain.
	SessionID  string
	Span       DigestSpan
	LeftBehind []DigestKindGroup
	Touched    []DigestKindGroup
	Totals     DigestTotals
	Coverage   DigestCoverage
}

// SessionDigest assembles the digest for one session's own refs.
//
// This is the grain most calls will use -- the end-of-session "what did I
// touch, what did I leave behind" -- and it deliberately does NOT roll up the
// lineage. Rolling up is what the workstream grain is; conflating them would
// leave no way to ask the narrower question. The workstream is still reported
// so a caller can escalate to the roll-up in one hop.
func (s *Store) SessionDigest(sessionID string, opts DigestOptions) (Digest, error) {
	row, err := s.GetSession(sessionID)
	if err != nil {
		return Digest{}, fmt.Errorf("session digest for %q: %w", sessionID, err)
	}

	limit := opts.limit()
	refs, truncated, err := s.digestRefs(func(l int) ([]SessionRefRow, error) {
		return s.ListSessionRefs(sessionID, opts.listOptions(l))
	}, limit)
	if err != nil {
		return Digest{}, fmt.Errorf("session digest for %q: %w", sessionID, err)
	}

	d := Digest{Grain: GrainSession, SessionID: sessionID}
	if row.WorkstreamID.Valid && row.WorkstreamID.String != "" {
		ws, err := s.GetWorkstream(row.WorkstreamID.String)
		if err != nil {
			// Dangling pointer: report it rather than rendering the session as
			// container-less, which would look identical to never having had
			// one. FK enforcement is off (ADR 0008) so this is reachable.
			return Digest{}, fmt.Errorf("session digest for %q: references workstream %q: %w", sessionID, row.WorkstreamID.String, err)
		}
		d.Workstream = ws
	}
	assembleDigest(&d, []SessionRow{*row}, refs, limit, truncated)
	return d, nil
}

// WorkstreamDigest assembles the roll-up across every session in the
// container.
//
// This is the grain the sprint exists for: a workstream spans fresh -> compact
// -> resume, so a digest here shows work from before a compaction alongside
// work from after it. Span.SpansLineage reports whether that actually happened
// for this workstream rather than leaving a reader to assume it from the size.
func (s *Store) WorkstreamDigest(workstreamID string, opts DigestOptions) (Digest, error) {
	ws, err := s.GetWorkstream(workstreamID)
	if err != nil {
		return Digest{}, fmt.Errorf("workstream digest for %q: %w", workstreamID, err)
	}
	sessions, err := s.sessionsInWorkstream(workstreamID)
	if err != nil {
		return Digest{}, fmt.Errorf("workstream digest for %q: %w", workstreamID, err)
	}

	limit := opts.limit()
	refs, truncated, err := s.digestRefs(func(l int) ([]SessionRefRow, error) {
		return s.ListWorkstreamRefs(workstreamID, opts.listOptions(l))
	}, limit)
	if err != nil {
		return Digest{}, fmt.Errorf("workstream digest for %q: %w", workstreamID, err)
	}

	d := Digest{Grain: GrainWorkstream, Workstream: ws}
	assembleDigest(&d, sessions, refs, limit, truncated)
	return d, nil
}

// digestRefs fetches with limit+1 so truncation is DETECTED rather than
// assumed, then discards the extra.
//
// Asking for exactly `limit` cannot distinguish "there were exactly this many"
// from "there were more" -- and the two readings differ in whether the digest
// is complete, which is the one thing a recovery reader must not guess at.
func (s *Store) digestRefs(list func(limit int) ([]SessionRefRow, error), limit int) ([]SessionRefRow, bool, error) {
	rows, err := list(limit + 1)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// sessionsInWorkstream returns the container's sessions, oldest first so a
// lineage reads in the order it happened.
func (s *Store) sessionsInWorkstream(workstreamID string) ([]SessionRow, error) {
	rows, err := s.db.Query(
		`SELECT id, launch_id, project_id, logical_agent_id, provider_id, provider_kind,
		        workspace, state, pid, exit_code, created_at, updated_at, ended_at,
		        session_group_id, parent_session_id, intent, publication, workstream_id,
		        ref_attribution
		 FROM sessions WHERE workstream_id = ? ORDER BY created_at ASC`, workstreamID)
	if err != nil {
		return nil, fmt.Errorf("sessions in workstream %q: %w", workstreamID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ID, &r.LaunchID, &r.ProjectID, &r.LogicalAgentID, &r.ProviderID,
			&r.ProviderKind, &r.Workspace, &r.State, &r.PID, &r.ExitCode, &r.CreatedAt,
			&r.UpdatedAt, &r.EndedAt, &r.SessionGroupID, &r.ParentSessionID, &r.Intent,
			&r.Publication, &r.WorkstreamID, &r.RefAttribution); err != nil {
			return nil, fmt.Errorf("sessions in workstream %q: scan: %w", workstreamID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// assembleDigest fills the span, groups, totals and coverage. Pure over its
// inputs -- no further queries -- so the shape is testable without a DB.
func assembleDigest(d *Digest, sessions []SessionRow, refs []SessionRefRow, limit int, truncated bool) {
	refsBySession := map[string]int{}
	for _, r := range refs {
		refsBySession[r.SessionID]++
	}

	inSpan := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		inSpan[sess.ID] = true
	}

	span := DigestSpan{SessionCount: len(sessions)}
	attribution := map[string]int{}
	proxyAttributable := 0
	for _, sess := range sessions {
		attr := NormalizeRefAttribution(sess.RefAttribution.String)
		attribution[attr]++
		if CanProduceProxyRefs(attr) {
			proxyAttributable++
		}
		parent := sess.ParentSessionID.String
		if parent != "" && inSpan[parent] {
			span.SpansLineage = true
		}
		span.Sessions = append(span.Sessions, DigestSession{
			ID:              sess.ID,
			Intent:          sess.Intent,
			ParentSessionID: parent,
			State:           sess.State,
			CreatedAt:       sess.CreatedAt,
			EndedAt:         sess.EndedAt.String,
			RefAttribution:  attr,
			RefCount:        refsBySession[sess.ID],
		})
	}
	d.Span = span

	d.LeftBehind, d.Touched = groupRefs(refs)
	d.Totals = digestTotals(refs)
	d.Coverage = DigestCoverage{
		Limit: limit, Truncated: truncated,
		Attribution: attribution, ProxyAttributable: proxyAttributable,
	}
}

// groupRefs splits refs into the left-behind and touched sections, each
// grouped by kind.
//
// The split is on relation: created/updated is what this work PRODUCED,
// read/referenced is what it CONSULTED. That line is the one a reviewer and a
// recovery instruction both care about, and flattening it back into a single
// "touched" count is the specific failure the task names.
//
// An unknown relation -- possible because AttachSessionRef constrains the
// value but a future migration could widen it -- lands in Touched rather than
// being dropped. Losing a ref because its relation is unrecognized would make
// the digest silently incomplete; putting it in the weaker section understates
// it, which is the safe direction.
func groupRefs(refs []SessionRefRow) (leftBehind, touched []DigestKindGroup) {
	type buckets struct {
		created, updated, read, referenced []DigestRef
		latest                             string
	}
	byKind := map[string]*buckets{}
	order := []string{}

	for _, r := range refs {
		b, ok := byKind[r.Kind]
		if !ok {
			b = &buckets{}
			byKind[r.Kind] = b
			order = append(order, r.Kind)
		}
		if r.At > b.latest {
			b.latest = r.At
		}
		ref := DigestRef{
			SessionID: r.SessionID, RefID: r.RefID, URI: r.URI,
			Relation: r.Relation, Source: r.Source, At: r.At,
		}
		switch r.Relation {
		case RelationCreated:
			b.created = append(b.created, ref)
		case RelationUpdated:
			b.updated = append(b.updated, ref)
		case RelationRead:
			b.read = append(b.read, ref)
		default:
			b.referenced = append(b.referenced, ref)
		}
	}

	// Most recent activity first, kind name as the tiebreaker so equal
	// timestamps still produce a stable order rather than map iteration order.
	sort.SliceStable(order, func(i, j int) bool {
		li, lj := byKind[order[i]].latest, byKind[order[j]].latest
		if li != lj {
			return li > lj
		}
		return order[i] < order[j]
	})

	for _, kind := range order {
		b := byKind[kind]
		if len(b.created) > 0 || len(b.updated) > 0 {
			leftBehind = append(leftBehind, DigestKindGroup{
				Kind: kind, Created: b.created, Updated: b.updated,
			})
		}
		if len(b.read) > 0 || len(b.referenced) > 0 {
			touched = append(touched, DigestKindGroup{
				Kind: kind, Read: b.read, Referenced: b.referenced,
			})
		}
	}
	return leftBehind, touched
}

func digestTotals(refs []SessionRefRow) DigestTotals {
	t := DigestTotals{
		Refs:       len(refs),
		ByRelation: map[string]int{},
		BySource:   map[string]int{},
	}
	for _, r := range refs {
		t.ByRelation[r.Relation]++
		t.BySource[r.Source]++
	}
	return t
}

// ParseRefSelector splits a "<kind>:<ref_id>" selector.
//
// SPLIT ON THE FIRST COLON ONLY. Ref ids routinely contain colons --
// `msg://agent/agent-mux/agt_x9k2p4` is the everyday case -- and splitting on
// every colon yields kind="messaging_urn", ref_id="msg", which matches nothing
// and returns an empty result with no error. That reads as "no workstream
// touched this", which is a wrong answer wearing the clothes of a right one.
//
// A first-colon split is unambiguous because kinds are underscore-cased and
// colon-free by construction (see KnownRefKinds).
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

// WorkstreamsForRef answers the reverse question: which workstreams contain a
// session that touched this object.
//
// IT RETURNS ALL MATCHES AND NEVER PICKS ONE. Two separate efforts touching
// the same task is ordinary -- a fix and a later revert, two agents on one
// epic -- so the honest answer to "which workstream did CW-... happen in" is
// sometimes more than one. Selecting the most recent, the largest, or the
// first would produce a single authoritative-looking answer that is wrong
// whenever the ambiguity is real, and the caller would have no way to tell.
// A list makes the ambiguity visible at the only point anyone can resolve it.
//
// Sessions with no workstream are skipped rather than reported: they have no
// container to return, and a digest is a container-grained thing. A caller
// wanting those should ask at session grain.
func (s *Store) WorkstreamsForRef(kind, refID string) ([]WorkstreamRow, error) {
	if kind == "" || refID == "" {
		return nil, fmt.Errorf("workstreams for ref: kind and ref_id are required")
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT w.id, w.name, w.workflow_id, w.status, w.created_at, w.updated_at
		 FROM session_refs r
		 JOIN sessions s ON s.id = r.session_id
		 JOIN workstreams w ON w.id = s.workstream_id
		 WHERE r.kind = ? AND r.ref_id = ?
		 ORDER BY w.created_at DESC`, kind, refID)
	if err != nil {
		return nil, fmt.Errorf("workstreams for ref %s/%s: %w", kind, refID, err)
	}
	defer func() { _ = rows.Close() }()
	return scanWorkstreams(rows, fmt.Sprintf("workstreams for ref %s/%s", kind, refID))
}
