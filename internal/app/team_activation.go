package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"path/filepath"
	"slices"
	"time"

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
	e, err := s.teamPlanEvidence(plan, s.launchWorker(id), nil)
	if err == nil {
		s.inspectInstalledTeamSandbox(plan, &e)
		if e.Worker != "" && s.Workers != nil {
			if id, resolveErr := s.Workers.Resolve(ctx, e.Worker); resolveErr == nil {
				e.PlacementResolved = s.Workers.Connected(id)
			}
		}
		e.Fingerprint = e.Digest()
	}
	return e, err
}

// teamPlanEvidence receives the same plan object that create persists. At launch,
// effective is the exact sandbox selected for the provider request (or worker spec).
func (s *Service) teamPlanEvidence(p *launch.Plan, worker string, effective *sandbox.Profile) (teamactivation.Launch, error) {
	s.catalogState.mu.RLock()
	cat := s.Catalog
	generation := s.catalogState.generation
	s.catalogState.mu.RUnlock()
	if cat == nil {
		return teamactivation.Launch{}, errors.New("catalog unavailable")
	}
	name := p.SandboxProfile
	if effective != nil {
		name = effective.ID
	} else if name == "" {
		name = cat.Agents[p.CatalogAgent()].Permissions.DefaultSandbox
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
	if sp.ID == "workspace-plus-net" {
		sp.AllowLoopback = true
	}
	sp.Description = ""
	normalize := func(in []string) []string {
		out := slices.Clone(in)
		for n := range out {
			out[n] = filepath.Clean(out[n])
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	sp.FS.Read = normalize(sp.FS.Read)
	sp.FS.Write = normalize(sp.FS.Write)
	sp.FS.Deny = normalize(sp.FS.Deny)
	sp.FS.Protect = normalize(sp.FS.Protect)
	sp.LoopbackForwardPorts = slices.Clone(sp.LoopbackForwardPorts)
	slices.Sort(sp.LoopbackForwardPorts)
	sp.LoopbackForwardPorts = slices.Compact(sp.LoopbackForwardPorts)
	raw, err := json.Marshal(struct {
		Domain string
		Policy sandbox.Profile
	}{"tether.sandbox-effective/v1", sp})
	if err != nil {
		return teamactivation.Launch{}, err
	}
	sum := sha256.Sum256(raw)
	privateDigest := "sha256:" + hex.EncodeToString(sum[:])
	structure := launchprofile.SandboxStructure{ReadCount: len(sp.FS.Read), WriteCount: len(sp.FS.Write), DenyCount: len(sp.FS.Deny), ProtectCount: len(sp.FS.Protect), Net: sp.Net, Loopback: sp.AllowLoopback, Subprocess: sp.Subprocess, HostFilesystem: sp.HostFilesystem, DenyGUI: sp.DenyGUILaunch, DenyServiceManager: sp.DenyUserServiceManager}
	raw, err = json.Marshal(struct {
		Domain    string
		Structure launchprofile.SandboxStructure
	}{"tether.sandbox-structure/v1", structure})
	if err != nil {
		return teamactivation.Launch{}, err
	}
	sum = sha256.Sum256(raw)
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	servers := slices.Clone(launch.EffectiveMCPServers(p.Env))
	slices.Sort(servers)
	e := teamactivation.Launch{Version: "tether.launch-evidence/v1", CatalogGeneration: generation, CatalogGenerationKnown: effective == nil, PlacementResolved: worker == "", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), BootProfileID: p.BootProfileID, ExecutionBasis: "legacy_catalog", LaunchID: p.LaunchID, CatalogAgent: p.CatalogAgent(), Provider: p.ProviderID, Brand: p.ProviderBrand, Runtime: p.RuntimeKind, CatalogModel: p.CatalogModel, Worker: worker, Project: p.ProjectID, PermissionMode: p.PermissionMode, Sandbox: name, SandboxDigest: "sha256:" + hex.EncodeToString(sum[:]), MCPServers: servers, ScopeResolution: "launch_admission_caller_bound", EnvironmentKeys: keys, Route: p.Route, Lifecycle: p.Lifecycle, Placement: "hub"}
	if worker != "" {
		e.Placement = "hub_asserted_not_worker_verified"
	}
	if !e.CatalogGenerationKnown {
		e.CatalogGeneration = 0
	}
	e.SandboxStructure = structure
	e.EffectiveSandboxPolicyDigest = privateDigest
	e.SandboxEnforcement = "unknown"
	e.Fingerprint = e.Digest()
	return e, nil
}

// Evidence capture must not change launch outcomes. Missing observations are
// reported as unknown, never reconstructed from today's catalog.
func (s *Service) recordTeamLaunchEvidence(ctx context.Context, id string, snapshot *teamactivation.Launch) {
	if snapshot == nil {
		return
	}
	e := *snapshot
	e.PlacementResolved = true
	if scopes, found, err := identity.NewStore(s.Store.DB()).SessionGrantedScopes(ctx, id); err == nil && found {
		e.GrantedScopes = slices.Clone(scopes)
		slices.Sort(e.GrantedScopes)
		e.ScopeResolution = "granted_session_credential"
	} else {
		e.ScopeResolution = "unknown"
	}
	if outcome, err := s.Store.TeamEvidence(ctx, "sandbox_result", id); err == nil {
		e.SandboxEnforcement = outcome.SandboxEnforcement
	}
	e.Fingerprint = e.Digest()
	_ = s.Store.RecordTeamEvidence(ctx, "launched", id, e)
}

func (s *Service) sealTeamPlanEvidence(p *launch.Plan, worker string) {
	if !p.TeamMember {
		return
	}
	if e, err := s.teamPlanEvidence(p, worker, nil); err == nil {
		e.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
		p.ActivationEvidence = &e
	}
}

func (s *Service) recordTeamSandboxOutcome(ctx context.Context, id string, out agentsessions.SandboxOutcome) {
	state := "unknown"
	switch {
	case out.Enforced && len(out.Unsupported) > 0:
		state = "downgraded"
	case out.Enforced:
		state = "enforced"
	case out.Disabled:
		state = "not_enforced"
	case len(out.Unsupported) > 0:
		state = "unsupported_platform"
	}
	if state == "unknown" {
		return
	}
	_ = s.Store.RecordTeamEvidence(ctx, "sandbox_result", id, teamactivation.Launch{Version: "tether.launch-evidence/v1", ExecutionBasis: "legacy_catalog", SandboxEnforcement: state})
}
