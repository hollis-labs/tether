package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// The worker owns filesystem evidence. The hub never treats its mirror workspace
// or a guessed native id as that evidence. Probe changes no execution state.
func (s *Service) workerNativeSource(ctx context.Context, source *store.SessionRow, p *launch.Plan) error {
	if source == nil || !session.State(source.State).Terminal() || source.ProviderID != p.ProviderID || p.ProviderBrand != "codex" || p.RuntimeKind != "jsonrpc-stdio" {
		return nativeOnlyError("terminal same-provider Codex app-server source required")
	}
	mapping, mapErr := s.Store.GetSessionProviderMapping(source.ID, "tether", source.ProviderID)
	if mapErr == nil && (!mapping.NativeSessionID.Valid || mapping.NativeSessionID.String == "") {
		return nativeOnlyError("source native mapping was retired")
	}
	if mapErr != nil && !errors.Is(mapErr, store.ErrProviderMappingNotFound) {
		return mapErr
	}
	prior, err := s.Store.GetLaunchPlan(source.ID)
	if err != nil || prior.TeamMember || prior.IdentityURN != p.IdentityURN || prior.LogicalAgentID != p.LogicalAgentID {
		return nativeOnlyError("source identity changed")
	}
	if err := s.checkWorkerResume(source, api.ResumeOptions{NativeOnly: true}); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	f, err := s.Workers.Send(wait, source.WorkerID, workerwire.Frame{T: "resume_probe", Session: source.ID, CmdID: uuid.NewString()})
	if err != nil {
		return nativeOnlyError("worker source probe unavailable: " + err.Error())
	}
	n := f.NativeContext
	if n == nil || n.Session != source.ID || n.Actor != registry.LogicalAgentBindingTarget(p.LogicalAgentID) || n.Thread == "" || n.Workroot == "" || n.Generation == "" {
		return nativeOnlyError("invalid worker native source receipt")
	}
	if n.Generation != p.WorkerSourceGeneration {
		return nativeOnlyError(fmt.Sprintf("worker source generation changed or missing; observed %s", n.Generation))
	}
	if p.ResumeProviderSessionID != "" && p.ResumeProviderSessionID != n.Thread {
		return nativeOnlyError("native source mapping conflicts with worker receipt")
	}
	p.WorkRoot, p.ResumeProviderSessionID = n.Workroot, n.Thread
	return nil
}

// A new snapshot is not an edit of the old one. Explicit digest approval is
// restricted to a current catalog writer; session.write alone cannot add origins.
// The digest uses the source id, so it can be reviewed before allocating a child.
func (s *Service) workerNativePolicy(ctx context.Context, p *launch.Plan, current mcpgateway.SessionPolicy) error {
	caller, ok := identity.FromContext(ctx)
	if !ok || !caller.Verified || caller.ID == "" || caller.SessionID == p.ResumeSourceSessionID || (!slices.Contains(caller.Scopes, "*") && !slices.Contains(caller.Scopes, "catalog.write")) {
		return nativeOnlyError("independent current catalog authority required for worker policy")
	}
	// Fence the exact credential, not another rotated credential with the same id.
	var expiry sql.NullString
	var scopesJSON string
	err := s.Store.DB().QueryRowContext(ctx, `SELECT scopes_json,expires_at FROM principals WHERE id=? AND principal_id=? AND revoked_at IS NULL`, caller.CredentialID, caller.ID).Scan(&scopesJSON, &expiry)
	var scopes []string
	if err != nil || json.Unmarshal([]byte(scopesJSON), &scopes) != nil || !slices.Equal(scopes, caller.Scopes) {
		return nativeOnlyError("worker continuation authority is stale or unbacked")
	}
	if expiry.Valid {
		at, err := time.Parse(time.RFC3339Nano, expiry.String)
		if err != nil || !at.After(time.Now()) {
			return nativeOnlyError("worker continuation authority expired")
		}
	}

	if _, err := s.nativeOnlyCredentialCeiling(ctx, p); err != nil {
		return err
	}
	// Verify the old immutable record even when an authorized new policy differs.
	var raw string
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT policy_json FROM session_mcp_policy WHERE session_id=?`, p.ResumeSourceSessionID).Scan(&raw); err != nil {
		return nativeOnlyError("sealed source policy unavailable")
	}
	var old mcpgateway.SessionPolicy
	if err := decodeWorkerSourcePolicy(raw, &old); err != nil || old.SessionID != p.ResumeSourceSessionID || old.AgentID != p.LogicalAgentID {
		return nativeOnlyError("sealed source policy invalid")
	}
	current.SessionID = p.ResumeSourceSessionID
	digest := current.Seal().Digest
	if p.WorkerPolicyDigest != digest {
		return nativeOnlyError("explicit current worker policy digest required: " + digest)
	}
	return nil
}

func (s *Service) workerNativeAuthority(ctx context.Context, p *launch.Plan) error {
	// Re-resolve at admission: a persisted prepared plan is not today's grant.
	cat, err := s.launchCatalog(p.LaunchID)
	if err != nil {
		return err
	}
	fresh, err := launch.Resolve(cat, launch.Input{LaunchID: p.LaunchID, CatalogRoot: s.CatalogRoot})
	if err != nil {
		return err
	}
	s.applyCatalogIdentity(fresh)
	if fresh.ProviderID != p.ProviderID || fresh.ProviderBrand != p.ProviderBrand || fresh.RuntimeKind != p.RuntimeKind || fresh.IdentityURN != p.IdentityURN || !slices.Equal(fresh.MCPScopes, p.MCPScopes) {
		return nativeOnlyError("current catalog seat authority changed")
	}
	var cfg mcpgateway.Config
	if s.cat() != nil {
		cfg = s.cat().Global.MCP
	}
	policy, err := sessionMCPPolicy(p.ResumeSourceSessionID, p.LogicalAgentID, fresh, cfg)
	if err != nil {
		return err
	}
	policy.UpstreamOwnership, err = s.mcpOwnership()
	if err != nil {
		return err
	}
	policy.ExtractRefs = fresh.ExtractRefs
	policy.EditableAgents = s.launchEditableAgents(fresh)
	return s.workerNativePolicy(ctx, p, policy.Seal())
}

func decodeWorkerSourcePolicy(raw string, p *mcpgateway.SessionPolicy) error {
	if err := json.Unmarshal([]byte(raw), p); err != nil {
		return err
	}
	return p.Validate()
}

func (s *Service) retryWorkerNativeSource(latest *store.SessionRow, opts api.ResumeOptions) (*store.SessionRow, error) {
	if latest == nil || latest.State != "failed" || latest.WorkerID == "" || latest.ParentSessionID.String != opts.SourceSessionID {
		return nil, nativeOnlyError("canonical source changed or missing")
	}
	failed, err := s.Store.GetLaunchPlan(latest.ID)
	if err != nil || !failed.NativeResumeOnly || failed.WorkerSourceGeneration != opts.WorkerSourceGeneration || failed.WorkerPolicyDigest != opts.WorkerPolicyDigest {
		return nil, nativeOnlyError("failed attempt does not match requested continuation")
	}
	if _, err := s.Store.GetSessionProviderMapping(latest.ID, "tether", latest.ProviderID); !errors.Is(err, store.ErrProviderMappingNotFound) {
		return nil, nativeOnlyError("failed attempt had native custody; reconcile before retry")
	}
	source, err := s.Store.GetSession(opts.SourceSessionID)
	if err != nil || source.WorkerID != latest.WorkerID || source.LogicalAgentID != latest.LogicalAgentID {
		return nil, nativeOnlyError("retry source changed")
	}
	return source, nil
}
