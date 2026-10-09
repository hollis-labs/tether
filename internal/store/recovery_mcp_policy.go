package store

import (
	"context"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

// RecoverySessionMCPPolicy is a trusted host read, not token verification.
// Retained snapshots alone do not authorize reads: an unrevoked session
// principal and the highest unrevoked binding must still exist. Terminal,
// missing, superseded or revoked authority produces an explicit omission.
func (s *Store) RecoverySessionMCPPolicy(ctx context.Context, id string) (mcpgateway.SessionPolicy, error) {
	policy, err := s.SessionMCPPolicy(ctx, id)
	if err != nil {
		return policy, err
	}
	var active bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM principals WHERE kind='session' AND session_id=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>strftime('%Y-%m-%dT%H:%M:%fZ','now')))
 AND EXISTS(SELECT 1 FROM runtime_bindings b WHERE b.session_id=? AND b.revoked_at IS NULL AND (b.lease_expires_at IS NULL OR b.lease_expires_at>strftime('%Y-%m-%dT%H:%M:%fZ','now')) AND b.generation=(SELECT MAX(generation) FROM runtime_bindings WHERE target_urn=b.target_urn))`, id, id).Scan(&active)
	if err != nil {
		return mcpgateway.SessionPolicy{}, err
	}
	if !active {
		return mcpgateway.SessionPolicy{}, ErrSessionMCPPolicyUnavailable
	}
	return policy, nil
}
