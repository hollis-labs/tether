package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/tether/internal/mcpgateway"
)

var ErrSessionMCPPolicyUnavailable = errors.New("session MCP policy unavailable; resume or relaunch the session")

// SaveSessionMCPPolicy records immutable effective authority before the session
// credential is delivered. A preparation retry may repeat the SAME snapshot;
// it cannot silently replace a snapshot already bound to a credential.
func (s *Store) SaveSessionMCPPolicy(ctx context.Context, policy mcpgateway.SessionPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO session_mcp_policy(session_id,policy_json)
 SELECT id,? FROM sessions WHERE id=? AND COALESCE(logical_agent_id,'')=? AND state NOT IN ('completed','failed','killed')
 ON CONFLICT(session_id) DO NOTHING`, string(raw), policy.SessionID, policy.AgentID)
	if err != nil {
		return fmt.Errorf("persist session MCP policy: %w", err)
	}
	stored, err := s.SessionMCPPolicy(ctx, policy.SessionID)
	if err != nil {
		return err
	}
	if stored.Digest != policy.Digest {
		return fmt.Errorf("session MCP policy already bound; resume or relaunch the session")
	}
	return nil
}

// SessionMCPPolicy refuses terminal/missing/legacy sessions even when their
// retained policy row still exists for audit. Credential verification also
// refuses the revoked session token; neither check substitutes for the other.
func (s *Store) SessionMCPPolicy(ctx context.Context, id string) (mcpgateway.SessionPolicy, error) {
	var raw string
	var agent string
	err := s.db.QueryRowContext(ctx, `SELECT p.policy_json, COALESCE(s.logical_agent_id,'') FROM session_mcp_policy p JOIN sessions s ON s.id=p.session_id
 WHERE p.session_id=? AND s.state NOT IN ('completed','failed','killed')`, id).Scan(&raw, &agent)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpgateway.SessionPolicy{}, ErrSessionMCPPolicyUnavailable
	}
	if err != nil {
		return mcpgateway.SessionPolicy{}, err
	}
	var policy mcpgateway.SessionPolicy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return policy, fmt.Errorf("invalid stored session MCP policy")
	}
	if err := policy.Validate(); err != nil {
		return policy, err
	}
	if policy.SessionID != id || policy.AgentID != agent {
		return policy, fmt.Errorf("stored session MCP policy identity mismatch")
	}
	return policy, nil
}
