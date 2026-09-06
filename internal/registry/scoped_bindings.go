package registry

// scoped_bindings.go — T04 (messaging vNext, CW-20260906-0035). The
// architecture's "narrow scoped-address binding primitive": a consumer-owned
// scope plus role/slot name maps to participant URNs -- e.g. scope
// "torque:sprint:CW-20260906-0023" slot "reviewer" resolves to one or more
// registry URNs. Deliberately distinct from:
//
//   - ADR-0042's group owner/moderator/member roles (a group-permission
//     concept, not a resolution mapping).
//   - registry_links (ADR-0042 already ruled this shape out for anything
//     carrying per-member state; a scoped binding carries revision history,
//     which is exactly that kind of state).
//
// Pure resolution/mapping only -- no workflow, staffing, activation, or
// command authority. "Saved teams, staffing counts, durable-versus-fresh
// activation, phases, approval gates, semantic routing... remain
// consumer-owned" (architecture doc); this primitive answers "who currently
// holds slot X in scope Y", nothing more.
//
// revision is a monotonic counter per (scope, slot), minted the same way
// bindings.go mints a runtime-binding generation: read current max inside a
// transaction, insert next+1, commit -- so concurrent binders for the same
// (scope, slot) can never collide on a revision number, and every prior
// revision remains queryable history (rows are never updated or deleted).
// This is what gives binding resolution its required provenance ("explains
// scope/revision/targets") and lets an already-accepted fanout delivery's
// frozen recipient set survive a LATER rebinding untouched -- the delivery
// core's own frozen RecipientTarget snapshot (see group_fanout.go) never
// re-reads this table after the fact, so a rebind can never retroactively
// redirect an in-flight or historical delivery.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ScopedBinding is one revision of a (scope, slot) resolution.
type ScopedBinding struct {
	ID           string
	Scope        string
	Slot         string
	Revision     int64
	TargetURNs   []string
	Relationship json.RawMessage
	CreatedBy    string
	CreatedAt    time.Time
}

// ErrAmbiguousBinding is returned by ResolveScopedBindingSingle when the
// current revision names more than one target and the caller did not
// request fanout.
var ErrAmbiguousBinding = errors.New("registry: scoped binding resolves to more than one target")

// ErrBindingHasNoTargets is returned by ResolveScopedBindingSingle when the
// current revision names zero targets.
var ErrBindingHasNoTargets = errors.New("registry: scoped binding has no targets")

