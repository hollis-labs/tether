package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hollis-labs/tether/next/internal/store"
)

// session_refs: what a session touched (old session_refs.go, design record CW-20260912-0023; the
// closedness of kind/ref_id/uri and what source does and does not prove are stated in migration
// 0024 of the old tree).

// Ref kinds. The column is unconstrained by design (a new kind is ordinary growth); these
// constants are the canonical spellings, so callers do not drift (git_commit vs commit).
const (
	KindForumPost         = "forum_post"
	KindTorqueTask        = "torque_task"
	KindGitCommit         = "git_commit"
	KindGitPR             = "git_pr"
	KindTesseractItem     = "tesseract_item"
	KindTesseractRevision = "tesseract_revision"
	KindTesseractKey      = "tesseract_key"
	KindCerberusDeploy    = "cerberus_deploy"
	KindADR               = "adr"
	KindURL               = "url"
)

// KnownRefKinds is the canonical set. It is not enforced on write: an unknown kind is accepted.
var KnownRefKinds = map[string]bool{
	KindForumPost: true, KindTorqueTask: true, KindGitCommit: true, KindGitPR: true,
	KindTesseractItem: true, KindTesseractRevision: true, KindTesseractKey: true,
	KindCerberusDeploy: true, KindADR: true, KindURL: true,
}

// IsKnownRefKind reports whether kind is one of the canonical kinds (false is not an error).
func IsKnownRefKind(kind string) bool { return KnownRefKinds[kind] }

// Ref relations: how the session touched the object.
const (
	RelationCreated    = "created"
	RelationUpdated    = "updated"
	RelationRead       = "read"
	RelationReferenced = "referenced"
)

// Ref sources: how we know (old migration 0080). No source validates an identifier, and an absent
// ref is never evidence that a session did nothing.
const (
	// SourceProxy is traffic observed by the hub-owned MCP handler; HTTP callers cannot assert it.
	SourceProxy = "proxy"
	// SourceProxyClaimed is a historical, unverified assertion, never observed evidence.
	SourceProxyClaimed = "proxy_claimed"
	// SourceSession and SourceOperator are derived from verified credentials.
	SourceSession  = "session"
	SourceOperator = "operator"
	// SourceAPI is recorded through Tether's own HTTP surface.
	SourceAPI = "api"
	// SourceAgent is an agent's assertion.
	SourceAgent = "agent"
)

var refsValidRelations = map[string]bool{
	RelationCreated: true, RelationUpdated: true, RelationRead: true, RelationReferenced: true,
}

var refsValidSources = map[string]bool{
	SourceProxy: true, SourceAPI: true, SourceAgent: true, SourceProxyClaimed: true, SourceSession: true, SourceOperator: true,
}

// SessionRefRow is one session_refs row. The canonical fields are derived (CW-20260914-0003) and
// not stored; the caller fills them.
type SessionRefRow struct {
	ID                                                int64
	SessionID, Kind, RefID, URI, Relation, Source, At string
	ParentItemID, CanonicalKind, CanonicalRefID       string
}

// CanonicalRef returns the identity used to deduplicate direct item captures against revision and
// key captures of the same item.
func (r SessionRefRow) CanonicalRef() (kind, refID string) {
	if r.CanonicalKind != "" && r.CanonicalRefID != "" {
		return r.CanonicalKind, r.CanonicalRefID
	}
	if (r.Kind == KindTesseractRevision || r.Kind == KindTesseractKey) && r.ParentItemID != "" {
		return KindTesseractItem, r.ParentItemID
	}
	return r.Kind, r.RefID
}

// AttachRefResult reports what a write did; a repeat is a success either way.
type AttachRefResult struct {
	Inserted bool // the row did not exist
	Upgraded bool // an existing row's source was raised to proxy
}

