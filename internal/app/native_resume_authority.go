package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

type nativeCredentialCeiling struct {
	scopes  []string
	expires *time.Time
}

// Historical execution metadata constrains a new credential; only the current
// verified caller authorizes it. A revoked old token is never reactivated.
func (s *Service) nativeOnlyCredentialCeiling(ctx context.Context, plan *launch.Plan) (nativeCredentialCeiling, error) {
	var floor nativeCredentialCeiling
	mode := string(identity.Observe)
	if s.Catalog != nil {
		mode = s.Catalog.Global.Identity.EffectiveMode()
	}
	if mode == string(identity.Off) {
		return floor, nil
	}
	caller, ok := identity.FromContext(ctx)
	now := time.Now()
	if !ok || caller.ID == "" || caller.RevokedAt != nil || caller.SessionID == plan.ResumeSourceSessionID || caller.ExpiresAt != nil && !caller.ExpiresAt.After(now) || !slices.Contains(caller.Scopes, "*") && !slices.Contains(caller.Scopes, "session.write") {
		return floor, nativeOnlyError("independent current launch authority required")
	}
	var raw string
	var expires sql.NullString
	err := s.Store.DB().QueryRowContext(ctx, `SELECT scopes_json,expires_at FROM principals WHERE principal_id=? AND kind='session' AND session_id=? ORDER BY id DESC LIMIT 1`, "msg://session/local/"+plan.ResumeSourceSessionID, plan.ResumeSourceSessionID).Scan(&raw, &expires)
	if err != nil || json.Unmarshal([]byte(raw), &floor.scopes) != nil || floor.scopes == nil {
		return floor, nativeOnlyError("original credential ceiling unavailable")
	}
	if expires.Valid {
		at, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil || !at.After(now) {
			return floor, nativeOnlyError("original credential ceiling expired")
		}
		floor.expires = &at
	}
	if caller.ExpiresAt != nil && (floor.expires == nil || caller.ExpiresAt.Before(*floor.expires)) {
		floor.expires = caller.ExpiresAt
	}
	return floor, nil
}

func (s *Service) nativeOnlyPolicy(ctx context.Context, plan *launch.Plan, current mcpgateway.SessionPolicy) (mcpgateway.SessionPolicy, error) {
	var raw string
	var agent string
	err := s.Store.DB().QueryRowContext(ctx, `SELECT p.policy_json,COALESCE(s.logical_agent_id,'') FROM session_mcp_policy p JOIN sessions s ON s.id=p.session_id WHERE p.session_id=?`, plan.ResumeSourceSessionID).Scan(&raw, &agent)
	var source mcpgateway.SessionPolicy
	if err != nil || json.Unmarshal([]byte(raw), &source) != nil || source.Validate() != nil || source.SessionID != plan.ResumeSourceSessionID || source.AgentID != agent || source.AgentID != current.AgentID {
		return current, nativeOnlyError("sealed source MCP floor unavailable")
	}
	source.SessionID = current.SessionID
	source = source.Seal()
	if source.Digest != current.Seal().Digest {
		return current, nativeOnlyError("current MCP policy conflicts with sealed source floor")
	}
	return source, nil
}

func (s *Service) nativeOnlyLaunchAuthority(ctx context.Context, plan *launch.Plan) error {
	if _, err := s.nativeOnlyCredentialCeiling(ctx, plan); err != nil {
		return err
	}
	var cfg mcpgateway.Config
	if s.Catalog != nil {
		cfg = s.Catalog.Global.MCP
	}
	policy, err := sessionMCPPolicy(plan.ResumeSourceSessionID, plan.LogicalAgentID, plan, cfg)
	if err != nil {
		return nativeOnlyError("current MCP policy unavailable")
	}
	ownership, err := s.mcpOwnership()
	if err != nil {
		return nativeOnlyError("current MCP ownership unavailable")
	}
	policy.UpstreamOwnership = ownership
	policy.ExtractRefs = plan.ExtractRefs
	if !policy.ExtractRefs && s.Catalog != nil {
		policy.ExtractRefs = config.EffectiveExtractRefs(s.Catalog.Global, s.Catalog.Projects[plan.ProjectID], s.Catalog.Launches[plan.LaunchID])
	}
	_, err = s.nativeOnlyPolicy(ctx, plan, policy.Seal())
	return err
}
