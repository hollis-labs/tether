package store

// session_refs.go — what a session touched.
//
// S2 of SP-20260912-0001 (CW-20260912-0060); design record CW-20260912-0023.
// The closedness of the kind/ref_id/uri triple, and what `source` does and does
// not prove, are stated once in migration 0024. Read that before extending
// anything here.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Ref kinds. The COLUMN is deliberately unconstrained (migration 0024): a new
// kind means another system became correlatable, which is ordinary growth and
// should not need a migration, and an unexpected kind cannot turn this table
// into a content store.
//
// The discipline lives here instead, because the real hazard is drift rather
// than invention: git_commit / gitcommit / commit are one thing spelled three
// ways, and nothing notices until a digest months later shows three kinds that
// should have been one. Use these constants at the callsite; adding a kind is
// a one-line change here, not a migration.
const (
	KindTorqueTask        = "torque_task"
	KindGitCommit         = "git_commit"
	KindGitPR             = "git_pr"
	KindTesseractRevision = "tesseract_revision"
	KindCerberusDeploy    = "cerberus_deploy"
	KindADR               = "adr"
	KindURL               = "url"
)

// KnownRefKinds is the canonical set. It is NOT enforced on write -- an
// unknown kind is accepted, by design. It exists so callers can check
// themselves and so a drift audit has something to compare against.
var KnownRefKinds = map[string]bool{
	KindTorqueTask: true, KindGitCommit: true, KindGitPR: true,
	KindTesseractRevision: true, KindCerberusDeploy: true,
	KindADR: true, KindURL: true,
}

// IsKnownRefKind reports whether kind is one of the canonical kinds. A false
// answer is not an error -- it is the signal a caller can use to decide
// whether it meant to coin a new kind or has just misspelled an existing one.
func IsKnownRefKind(kind string) bool { return KnownRefKinds[kind] }

// Ref relations. A ref records not just that a session touched an object but
// how, which is what makes the audit answer a useful question.
const (
	RelationCreated    = "created"
	RelationUpdated    = "updated"
	RelationRead       = "read"
	RelationReferenced = "referenced"
)

// Ref sources. See migration 0024 for the full statement — in particular that
// SourceProxy means OBSERVED, not validated, and that the absence of a
// proxy-sourced ref is never evidence of anything.
const (
	// SourceProxy — the mux proxy saw this session make this call carrying
	// this identifier. Not forgeable by the calling agent; not validated
	// either.
	SourceProxy = "proxy"
	// SourceAPI — recorded through Tether's own HTTP surface.
	SourceAPI = "api"
	// SourceAgent — an agent asserted it.
	SourceAgent = "agent"
)

var validRelations = map[string]bool{
	RelationCreated: true, RelationUpdated: true,
	RelationRead: true, RelationReferenced: true,
}

var validSources = map[string]bool{
	SourceProxy: true, SourceAPI: true, SourceAgent: true,
}

// SessionRefRow mirrors the session_refs table.
type SessionRefRow struct {
	ID        int64
	SessionID string
	Kind      string
	RefID     string
	URI       string
	Relation  string
	Source    string
	At        string
}

// AttachRefResult reports what a write actually did. A repeat is a success in
// every case -- the hooks that write git refs can legitimately run twice -- so
// the caller needs this to tell "recorded" from "already knew".
type AttachRefResult struct {
	// Inserted is true when the row did not exist.
	Inserted bool
	// Upgraded is true when an existing row's source was raised to proxy.
	Upgraded bool
}