// AttachSessionRef records that a session touched an object, idempotent on (session_id, kind,
// ref_id, relation). A repeat never revises relation, ref_id, uri or at (the row means "recorded
// at this time"). Two things may change on a repeat, both one-way: a proxy observation raises a
// non-proxy source to proxy (better evidence for the same fact; proxy is terminal, no other source
// overwrites another), and an unset parent_item_id is filled.
//
// The old store did a SELECT then an INSERT or UPDATE and relied on its single connection. Here it
// is one class C transaction of writes that each decide for themselves: INSERT ... ON CONFLICT DO
// NOTHING, then guarded UPDATEs (source <> 'proxy'; parent_item_id IS NULL) whose row counts are
// the result. Concurrent attaches of one ref (Postgres) therefore report exactly one Inserted and
// at most one Upgraded, and never fail on the unique constraint.
func (s *Store) AttachSessionRef(ctx context.Context, ref SessionRefRow) (AttachRefResult, error) {
	switch {
	case ref.SessionID == "":
		return AttachRefResult{}, errors.New("attach session ref: session_id required")
	case ref.Kind == "":
		return AttachRefResult{}, errors.New("attach session ref: kind required")
	case ref.RefID == "":
		return AttachRefResult{}, errors.New("attach session ref: ref_id required")
	}
	if ref.Relation == "" {
		ref.Relation = RelationReferenced
	}
	if !refsValidRelations[ref.Relation] {
		return AttachRefResult{}, fmt.Errorf("attach session ref: invalid relation %q (want created, updated, read or referenced)", ref.Relation)
	}
	if ref.Source == "" {
		ref.Source = SourceAgent
	}
	if !refsValidSources[ref.Source] {
		return AttachRefResult{}, fmt.Errorf("attach session ref: invalid source %q", ref.Source)
	}
	if ref.At == "" {
		ref.At = s.secondStamp()
	}
	var result AttachRefResult
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		result = AttachRefResult{}
		key := []any{ref.SessionID, ref.Kind, ref.RefID, ref.Relation}
		n, err := refsExec(ctx, tx, `INSERT INTO session_refs (session_id, kind, ref_id, uri, relation, source, at, parent_item_id)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (session_id, kind, ref_id, relation) DO NOTHING`,
			ref.SessionID, ref.Kind, ref.RefID, nullText(ref.URI), ref.Relation, ref.Source, ref.At, nullText(ref.ParentItemID))
		if err != nil || n == 1 {
			result.Inserted = n == 1
			return err
		}
		if ref.Source == SourceProxy {
			n, err := refsExec(ctx, tx, `UPDATE session_refs SET source = ?, parent_item_id = COALESCE(parent_item_id, ?)
 WHERE session_id = ? AND kind = ? AND ref_id = ? AND relation = ? AND source <> ?`,
				append(append([]any{SourceProxy, nullText(ref.ParentItemID)}, key...), SourceProxy)...)
			if err != nil || n == 1 {
				result.Upgraded = n == 1
				return err
			}
		}
		if ref.ParentItemID != "" {
			_, err := refsExec(ctx, tx, `UPDATE session_refs SET parent_item_id = ?
 WHERE session_id = ? AND kind = ? AND ref_id = ? AND relation = ? AND parent_item_id IS NULL`,
				append([]any{ref.ParentItemID}, key...)...)
			return err
		}
		return nil
	})
	if err != nil {
		return AttachRefResult{}, fmt.Errorf("attach session ref %s/%s to %q: %w", ref.Kind, ref.RefID, ref.SessionID, err)
	}
	return result, nil
}

