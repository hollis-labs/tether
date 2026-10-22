package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
	"github.com/hollis-labs/tether/internal/workspace"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func (s *Service) createWorkerSession(in CreateSessionInput) (*Launched, error) {
	return s.createWorkerSessionWith(in, "", nil)
}

// createWorkerSessionWith is createWorkerSession with a caller-supplied
// idempotency digest (empty means the ordinary create digest) and a plan
// mutator applied after the plan is built (team members use both).
func (s *Service) createWorkerSessionWith(in CreateSessionInput, digest string, mutate func(*launch.Plan) error) (*Launched, error) {
	if s.Workers == nil {
		return nil, workerhub.ErrChannelUnavailable
	}
	workerID, err := s.Workers.Resolve(context.Background(), in.Worker)
	if err != nil {
		return nil, err
	}
	if !s.Workers.Connected(workerID) {
		return nil, fmt.Errorf("worker %q: %w", in.Worker, workerhub.ErrOffline)
	}
	var key *store.SessionIdempotency
	if in.IdempotencyKey != "" {
		principal := idempotencyPrincipal(in.IdempotencyPrincipal)
		unlock := s.lockIdempotencyKey(principal, in.IdempotencyKey)
		defer unlock()
		if digest == "" {
			digest = createRequestDigest(in)
		}
		if in.ReplaceIdempotency {
			if err := s.releaseKeyForReplace(principal, in.IdempotencyKey); err != nil {
				return nil, err
			}
		}
		if l, err := s.replayIfKeyed(principal, in.IdempotencyKey, store.IdempotencyOpCreate, digest); l != nil || err != nil {
			return l, err
		}
		key = &store.SessionIdempotency{PrincipalID: principal, Key: in.IdempotencyKey, Operation: store.IdempotencyOpCreate, RequestDigest: digest}
	}
	plan, err := s.BuildLaunchPlan(in)
	if err != nil {
		return nil, err
	}
	if mutate != nil {
		if err = mutate(plan); err != nil {
			return nil, err
		}
	}
	s.applyCatalogIdentity(plan)
	id := uuid.NewString()
	root := filepath.Join(config.ResolveWorkspaceRoot(s.cat().Global.Catalog.Defaults, s.cat().Paths), "worker-sessions")
	ws, err := workspace.Create(root, id, plan)
	if err != nil {
		return nil, err
	}
	row := store.SessionRow{ID: id, WorkerID: workerID, LaunchID: plan.LaunchID, ProjectID: plan.ProjectID, LogicalAgentID: plan.LogicalAgentID, ProviderID: plan.ProviderID, ProviderKind: "cli", TaskID: nullableTaskID(plan.TaskID), App: nullableApp(plan.App), Workspace: ws.Root, State: "created", Publication: "tether-hosted"}
	s.sealTeamPlanEvidence(plan, row.WorkerID)
	if err = s.Store.CreateSessionKeyed(row, plan, key); err != nil {
		return nil, err
	}
	return &Launched{SessionID: id, Workspace: ws, Plan: plan, ProviderKind: "cli"}, nil
}
func (s *Service) workerSpec(ctx context.Context, id string, p *launch.Plan) (workerwire.LaunchSpec, error) {
	brand := p.ProviderBrand
	if brand == "" {
		brand = filepath.Base(p.Command)
	}
	if strings.Contains(brand, "claude") {
		brand = "claude"
	}
	spec := workerwire.LaunchSpec{Session: id, Actor: registry.LogicalAgentBindingTarget(p.LogicalAgentID), Provider: brand, PermissionMode: p.PermissionMode, RuntimeKind: p.RuntimeKind, Args: append([]string(nil), p.Args...), Env: p.Env, BootPrompt: p.BootPrompt, BootMode: p.BootMode, BootFiles: p.BootDirOverlay, NativeFiles: p.NativeFiles, Workspace: workerwire.Workspace{Kind: "scratch"}, MCPServers: append([]string{}, launch.EffectiveMCPServers(p.Env)...), ResumeSessionID: p.ResumeProviderSessionID}
	spec.ExtractRefs = p.ExtractRefs
	// The plan's map is shared; the hosted marker belongs to this session only.
	spec.Env = make(map[string]string, len(p.Env)+1)
	for k, v := range p.Env {
		spec.Env[k] = v
	}
	spec.Env[HostedSessionEnv] = id
	if p.Command != "" {
		spec.Command = filepath.Base(p.Command)
	}
	if !spec.ExtractRefs && s.cat() != nil {
		spec.ExtractRefs = config.EffectiveExtractRefs(s.cat().Global, s.cat().Projects[p.ProjectID], s.cat().Launches[p.LaunchID])
	}
	// The project's git remote is what a worker can clone; RepoRoot is a path on the hub's machine. Prefer
	// repo_url, then a URL-shaped repo_root, then a local checkout's origin (only exists when the hub has one).
	if p.RepoURL != "" {
		spec.Workspace = workerwire.Workspace{Kind: "git", URL: p.RepoURL}
	} else if strings.HasPrefix(p.RepoRoot, "https://") || strings.HasPrefix(p.RepoRoot, "git@") {
		spec.Workspace = workerwire.Workspace{Kind: "git", URL: p.RepoRoot}
	} else if p.RepoRoot != "" {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(probe, "git", "-C", p.RepoRoot, "remote", "get-url", "origin").Output() //nolint:gosec // G204: RepoRoot comes from the hub's own catalog, not from a worker
		if err == nil {
			spec.Workspace = workerwire.Workspace{Kind: "git", URL: strings.TrimSpace(string(out))}
		}
	}
	if p.Shared != nil {
		if spec.BootFiles == nil {
			spec.BootFiles = map[string]string{}
		}
		for name, path := range map[string]string{"CLAUDE.md": p.Shared.BootFile, "transient.md": p.Shared.TransientFile} {
			if path != "" {
				b, err := os.ReadFile(path) //nolint:gosec // G304: boot file paths come from the hub's resolved launch plan
				if err != nil {
					return spec, err
				}
				spec.BootFiles[name] = string(b)
			}
		}
	}
	var err error
	if p.SandboxProfile != "" {
		profile, ok, e := s.cat().SandboxProfile(p.CatalogAgent(), p.SandboxProfile)
		err = e
		if ok {
			spec.Sandbox = &profile
		}
	} else {
		profile, ok, e := s.cat().AgentSandbox(p.CatalogAgent())
		err = e
		if ok {
			spec.Sandbox = &profile
		}
	}
	return spec, err
}