// SetScopedBinding mints the next revision for (scope, slot) and records
// targetURNs -- the current holders of that slot -- plus optional
// relationship metadata (e.g. {"reports_to": "msg://agent/..."}) and the
// caller's own URN for provenance. targetURNs order is preserved as given
// (callers that care about a canonical single target should pass exactly
// one). Minting the next revision happens inside one transaction so two
// concurrent SetScopedBinding calls for the same (scope, slot) can never
// both claim the same revision number.
func (s *Storage) SetScopedBinding(ctx context.Context, scope, slot string, targetURNs []string, relationship json.RawMessage, createdBy string) (ScopedBinding, error) {
	if scope == "" || slot == "" {
		return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: scope and slot are required")
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var maxRev sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(revision) FROM scoped_role_bindings WHERE scope = ? AND slot = ?`, scope, slot,
	).Scan(&maxRev); err != nil {
		return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: read max revision: %w", err)
	}
	next := int64(1)
	if maxRev.Valid {
		next = maxRev.Int64 + 1
	}

	targetsJSON, err := json.Marshal(targetURNs)
	if err != nil {
		return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: marshal targets: %w", err)
	}

	b := ScopedBinding{
		ID:           uuid.NewString(),
		Scope:        scope,
		Slot:         slot,
		Revision:     next,
		TargetURNs:   targetURNs,
		Relationship: relationship,
		CreatedBy:    createdBy,
		CreatedAt:    time.Now().UTC(),
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO scoped_role_bindings (id, scope, slot, revision, target_urns_json, relationship_json, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.Scope, b.Slot, b.Revision, string(targetsJSON), nullIfEmptyJSON(relationship), nullIfEmpty(createdBy), formatTime(b.CreatedAt),
	); err != nil {
		return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: commit: %w", err)
	}
	return b, nil
}

// ResolveScopedBinding returns the CURRENT (highest-revision) binding for
// (scope, slot). Returns ErrNotFound if no binding was ever set.
func (s *Storage) ResolveScopedBinding(ctx context.Context, scope, slot string) (ScopedBinding, error) {
	return scanScopedBinding(ctx, s.db,
		`SELECT id, scope, slot, revision, target_urns_json, relationship_json, created_by, created_at
		 FROM scoped_role_bindings WHERE scope = ? AND slot = ? ORDER BY revision DESC LIMIT 1`,
		scope, slot)
}

// ResolveScopedBindingSingle resolves (scope, slot) to exactly one target
// URN. Returns ErrBindingHasNoTargets for zero targets, ErrAmbiguousBinding
// for more than one (the architecture's "a single-recipient alias requires
// one valid target or an explicit consumer selection policy; multiple
// targets are an error unless fanout is requested" -- fanout callers use
// ResolveScopedBinding directly and iterate TargetURNs themselves instead).
func (s *Storage) ResolveScopedBindingSingle(ctx context.Context, scope, slot string) (string, ScopedBinding, error) {
	b, err := s.ResolveScopedBinding(ctx, scope, slot)
	if err != nil {
		return "", ScopedBinding{}, err
	}
	switch len(b.TargetURNs) {
	case 0:
		return "", b, fmt.Errorf("%w: scope=%q slot=%q revision=%d", ErrBindingHasNoTargets, scope, slot, b.Revision)
	case 1:
		return b.TargetURNs[0], b, nil
	default:
		return "", b, fmt.Errorf("%w: scope=%q slot=%q revision=%d targets=%v", ErrAmbiguousBinding, scope, slot, b.Revision, b.TargetURNs)
	}
}

// ListScopedBindingRevisions returns every revision ever set for (scope,
// slot), newest first -- the audit/provenance view.
func (s *Storage) ListScopedBindingRevisions(ctx context.Context, scope, slot string) ([]ScopedBinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, slot, revision, target_urns_json, relationship_json, created_by, created_at
		 FROM scoped_role_bindings WHERE scope = ? AND slot = ? ORDER BY revision DESC`,
		scope, slot)
	if err != nil {
		return nil, fmt.Errorf("registry: list scoped binding revisions: %w", err)
	}
	defer rows.Close()
	out := []ScopedBinding{}
	for rows.Next() {
		b, err := scanScopedBindingRow(rows)
		if err != nil {
			return nil, fmt.Errorf("registry: list scoped binding revisions: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func nullIfEmptyJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func scanScopedBindingFrom(sc interface{ Scan(dest ...any) error }) (ScopedBinding, error) {
	var (
		b                    ScopedBinding
		targetsJSON          string
		relationship         sql.NullString
		createdBy, createdAt string
	)
	if err := sc.Scan(&b.ID, &b.Scope, &b.Slot, &b.Revision, &targetsJSON, &relationship, &createdBy, &createdAt); err != nil {
		return ScopedBinding{}, err
	}
	if err := json.Unmarshal([]byte(targetsJSON), &b.TargetURNs); err != nil {
		return ScopedBinding{}, fmt.Errorf("unmarshal target_urns_json: %w", err)
	}
	if relationship.Valid {
		b.Relationship = json.RawMessage(relationship.String)
	}
	b.CreatedBy = createdBy
	b.CreatedAt = parseTime(createdAt)
	return b, nil
}

func scanScopedBinding(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, query string, args ...any) (ScopedBinding, error) {
	b, err := scanScopedBindingFrom(q.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return ScopedBinding{}, fmt.Errorf("%w: %v", ErrNotFound, args)
	}
	return b, err
}

func scanScopedBindingRow(rows *sql.Rows) (ScopedBinding, error) {
	return scanScopedBindingFrom(rows)
}