func refsExec(ctx context.Context, tx store.Tx, q string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, rebind(tx, q), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListSessionRefsOptions filters a ref listing; the zero value lists everything, newest first.
type ListSessionRefsOptions struct {
	Kind, Relation, Source string
	// Since bounds the listing to refs recorded at or after this UTC RFC 3339 timestamp, compared as
	// text (at is always written fixed-width UTC; the column sorts bytewise on both dialects).
	Since string
	Limit int // default 200
}

func (o ListSessionRefsOptions) refsArgs() []any {
	limit := o.Limit
	if limit <= 0 {
		limit = 200
	}
	return []any{o.Kind, o.Kind, o.Relation, o.Relation, o.Source, o.Source, o.Since, o.Since, limit}
}

const refsColumns = `r.id, r.session_id, r.kind, r.ref_id, r.uri, r.relation, r.source, r.at, r.parent_item_id`

// refsFilter is the constant predicate both listings share: every filter is bound, an empty one
// matches everything. The CASTs type the bare parameters as text on Postgres.
const refsFilter = `
 AND (CAST(? AS TEXT) = '' OR r.kind = ?)
 AND (CAST(? AS TEXT) = '' OR r.relation = ?)
 AND (CAST(? AS TEXT) = '' OR r.source = ?)
 AND (CAST(? AS TEXT) = '' OR r.at >= ?)
 ORDER BY r.at DESC, r.id DESC LIMIT ?`

// ListSessionRefs returns the refs attached to one session, newest first.
func (s *Store) ListSessionRefs(ctx context.Context, sessionID string, opts ListSessionRefsOptions) ([]SessionRefRow, error) {
	refs, err := s.refsList(ctx, `SELECT `+refsColumns+` FROM session_refs r WHERE r.session_id = ?`+refsFilter,
		append([]any{sessionID}, opts.refsArgs()...))
	if err != nil {
		return nil, fmt.Errorf("list session refs for %q: %w", sessionID, err)
	}
	return refs, nil
}

// ListWorkstreamRefs returns every ref attached to any session in a workstream, newest first. The
// join through sessions.workstream_id is the only path from a ref to a workstream (session_refs has
// no workstream column), which is what keeps a compacted child's refs in its parent's roll-up.
func (s *Store) ListWorkstreamRefs(ctx context.Context, workstreamID string, opts ListSessionRefsOptions) ([]SessionRefRow, error) {
	refs, err := s.refsList(ctx, `SELECT `+refsColumns+` FROM session_refs r JOIN sessions s ON s.id = r.session_id
 WHERE s.workstream_id = ?`+refsFilter, append([]any{workstreamID}, opts.refsArgs()...))
	if err != nil {
		return nil, fmt.Errorf("list workstream refs for %q: %w", workstreamID, err)
	}
	return refs, nil
}

func (s *Store) refsList(ctx context.Context, q string, args []any) ([]SessionRefRow, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, q), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SessionRefRow
	for rows.Next() {
		var ref SessionRefRow
		var uri, parent sql.NullString
		if err := rows.Scan(&ref.ID, &ref.SessionID, &ref.Kind, &ref.RefID, &uri, &ref.Relation, &ref.Source, &ref.At, &parent); err != nil {
			return nil, fmt.Errorf("scan session ref: %w", err)
		}
		ref.URI, ref.ParentItemID = uri.String, parent.String
		out = append(out, ref)
	}
	return out, rows.Err()
}

// Ref attribution values: whether a session's proxy could ever produce a source='proxy' ref (old
// ref_attribution.go and migration 0025).
const (
	// RefAttributionProxy: planted with --session and --extract-refs.
	RefAttributionProxy = "proxy"
	// RefAttributionNone: planted with --session, without --extract-refs.
	RefAttributionNone = "none"
	// RefAttributionUnlaunched: nothing planted (an external caller owns the process).
	RefAttributionUnlaunched = "unlaunched"
	// RefAttributionUnknown is how a NULL column reads; not a synonym for none.
	RefAttributionUnknown = "unknown"
)

// CanProduceProxyRefs reports whether a session with this attribution could ever yield a
// source='proxy' ref. False is a fact about configuration, never about the session's activity.
func CanProduceProxyRefs(attribution string) bool { return attribution == RefAttributionProxy }

// NormalizeRefAttribution maps the stored value (empty for NULL) onto the rendering vocabulary.
func NormalizeRefAttribution(v string) string {
	if v == "" {
		return RefAttributionUnknown
	}
	return v
}

// SetSessionRefAttribution records what was actually planted for a session (the planting site's
// own value, never derived from the session row). ErrSessionNotFound for an unknown session.
func (s *Store) SetSessionRefAttribution(ctx context.Context, sessionID, attribution string) error {
	if sessionID == "" {
		return errors.New("set ref attribution: session id required")
	}
	if attribution == "" {
		return fmt.Errorf("set ref attribution for %q: attribution required (pass %q explicitly rather than clearing the column, which means 'unknown')", sessionID, RefAttributionNone)
	}
	n, err := s.execN(ctx, `UPDATE sessions SET ref_attribution = ?, updated_at = ? WHERE id = ?`, attribution, s.secondStamp(), sessionID)
	if err != nil {
		return fmt.Errorf("set ref attribution for %q: %w", sessionID, err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}