// AttachSessionRef records that a session touched an object. It is idempotent
// on (session_id, kind, ref_id, relation).
//
// A repeat does NOT revise relation, ref_id, uri or at. Those are claims about
// what happened, and letting a later write rewrite them would turn an audit
// trail into a current view -- the row means "this was recorded at this time".
//
// `source` is the exception, and deliberately so. It is not a claim about the
// world; it is a claim about HOW WE KNOW. Raising `agent` or `api` to `proxy`
// does not rewrite what happened — it records that we now hold better evidence
// for the same unchanged fact. Leaving it alone has a real cost: if an agent
// asserts a ref and the proxy later observes the same call, the row would keep
// source='agent' while we actually hold a proxy observation, so a digest would
// UNDERSTATE its own evidence. That is precisely the failure this column
// exists to prevent.
//
// The upgrade is monotonic and one-way:
//
//   - only `proxy` upgrades. `api` and `agent` are both assertions and neither
//     is stronger, so they never overwrite each other or anything else.
//   - `proxy` is terminal; nothing downgrades it.
//   - ONLY source changes. `at` keeps the first timestamp, because that is
//     when it happened.
//
// The diagnostic worth protecting survives untouched: "the agent claimed X and
// the proxy never saw it" still shows as a lone source='agent' row, because an
// assertion nobody observed is never upgraded.
//
// Read-then-write is safe here without an explicit transaction: the store runs
// on a single connection (SetMaxOpenConns(1) in Open, to serialize all access),
// so no other statement can interleave between the lookup and the write.
//
// THAT IS AN INVARIANT THIS FUNCTION DEPENDS ON, not just a fact about Open.
// If MaxOpenConns is ever raised above 1 -- a reasonable-looking throughput
// change with no visible connection to this file -- this function must become
// an explicit transaction. Without one, two concurrent attaches of the same ref
// would both read "absent", both insert, and one would fail the UNIQUE
// constraint: an error on a path whose whole contract is that a repeat is safe.
func (s *Store) AttachSessionRef(ref SessionRefRow) (AttachRefResult, error) {
	if ref.SessionID == "" {
		return AttachRefResult{}, errors.New("attach session ref: session_id required")
	}
	if ref.Kind == "" {
		return AttachRefResult{}, errors.New("attach session ref: kind required")
	}
	if ref.RefID == "" {
		return AttachRefResult{}, errors.New("attach session ref: ref_id required")
	}
	if ref.Relation == "" {
		ref.Relation = RelationReferenced
	}
	if !validRelations[ref.Relation] {
		return AttachRefResult{}, fmt.Errorf("attach session ref: invalid relation %q (want created, updated, read or referenced)", ref.Relation)
	}
	if ref.Source == "" {
		ref.Source = SourceAgent
	}
	if !validSources[ref.Source] {
		return AttachRefResult{}, fmt.Errorf("attach session ref: invalid source %q (want proxy, api or agent)", ref.Source)
	}
	if ref.At == "" {
		ref.At = time.Now().UTC().Format(time.RFC3339)
	}

	fail := func(err error) (AttachRefResult, error) {
		return AttachRefResult{}, fmt.Errorf("attach session ref %s/%s to %q: %w", ref.Kind, ref.RefID, ref.SessionID, err)
	}

	var existingSource string
	err := s.db.QueryRow(
		`SELECT source FROM session_refs WHERE session_id=? AND kind=? AND ref_id=? AND relation=?`,
		ref.SessionID, ref.Kind, ref.RefID, ref.Relation,
	).Scan(&existingSource)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.Exec(
			`INSERT INTO session_refs (session_id, kind, ref_id, uri, relation, source, at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ref.SessionID, ref.Kind, ref.RefID, nullIfEmpty(ref.URI), ref.Relation, ref.Source, ref.At,
		); err != nil {
			return fail(err)
		}
		return AttachRefResult{Inserted: true}, nil

	case err != nil:
		return fail(err)

	case ref.Source == SourceProxy && existingSource != SourceProxy:
		// Better evidence for the same fact. Only source moves; at stays.
		if _, err := s.db.Exec(
			`UPDATE session_refs SET source=? WHERE session_id=? AND kind=? AND ref_id=? AND relation=?`,
			SourceProxy, ref.SessionID, ref.Kind, ref.RefID, ref.Relation,
		); err != nil {
			return fail(err)
		}
		return AttachRefResult{Upgraded: true}, nil

	default:
		// Already recorded, and this write is no stronger. No-op, not an error.
		return AttachRefResult{}, nil
	}
}

// ListSessionRefsOptions filters a ref listing. Zero value returns everything
// for the scope, newest first.
type ListSessionRefsOptions struct {
	Kind     string
	Relation string
	Source   string
	Limit    int
}

// filterArgs returns the six bound values the shared filter predicate needs.
//
// The predicate is written as `(? = ” OR col = ?)` per filter rather than
// assembled by concatenation, so the SQL text is a compile-time constant and
// every caller-supplied value is bound. That removes the whole question of
// whether a filter could reach the query text -- it cannot, there is no query
// text to reach. An empty filter matches everything via the first arm.
func (o ListSessionRefsOptions) filterArgs() []any {
	return []any{o.Kind, o.Kind, o.Relation, o.Relation, o.Source, o.Source}
}

func (o ListSessionRefsOptions) limit() int {
	if o.Limit <= 0 {
		return 200
	}
	return o.Limit
}

const sessionRefColumns = `r.id, r.session_id, r.kind, r.ref_id, r.uri, r.relation, r.source, r.at`

// sessionRefFilter is the constant predicate shared by both listings.
const sessionRefFilter = `
	  AND (? = '' OR r.kind = ?)
	  AND (? = '' OR r.relation = ?)
	  AND (? = '' OR r.source = ?)
	ORDER BY r.at DESC, r.id DESC
	LIMIT ?`

func scanSessionRefs(rows *sql.Rows) ([]SessionRefRow, error) {
	defer func() { _ = rows.Close() }()
	var out []SessionRefRow
	for rows.Next() {
		var r SessionRefRow
		var uri sql.NullString
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Kind, &r.RefID, &uri, &r.Relation, &r.Source, &r.At); err != nil {
			return nil, fmt.Errorf("scan session ref: %w", err)
		}
		r.URI = uri.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// listSessionRefsQuery and listWorkstreamRefsQuery are whole constants rather
// than a base plus appended fragments, so neither is built at runtime.
const listSessionRefsQuery = `SELECT ` + sessionRefColumns + `
	FROM session_refs r
	WHERE r.session_id = ?` + sessionRefFilter

const listWorkstreamRefsQuery = `SELECT ` + sessionRefColumns + `
	FROM session_refs r
	JOIN sessions s ON s.id = r.session_id
	WHERE s.workstream_id = ?` + sessionRefFilter

// ListSessionRefs returns the refs attached to one session, newest first.
func (s *Store) ListSessionRefs(sessionID string, opts ListSessionRefsOptions) ([]SessionRefRow, error) {
	args := append([]any{sessionID}, opts.filterArgs()...)
	args = append(args, opts.limit())
	rows, err := s.db.Query(listSessionRefsQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("list session refs for %q: %w", sessionID, err)
	}
	return scanSessionRefs(rows)
}

// ListWorkstreamRefs returns every ref attached to any session in a
// workstream, newest first.
//
// The join goes through sessions.workstream_id, which is the ONLY path from a
// ref to a workstream -- session_refs deliberately has no workstream_id of its
// own (migration 0024). That is also what makes roll-up survive a compaction:
// the compact child is a different session row, but S1's inheritance put it in
// the same workstream, so its refs land in the same roll-up as its parent's.
func (s *Store) ListWorkstreamRefs(workstreamID string, opts ListSessionRefsOptions) ([]SessionRefRow, error) {
	args := append([]any{workstreamID}, opts.filterArgs()...)
	args = append(args, opts.limit())
	rows, err := s.db.Query(listWorkstreamRefsQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("list workstream refs for %q: %w", workstreamID, err)
	}
	return scanSessionRefs(rows)
}