// workerLaunchWait is how long the hub waits for a worker to finish a launch (a git clone plus process
// start). It must exceed workerruntime.WorkerLaunchTimeout so the worker gives up first.
const workerLaunchWait = 5 * time.Minute

const launchFailedPrefix = "worker launch failed"

func (s *Service) launchWorkerSession(ctx context.Context, row *store.SessionRow, p *launch.Plan) (*Launched, error) {
	spec, err := s.workerSpec(ctx, row.ID, p)
	if err != nil {
		return nil, err
	}
	var launchEvidence *launchprofile.LaunchEvidence
	if p.TeamMember {
		if evidence, evidenceErr := s.teamPlanEvidence(p, row.WorkerID, spec.Sandbox); evidenceErr == nil {
			evidence.CatalogGeneration = 0
			launchEvidence = &evidence
		}
	}
	spec.Env = sessionCorrelation(*row).SealMap(spec.Env)
	if len(p.EditableAgents) > 0 {
		if s.Workers == nil || !s.Workers.Supports(row.WorkerID, workerwire.FeatureEditableAgents) {
			exit := 1
			_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
			return nil, fmt.Errorf("agent declares editable_agents but worker does not support them; upgrade tether-worker")
		}
		spec.EditableAgents = slices.Clone(p.EditableAgents)
	}
	if spec.ExtractRefs && (s.Workers == nil || !s.Workers.Supports(row.WorkerID, workerwire.FeatureExtractRefs)) {
		exit := 1
		_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
		return nil, fmt.Errorf("extract_refs requires worker support; upgrade tether-worker")
	}
	if spec.ExtractRefs && spec.Provider != "claude" {
		exit := 1
		_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
		return nil, fmt.Errorf("extract_refs requires the worker's claude MCP relay")
	}
	if err := s.saveWorkerMCPPolicy(ctx, row, p, spec.ExtractRefs); err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
		return nil, fmt.Errorf("capture session MCP authority: %w", err)
	}
	if (spec.Provider == "codex" || spec.Provider == "antigravity") && (s.Workers == nil || !s.Workers.Supports(row.WorkerID, workerwire.FeatureProviderProcess)) {
		exit := 1
		_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
		return nil, fmt.Errorf("worker does not support native %s turns; upgrade tether-worker", spec.Provider)
	}
	spec.MCPPolicy = true
	undoIdentity, err := s.prepareWorkerIdentity(ctx, row, p, &spec)
	if err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
		return nil, err
	}
	if spec.ExtractRefs && spec.Credential == "" {
		undoIdentity()
		exit := 1
		_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
		return nil, fmt.Errorf("extract_refs requires a worker session credential")
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		undoIdentity()
		return nil, err
	}
	if err = s.Store.UpdateSessionState(row.ID, "launching", 0, nil); err != nil {
		undoIdentity()
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, workerLaunchWait)
	defer cancel()
	result, err := s.Workers.Send(wait, row.WorkerID, workerwire.Frame{T: "launch", CmdID: "launch:" + row.ID, Session: row.ID, Spec: raw})
	if err != nil {
		if result.Error == "" {
			// No verdict from the worker (canceled wait, dropped link): the
			// launch frame was sent and the worker may still start the
			// session. Marking it failed would hide a live process from stop and
			// rollback, and its later state report would resurrect it. Leave it
			// launching with its identity bound; the worker's state report or a
			// stop by session id settles it.
			log.Printf("worker session %s: launch outcome unknown, leaving it launching: %v", row.ID, err)
			return nil, err
		}
		undoIdentity()
		exit := 1
		_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
		cause := workerhub.LaunchFailureCause(err)
		log.Printf("WARN worker session %s: launch failed on worker %s: %s", row.ID, row.WorkerID, cause)
		if raw, mErr := json.Marshal(map[string]string{"worker": row.WorkerID, "cause": cause}); mErr == nil {
			_ = s.publishWorkerEvent(row.ID, "session.launch_failed", raw)
		}
		return nil, fmt.Errorf("%s: %w", launchFailedPrefix, err)
	}
	if err = s.Store.UpdateSessionState(row.ID, "running", result.PID, nil); err != nil {
		return nil, err
	}
	s.recordTeamLaunchEvidence(context.WithoutCancel(ctx), row.ID, launchEvidence)
	if spec.Provider == "claude" && spec.Credential != "" && s.Workers.Supports(row.WorkerID, workerwire.FeatureExtractRefs) {
		attribution := store.RefAttributionNone
		if spec.ExtractRefs {
			attribution = store.RefAttributionProxy
		}
		if err := s.Store.SetSessionRefAttribution(row.ID, attribution); err != nil {
			log.Printf("worker session %s: could not save ref attribution", row.ID)
		}
	}
	if s.Registry != nil && p.LogicalAgentID != "" {
		if _, err := s.Registry.LeaseBinding(ctx, registry.LogicalAgentBindingTarget(p.LogicalAgentID), row.ID, row.WorkerID, row.ID, nil, registry.VisibilityTetherHosted, 0); err != nil {
			return nil, err
		}
	}
	return &Launched{SessionID: row.ID, Workspace: workspace.Open(row.Workspace, row.ID), Plan: p, ProviderKind: row.ProviderKind}, nil
}
func (s *Service) sendWorkerCommand(ctx context.Context, row *store.SessionRow, kind string, data []byte) error {
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := s.Workers.Send(wait, row.WorkerID, workerwire.Frame{T: kind, CmdID: uuid.NewString(), Session: row.ID, Data: data})
	return err
}

// saveWorkerMCPPolicy records a worker-placed session's MCP authority the way a hub-local launch does, so the hub's
// /mcp gateway can serve the session's agent token with exactly this policy: the servers, profile and tool allowlist
// from the plan, the upstream ownership, extract_refs, and the editable_agents allowlist. Before this a worker session
// had no saved policy and /mcp refused it (CW-20261016-0160).
func (s *Service) saveWorkerMCPPolicy(ctx context.Context, row *store.SessionRow, p *launch.Plan, extractRefs bool) error {
	var cfg mcpgateway.Config
	if s.cat() != nil {
		cfg = s.cat().Global.MCP
	}
	ownership, err := s.mcpOwnership()
	if err != nil {
		return err
	}
	policy, err := sessionMCPPolicy(row.ID, row.LogicalAgentID, p, cfg)
	if err != nil {
		return err
	}
	policy.UpstreamOwnership = ownership
	policy.ExtractRefs = extractRefs
	policy.EditableAgents = s.launchEditableAgents(p)
	return s.Store.SaveSessionMCPPolicy(ctx, policy.Seal())
}
