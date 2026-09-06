package registry

// register_idempotent.go — T02 (messaging vNext, CW-20260906-0034).
//
// Register (service.go) has no idempotency: every call mints a brand-new
// URN, full stop (confirmed by the T02 design research -- see
// planning/docs/messaging-vnext/T01-compatibility-contract.md and the T02
// task notes). The one existing dedup primitive, LookupURNByExternalID +
// AttachExternalID, is wired only as a manual two-step a caller can choose
// to run before/after Register (as internal/registry/bootstrap.go's
// importFile does); it isn't built into Register itself, and the two calls
// aren't atomic with each other.
//
// RegisterIdempotent closes both gaps for exactly the case the architecture
// calls out: "Registration/upsert is authenticated and idempotent against an
// owner-scoped external key, not display-name matching." A caller that wants
// a durable, explicitly-published actor supplies a (substrate, externalID)
// key; RegisterIdempotent looks up and inserts inside ONE transaction, so
// two concurrent callers racing to publish the same durable actor cannot
// both mint a fresh URN -- exactly one wins, the other observes the winner's
// URN. Ordinary one-off session boots that never call RegisterIdempotent are
// completely unaffected: nothing changes about plain Register.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RegisterWithExternalKey is the storage-layer half of idempotent
// publication. It looks up (kind, substrate, externalID) and, only if
// unclaimed, inserts p (which must already carry p.URN pre-minted by the
// caller and p.ExternalIDs including the (substrate, externalID) pair) --
// both steps inside one transaction, so no other caller can observe or
// claim the same key in between.
//
// Returns (existingURN, false, nil) when the key was already claimed by a
// DIFFERENT urn than p.URN -- the caller's freshly minted p.URN is simply
// discarded (URNs are cheap; this only happens under a genuine race).
// Returns (p.URN, true, nil) on a fresh insert.
func (s *Storage) RegisterWithExternalKey(ctx context.Context, p Profile, substrate, externalID string) (urn string, created bool, err error) {
	if p.URN == "" {
		return "", false, errors.New("registry: register with external key: p.URN required (caller mints before calling)")
	}
	if substrate == "" || externalID == "" {
		return "", false, errors.New("registry: register with external key: substrate and external_id required")
	}
	if p.DisplayName == "" {
		return "", false, errors.New("registry: register with external key: display_name required")
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return "", false, fmt.Errorf("registry: register with external key: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var existing string
	lookupErr := tx.QueryRowContext(ctx, `
		SELECT e.urn
		  FROM registry_external_ids x
		  JOIN registry_entries e ON e.urn = x.urn
		 WHERE e.kind = ? AND x.substrate = ? AND x.external_id = ?
		 LIMIT 1`,
		string(p.Kind), substrate, externalID,
	).Scan(&existing)
	switch {
	case lookupErr == nil:
		// Already claimed -- by definition NOT by p.URN, since p.URN was
		// freshly minted by the caller against a pre-insert URNExists check
		// and cannot already own an external-id attachment. Nothing to
		// insert; just report the winner.
		if commitErr := tx.Commit(); commitErr != nil {
			return "", false, fmt.Errorf("registry: register with external key: commit (lookup-only): %w", commitErr)
		}
		return existing, false, nil
	case errors.Is(lookupErr, sql.ErrNoRows):
		// Unclaimed -- proceed to insert below, same transaction.
	default:
		return "", false, fmt.Errorf("registry: register with external key: lookup: %w", lookupErr)
	}

	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = now
	}
	if p.Status == "" {
		p.Status = StatusActive
	}
	if p.MuxInstanceID == "" {
		p.MuxInstanceID = defaultMuxInstanceID
	}
	p = p.WithExternalID(ExternalID{Substrate: substrate, ExternalID: externalID, AttachedAt: now})

	var callbackJSON sql.NullString
	if p.Callback != nil {
		b, mErr := json.Marshal(p.Callback)
		if mErr != nil {
			return "", false, fmt.Errorf("registry: register with external key: marshal callback: %w", mErr)
		}
		callbackJSON = sql.NullString{String: string(b), Valid: true}
	}
	var kindMetaJSON sql.NullString
	if len(p.KindMeta) > 0 {
		kindMetaJSON = sql.NullString{String: string(p.KindMeta), Valid: true}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO registry_entries
		    (urn, kind, mux_instance_id, display_name, title, role, description,
		     avatar, project, status, callback_json, cached_at, health_status,
		     last_seen_at, host_address, merged_into, kind_meta_json, last_updated_by,
		     created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.URN, string(p.Kind), p.MuxInstanceID, p.DisplayName,
		nullIfEmpty(p.Title), nullIfEmpty(p.Role), nullIfEmpty(p.Description),
		nullIfEmpty(p.Avatar), nullIfEmpty(p.Project), string(p.Status),
		callbackJSON, nullIfTimePtr(p.CachedAt), nullIfEmpty(p.HealthStatus),
		nullIfTimePtr(p.LastSeenAt), nullIfEmpty(p.HostAddress), nullIfEmpty(p.MergedInto),
		kindMetaJSON, nullIfEmpty(p.LastUpdatedBy),
		formatTime(p.CreatedAt), formatTime(p.UpdatedAt),
	); err != nil {
		return "", false, fmt.Errorf("registry: register with external key: insert entry: %w", err)
	}
	for _, ext := range p.ExternalIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_external_ids (urn, substrate, external_id, attached_at)
			 VALUES (?, ?, ?, ?)`,
			p.URN, ext.Substrate, ext.ExternalID, formatTime(ext.AttachedAt)); err != nil {
			return "", false, fmt.Errorf("registry: register with external key: insert external_id (%s,%s): %w", ext.Substrate, ext.ExternalID, err)
		}
	}
	for _, c := range p.Capabilities {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_capabilities (urn, capability) VALUES (?, ?)`,
			p.URN, c); err != nil {
			return "", false, fmt.Errorf("registry: register with external key: insert capability %q: %w", c, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("registry: register with external key: commit: %w", err)
	}
	return p.URN, true, nil
}

// RegisterIdempotent is the Service-level entry point: an authenticated
// caller (in the current same-host-UDS-trust model, "authenticated" means
// the caller is trusted to assert its own substrate/external-id pair --
// T05 owns adding real principal binding across every transport) publishes
// a durable participant keyed by (substrate, externalID). Calling this
// twice with the same key returns the same URN both times; it never
// creates a duplicate row and never mutates an existing row's fields
// (repeat callers wanting field updates use UpdateSelf, matching D5).
//
// kind must be KindAgent or KindProject -- groups have their own
// creator-owned Register path (registerGroup) and are out of scope here.
func (s *Service) RegisterIdempotent(ctx context.Context, kind Kind, p Profile, substrate, externalID string) (out Profile, created bool, err error) {
	switch kind {
	case KindAgent, KindProject:
	default:
		return Profile{}, false, fmt.Errorf("registry: register idempotent: %w: unsupported kind %q (agent/project only)", ErrInvalidRequest, string(kind))
	}
	if p.URN != "" {
		return Profile{}, false, fmt.Errorf("registry: register idempotent: %w: caller-supplied URN not allowed; server mints", ErrInvalidRequest)
	}
	if p.DisplayName == "" {
		return Profile{}, false, fmt.Errorf("registry: register idempotent: %w: display_name is required", ErrInvalidRequest)
	}
	if substrate == "" || externalID == "" {
		return Profile{}, false, fmt.Errorf("registry: register idempotent: %w: substrate and external_id are required", ErrInvalidRequest)
	}

	// Cheap pre-check outside the transaction: if the key is already
	// claimed, skip minting a URN we're just going to discard. This is a
	// pure optimization -- RegisterWithExternalKey still re-checks inside
	// its own transaction, so a race after this pre-check is still handled
	// correctly, just at the cost of one wasted mint (36^10 keyspace, not
	// a meaningful cost).
	if existing, ok, lookupErr := s.storage.LookupURNByExternalID(ctx, kind, externalID, substrate); lookupErr != nil {
		return Profile{}, false, fmt.Errorf("registry: register idempotent: pre-check: %w", lookupErr)
	} else if ok {
		out, getErr := s.storage.GetProfile(ctx, existing)
		if getErr != nil {
			return Profile{}, false, fmt.Errorf("registry: register idempotent: reload existing: %w", getErr)
		}
		return out, false, nil
	}

	var (
		minted string
		mErr   error
	)
	switch kind {
	case KindAgent:
		minted, mErr = MintAgentURN(ctx, s.storage.URNExists)
	case KindProject:
		minted, mErr = MintProjectURN(ctx, s.storage.URNExists)
	case KindGroup:
		// Unreachable — rejected by the kind check above. Listed here so
		// the exhaustive linter is satisfied (mirrors Register's own
		// equivalent switch in service.go).
		return Profile{}, false, fmt.Errorf("registry: register idempotent: %w: unreachable group path", ErrInvalidRequest)
	}
	if mErr != nil {
		return Profile{}, false, mErr
	}
	p.URN = minted
	p.Kind = kind
	if p.LastUpdatedBy == "" {
		p.LastUpdatedBy = "system:register"
	}

	winnerURN, wasCreated, err := s.storage.RegisterWithExternalKey(ctx, p, substrate, externalID)
	if err != nil {
		return Profile{}, false, fmt.Errorf("registry: register idempotent: %w", err)
	}
	out, err = s.storage.GetProfile(ctx, winnerURN)
	if err != nil {
		return Profile{}, false, fmt.Errorf("registry: register idempotent: reload: %w", err)
	}
	return out, wasCreated, nil
}
