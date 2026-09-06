package registry

// bindings.go — leased live runtime bindings (T02, messaging vNext,
// CW-20260906-0034). Fully greenfield: no lease/binding/generation/fencing
// primitive existed anywhere in Tether or in the agentkit version it
// currently depends on before this file (confirmed by the T02 design
// research).
//
// A RuntimeBinding answers "which concrete host/attempt currently owns
// delivery for this target right now" for an arbitrary msg:// target URN --
// either an exact-session address (msg://session/<authority>/<id>) or a
// durable actor's registry URN (msg://agent/<authority>/<id>). Reusing the
// shared msg:// address space (T01 finding: registry and messaging URNs
// already coincide) means one binding table serves both the "pinned exact
// session" and "stable actor that can move across sessions" cases from the
// architecture doc without needing to know in advance which kind a target
// is.
//
// generation is a monotonic counter scoped per target_urn: LeaseBinding
// always mints generation = (current max for this target) + 1. Only the
// highest non-revoked, non-expired-by-lease generation is ever "current"
// (CurrentBinding) -- this is the "one active home-host binding per
// concrete session, with generation fencing during replacement/recovery"
// requirement. A caller holding a stale generation can still call
// RenewLease/RevokeBinding on their own binding row (by binding id), but
// RenewLease fails once a newer generation exists for the same target --
// that's the fencing.
//
// Concurrency: LeaseBinding runs its generation read + insert inside one
// transaction (the codebase's existing precedent for a monotonic per-key
// counter -- see ADR-0042's group_seq -- but done atomically here rather
// than relying solely on connection-pool serialization, since binding
// leases are a security/correctness-sensitive fencing primitive, not a
// display ordering counter).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RuntimeBinding is one leased host/attempt attempt against a target URN.
type RuntimeBinding struct {
	ID             string
	TargetURN      string
	SessionID      string
	HostID         string
	AttemptID      string
	Generation     int64
	Capabilities   []string
	Visibility     PublicationVisibility
	LeasedAt       time.Time
	LeaseExpiresAt *time.Time
	RevokedAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PublicationVisibility mirrors agentkit's PublicationChoice vocabulary
// without requiring agentkit as a dependency of this package (go-messaging's
// own dependency-graph rule -- CONTRACTS.md -- applies by extension here:
// keep the registry's own leasing primitive neutral).
type PublicationVisibility string

const (
	VisibilityPrivateLocal   PublicationVisibility = "private-local"
	VisibilityPublishedLocal PublicationVisibility = "published-local"
	VisibilityTetherHosted   PublicationVisibility = "tether-hosted"
)

func (v PublicationVisibility) orDefault() PublicationVisibility {
	if v == "" {
		return VisibilityPrivateLocal
	}
	return v
}

func (v PublicationVisibility) valid() bool {
	switch v.orDefault() {
	case VisibilityPrivateLocal, VisibilityPublishedLocal, VisibilityTetherHosted:
		return true
	default:
		return false
	}
}

// ErrStaleGeneration is returned by RenewLease/RevokeBinding when a newer
// generation already exists for the same target_urn -- the caller's binding
// has been fenced out by a replacement.
var ErrStaleGeneration = errors.New("registry: binding generation superseded")

// ErrBindingNotFound is returned when a binding id doesn't exist.
var ErrBindingNotFound = errors.New("registry: binding not found")

// LeaseBinding mints a new binding for targetURN at generation
// (current max + 1), inside one transaction so two concurrent leasers for
// the same target cannot both win the same generation number. ttl <= 0
// means no expiry (the binding is current until explicitly revoked or
// superseded by a newer generation).
func (s *Storage) LeaseBinding(ctx context.Context, targetURN, sessionID, hostID, attemptID string, capabilities []string, visibility PublicationVisibility, ttl time.Duration) (RuntimeBinding, error) {
	if targetURN == "" || sessionID == "" || hostID == "" || attemptID == "" {
		return RuntimeBinding{}, fmt.Errorf("registry: lease binding: target_urn, session_id, host_id and attempt_id are required")
	}
	visibility = visibility.orDefault()
	if !visibility.valid() {
		return RuntimeBinding{}, fmt.Errorf("registry: lease binding: invalid visibility %q", visibility)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: lease binding: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var maxGen sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(generation) FROM runtime_bindings WHERE target_urn = ?`, targetURN,
	).Scan(&maxGen); err != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: lease binding: read max generation: %w", err)
	}
	next := int64(1)
	if maxGen.Valid {
		next = maxGen.Int64 + 1
	}

	now := time.Now().UTC()
	b := RuntimeBinding{
		ID:           uuid.NewString(),
		TargetURN:    targetURN,
		SessionID:    sessionID,
		HostID:       hostID,
		AttemptID:    attemptID,
		Generation:   next,
		Capabilities: capabilities,
		Visibility:   visibility,
		LeasedAt:     now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		b.LeaseExpiresAt = &exp
	}

	var capsJSON sql.NullString
	if len(capabilities) > 0 {
		cb, mErr := json.Marshal(capabilities)
		if mErr != nil {
			return RuntimeBinding{}, fmt.Errorf("registry: lease binding: marshal capabilities: %w", mErr)
		}
		capsJSON = sql.NullString{String: string(cb), Valid: true}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runtime_bindings
		    (id, target_urn, session_id, host_id, attempt_id, generation, capabilities_json,
		     visibility, leased_at, lease_expires_at, revoked_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		b.ID, b.TargetURN, b.SessionID, b.HostID, b.AttemptID, b.Generation, capsJSON,
		string(b.Visibility), formatTime(b.LeasedAt), nullIfTimePtr(b.LeaseExpiresAt),
		formatTime(b.CreatedAt), formatTime(b.UpdatedAt),
	); err != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: lease binding: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: lease binding: commit: %w", err)
	}
	return b, nil
}

// RenewLease extends bindingID's lease to now+ttl. Fails with
// ErrStaleGeneration if a newer generation exists for the same target_urn,
// and with ErrBindingNotFound if bindingID doesn't exist. A revoked binding
// cannot be renewed (also ErrStaleGeneration -- revocation is a permanent
// fence, not a lease-expiry that renewal can undo).
func (s *Storage) RenewLease(ctx context.Context, bindingID string, ttl time.Duration) (RuntimeBinding, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: renew lease: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b, err := scanBindingTx(ctx, tx, `SELECT `+bindingColumns+` FROM runtime_bindings WHERE id = ?`, bindingID)
	if err != nil {
		return RuntimeBinding{}, err
	}
	if b.RevokedAt != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: renew lease: %w: binding %s is revoked", ErrStaleGeneration, bindingID)
	}
	var maxGen int64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(generation) FROM runtime_bindings WHERE target_urn = ?`, b.TargetURN).Scan(&maxGen); err != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: renew lease: read max generation: %w", err)
	}
	if maxGen > b.Generation {
		return RuntimeBinding{}, fmt.Errorf("registry: renew lease: %w: binding %s is generation %d, current is %d", ErrStaleGeneration, bindingID, b.Generation, maxGen)
	}

	now := time.Now().UTC()
	var newExpiry *time.Time
	if ttl > 0 {
		exp := now.Add(ttl)
		newExpiry = &exp
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE runtime_bindings SET lease_expires_at = ?, updated_at = ? WHERE id = ?`,
		nullIfTimePtr(newExpiry), formatTime(now), bindingID,
	); err != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: renew lease: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RuntimeBinding{}, fmt.Errorf("registry: renew lease: commit: %w", err)
	}
	b.LeaseExpiresAt = newExpiry
	b.UpdatedAt = now
	return b, nil
}

// RevokeBinding marks bindingID revoked. Idempotent: revoking an
// already-revoked binding is a no-op success. Explicit revocation always
// succeeds regardless of generation -- an owner (or an authorized operator)
// can always relinquish its own lease; ErrStaleGeneration is specifically
// about renewal, not revocation.
func (s *Storage) RevokeBinding(ctx context.Context, bindingID string) error {
	now := formatTime(time.Now().UTC())
	res, err := s.db.ExecContext(ctx,
		`UPDATE runtime_bindings SET revoked_at = COALESCE(revoked_at, ?), updated_at = ? WHERE id = ?`,
		now, now, bindingID)
	if err != nil {
		return fmt.Errorf("registry: revoke binding: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("registry: revoke binding: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrBindingNotFound, bindingID)
	}
	return nil
}

// CurrentBinding returns the highest-generation, non-revoked binding for
// targetURN whose lease has not expired (lease_expires_at IS NULL means no
// expiry). Returns ErrBindingNotFound if no such binding exists -- either
// nothing was ever leased, the only bindings are revoked, or the lease(s)
// expired.
func (s *Storage) CurrentBinding(ctx context.Context, targetURN string) (RuntimeBinding, error) {
	nowStr := formatTime(time.Now().UTC())
	b, err := scanBinding(ctx, s.db,
		`SELECT `+bindingColumns+` FROM runtime_bindings
		 WHERE target_urn = ? AND revoked_at IS NULL
		   AND (lease_expires_at IS NULL OR lease_expires_at > ?)
		 ORDER BY generation DESC LIMIT 1`,
		targetURN, nowStr)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeBinding{}, fmt.Errorf("%w: target=%s", ErrBindingNotFound, targetURN)
	}
	return b, err
}

// ListBindingsForTarget returns every binding ever leased for targetURN,
// newest generation first -- an audit/debugging view, not the hot path.
func (s *Storage) ListBindingsForTarget(ctx context.Context, targetURN string) ([]RuntimeBinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+bindingColumns+` FROM runtime_bindings WHERE target_urn = ? ORDER BY generation DESC`, targetURN)
	if err != nil {
		return nil, fmt.Errorf("registry: list bindings: %w", err)
	}
	defer rows.Close()
	out := []RuntimeBinding{}
	for rows.Next() {
		b, err := scanBindingRow(rows)
		if err != nil {
			return nil, fmt.Errorf("registry: list bindings: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

const bindingColumns = `id, target_urn, session_id, host_id, attempt_id, generation, capabilities_json, visibility, leased_at, lease_expires_at, revoked_at, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanBindingFrom(sc rowScanner) (RuntimeBinding, error) {
	var (
		b                              RuntimeBinding
		capsJSON                       sql.NullString
		visibility                     string
		leasedAt, createdAt, updatedAt string
		leaseExpiresAt, revokedAt      sql.NullString
	)
	if err := sc.Scan(&b.ID, &b.TargetURN, &b.SessionID, &b.HostID, &b.AttemptID, &b.Generation,
		&capsJSON, &visibility, &leasedAt, &leaseExpiresAt, &revokedAt, &createdAt, &updatedAt); err != nil {
		return RuntimeBinding{}, err
	}
	b.Visibility = PublicationVisibility(visibility)
	b.LeasedAt = parseTime(leasedAt)
	b.CreatedAt = parseTime(createdAt)
	b.UpdatedAt = parseTime(updatedAt)
	if leaseExpiresAt.Valid {
		t := parseTime(leaseExpiresAt.String)
		b.LeaseExpiresAt = &t
	}
	if revokedAt.Valid {
		t := parseTime(revokedAt.String)
		b.RevokedAt = &t
	}
	if capsJSON.Valid {
		if err := json.Unmarshal([]byte(capsJSON.String), &b.Capabilities); err != nil {
			return RuntimeBinding{}, fmt.Errorf("unmarshal capabilities: %w", err)
		}
	}
	return b, nil
}

func scanBinding(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, query string, args ...any) (RuntimeBinding, error) {
	return scanBindingFrom(q.QueryRowContext(ctx, query, args...))
}

func scanBindingTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (RuntimeBinding, error) {
	b, err := scanBindingFrom(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeBinding{}, fmt.Errorf("%w: %v", ErrBindingNotFound, args)
	}
	return b, err
}

func scanBindingRow(rows *sql.Rows) (RuntimeBinding, error) {
	return scanBindingFrom(rows)
}
