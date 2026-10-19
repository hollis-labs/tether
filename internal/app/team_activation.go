package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/hollis-labs/libs/harness/core/sandbox"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/teamactivation"
)

func (s *Service) InspectTeamActivation(ctx context.Context, q teamactivation.Query) (teamactivation.Evidence, error) {
	if s.TeamActivationInspection == nil {
		return teamactivation.Evidence{}, errors.New("team activation inspection unavailable")
	}
	return s.TeamActivationInspection(ctx, q)
}

func (s *Service) InspectTeamLaunch(ctx context.Context, id string) (teamactivation.Launch, error) {
	if err := ctx.Err(); err != nil {
		return teamactivation.Launch{}, err
	}
	plan, err := s.BuildLaunchPlan(CreateSessionInput{LaunchID: id})
	if err != nil {
		return teamactivation.Launch{}, err
	}
	return s.teamPlanEvidence(plan, s.launchWorker(id), nil)
}

// teamPlanEvidence receives the same plan object that create persists. At launch,
// effective is the exact sandbox selected for the provider request (or worker spec).
func (s *Service) teamPlanEvidence(p *launch.Plan, worker string, effective *sandbox.Profile) (teamactivation.Launch, error) {
	cat := s.CurrentCatalog()
	if cat == nil {
		return teamactivation.Launch{}, errors.New("catalog unavailable")
	}
	a := cat.Agents[p.CatalogAgent()]
	name := p.SandboxProfile
	if name == "" {
		name = a.Permissions.DefaultSandbox
	}
	var sp sandbox.Profile
	if effective != nil {
		sp = *effective
	} else {
		var err error
		sp, _, err = cat.SandboxProfile(p.CatalogAgent(), name)
		if err != nil {
			return teamactivation.Launch{}, err
		}
	}
	// Sandbox configuration is policy, not environment/argv or authored content.
	// Only its digest is returned; paths inside the policy are never exposed.
	sp.Description = ""
	raw, err := json.Marshal(sp)
	if err != nil {
		return teamactivation.Launch{}, err
	}
	sum := sha256.Sum256(raw)
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	servers := slices.Clone(launch.EffectiveMCPServers(p.Env))
	slices.Sort(servers)
	e := teamactivation.Launch{Version: "tether.launch-evidence/v1", ExecutionBasis: "legacy_catalog", LaunchID: p.LaunchID, CatalogAgent: p.CatalogAgent(), Provider: p.ProviderID, Brand: p.ProviderBrand, Runtime: p.RuntimeKind, CatalogModel: a.Model, Worker: worker, Project: p.ProjectID, PermissionMode: p.PermissionMode, Sandbox: name, SandboxDigest: "sha256:" + hex.EncodeToString(sum[:]), MCPServers: servers, ScopeResolution: "launch_admission_caller_bound", EnvironmentKeys: keys, Route: p.Route, Lifecycle: p.Lifecycle, Placement: "hub"}
	if worker != "" {
		e.Placement = "hub_asserted_not_worker_verified"
	}
	e.Fingerprint = e.Digest()
	return e, nil
}

// Evidence capture must not change launch outcomes. Missing observations are
// reported as unknown, never reconstructed from today's catalog.
func (s *Service) recordTeamLaunchEvidence(ctx context.Context, id string, p *launch.Plan, worker string, sp *sandbox.Profile) {
	if !p.TeamMember {
		return
	}
	e, err := s.teamPlanEvidence(p, worker, sp)
	if err == nil {
		if scopes, found, scopeErr := identity.NewStore(s.Store.DB()).SessionGrantedScopes(ctx, id); scopeErr == nil && found {
			e.GrantedScopes = slices.Clone(scopes)
			slices.Sort(e.GrantedScopes)
			e.ScopeResolution = "granted_session_credential"
		} else {
			e.ScopeResolution = "unknown"
		}
		e.Fingerprint = e.Digest()
		_ = s.Store.RecordTeamEvidence(ctx, "launched", id, e)
	}
}

func (s *Service) sealTeamPlanEvidence(p *launch.Plan, worker string) {
	if !p.TeamMember {
		return
	}
	if e, err := s.teamPlanEvidence(p, worker, nil); err == nil {
		p.ActivationEvidence = &e
	}
}
