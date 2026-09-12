package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/hollis-labs/agentkit/agentlaunch/sessionshim"
	"github.com/hollis-labs/agentkit/agentruntime/runtimekind"
	"github.com/hollis-labs/agentkit/agentruntime/sessionkit"
	"github.com/hollis-labs/agentkit/agentruntime/turn"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

// localHostID is the RuntimeBinding host_id every session launched by this
// daemon leases under. See leaseActorBinding's doc comment for why a fixed
// literal is accurate rather than invented (no multi-host clustering model
// exists within one daemon instance -- ADR 0045).
const localHostID = "local"

// Launched is the success return of CreateSession / LaunchSession. Wait is nil
// on a Create-only return — there's nothing to wait on until LaunchSession runs.
type Launched struct {
	SessionID string
	Workspace *workspace.Session
	Plan      *launch.Plan

	// Wait blocks until the session reaches a terminal state and returns
	// its exit code. Routes through agentsessions.Manager; safe to call
	// from any goroutine, safe to call after the session has already
	// exited. Nil on the result of CreateSession — launching first is
	// required before there's anything to wait on.
	Wait func(ctx context.Context) (int, error)

	// ProviderKind is the runtime family ("cli" | "api"), resolved from
	// the runtime factory at create/launch time. Populated as part of
	// ADR 0022 G2/G3 so consumers don't need a follow-up catalog lookup.
	ProviderKind string
}

// ErrSessionNotCreated is returned by LaunchSession when the target
// session is in any state other than "created". Once launched, a
// session cannot be launched again — clients create a new session for
// a retry.
//
// Deprecated: prefer session.ErrNotCreated; this alias is kept so
// existing callers that reference app.ErrSessionNotCreated do not break.
var ErrSessionNotCreated = session.ErrNotCreated

// CreateSessionWithBootPrompt creates a session like CreateSession but
// replaces the catalog's static boot prompt with the provided text.
// Used by `mux boot <profile_id>` to inject a dynamically generated
// prompt without modifying the catalog.
func (s *Service) CreateSessionWithBootPrompt(launchID, bootPrompt string) (*Launched, error) {
	return s.CreateSessionWithInput(CreateSessionInput{
		LaunchID:           launchID,
		BootPromptOverride: bootPrompt,
	})
}

// CreateSession resolves the launch plan, materializes the workspace,
// and persists the session row in state=created along with the plan
// JSON. It does not start the runtime — call LaunchSession for that.
func (s *Service) CreateSession(launchID string) (*Launched, error) {
	return s.CreateSessionWithInput(CreateSessionInput{LaunchID: launchID})
}

// CreateSessionWithInput is the v005-08 Agent Ops entry point: resolves the
// base launch plan, applies caller-provided agent/boot-profile/override
// payloads, recomposes the boot prompt with skills, and persists the session.
//
// For Tier 1 (catalog-only) callers, pass LaunchID and leave the rest zero —
// behavior matches the pre-v005-08 CreateSession exactly.
//
// For Tier 2 (caller-provided), populate AgentFile / AgentInline /
// BootProfileFile / Override as needed. See CreateSessionInput for precedence.
func (s *Service) CreateSessionWithInput(in CreateSessionInput) (*Launched, error) {
	plan, err := s.BuildLaunchPlan(in)
	if err != nil {
		return nil, err
	}
	return s.createSessionFromPlan(plan)
}

// BuildLaunchPlan resolves and applies Agent Ops input without materializing a
// Tether session. It is used by direct-exec flows that need the same launch
// prompt/env composition as CreateSessionWithInput but must not persist a
// session row or involve the daemon/attach broker.
func (s *Service) BuildLaunchPlan(in CreateSessionInput) (*launch.Plan, error) {
	if in.LaunchID == "" {
		return nil, fmt.Errorf("launch id required")
	}
	plan, err := s.resolveWithInput(in)
	if err != nil {
		return nil, err
	}
	if err := s.applyAgentOps(plan, in); err != nil {
		return nil, err
	}
	return plan, nil
}

func (s *Service) createSessionFromPlan(plan *launch.Plan) (launched *Launched, err error) {
	sessID := uuid.NewString()

	wsRoot := plan.WriteHome
	if wsRoot == "" {
		// Explicit catalog workspace_root wins; cat.Paths supplies the
		// go-apppaths fallback only when global.yaml omits the key.
		wsRoot = filepath.Join(config.ResolveWorkspaceRoot(s.Catalog.Global.Catalog.Defaults, s.Catalog.Paths), plan.ProjectID)
	}
	if err = workspace.MaterializeWorkRoot(wsRoot, sessID, plan); err != nil {
		return nil, err
	}
	// Cleanup-on-error: if any step after MaterializeWorkRoot fails, remove the
	// worktree this call just materialized so a failed create does not leak a
	// git worktree (and its stale registration). RemoveMaterializedWorkRoot is
	// a no-op for shared/hybrid plans, so this never touches a shared repo_root.
	// Cleared once the session row is committed — at that point the worktree is
	// owned by the persisted session and follows the daemon retention policy.
	defer func() {
		if err != nil {
			if rmErr := workspace.RemoveMaterializedWorkRoot(plan); rmErr != nil {
				log.Printf("app: cleanup worktree for failed session create %s: %v", sessID, rmErr)
			}
		}
	}()

	if err = s.RefreshBootProfilePrompt(context.Background(), plan); err != nil {
		return nil, err
	}
	ws, err := workspace.Create(wsRoot, sessID, plan)
	if err != nil {
		return nil, err
	}

	// Fail fast if the catalog references a provider the factory map
	// can't satisfy. Better to error here than at launch time when the
	// user thinks they have a created session.
	factory, ok := s.factories[plan.ProviderID]
	if !ok {
		err = fmt.Errorf("no runtime for provider %q", plan.ProviderID)
		return nil, err
	}
	probe, err := factory(plan)
	if err != nil {
		err = fmt.Errorf("build runtime for %q: %w", plan.ProviderID, err)
		return nil, err
	}
	providerKind := probe.Kind()

	row := store.SessionRow{
		ID:             sessID,
		LaunchID:       plan.LaunchID,
		ProjectID:      plan.ProjectID,
		LogicalAgentID: plan.LogicalAgentID,
		ProviderID:     plan.ProviderID,
		ProviderKind:   providerKind,
		Workspace:      ws.Root,
		State:          string(session.StateCreated),
	}
	if err = s.Store.CreateSession(row, plan); err != nil {
		return nil, err
	}

	return &Launched{
		SessionID:    sessID,
		Workspace:    ws,
		Plan:         plan,
		ProviderKind: providerKind,
	}, nil
}

// LaunchSession starts a previously-created session. Rehydrates the
// plan from the store, opens the workspace non-destructively, runs
// Runtime.Prepare and hands off to agentsessions.Manager.Start.
// Returns ErrSessionNotCreated when the target is in any state other
// than "created" — relaunch of terminated sessions is not supported.
func (s *Service) LaunchSession(sessionID string) (*Launched, error) {
	row, err := s.Store.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if row.State != string(session.StateCreated) {
		return nil, fmt.Errorf("%w (state=%q)", ErrSessionNotCreated, row.State)
	}

	plan, err := s.Store.GetLaunchPlan(sessionID)
	if err != nil {
		return nil, fmt.Errorf("load launch plan: %w", err)
	}

	factory, ok := s.factories[plan.ProviderID]
	if !ok {
		return nil, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}
	rt, err := factory(plan)
	if err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(sessionID, string(session.StateFailed), 0, &exit)
		return nil, fmt.Errorf("build runtime: %w", err)
	}

	ws := workspace.Open(row.Workspace, sessionID)

	if err := rt.Prepare(context.Background()); err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(sessionID, string(session.StateFailed), 0, &exit)
		return nil, err
	}

	// Resolve sandbox profile if the agent specifies one.
	var profile sandbox.Profile
	if a, ok := s.Catalog.Agents[plan.LogicalAgentID]; ok {
		if name := a.Permissions.DefaultSandbox; name != "" {
			if sp, ok := s.Catalog.SandboxProfiles[name]; ok {
				profile = sp
				if profile.ID == "workspace-plus-net" {
					profile.AllowLoopback = true
				}
			}
		}
	}

	// Record provider-side session IDs for crash-recovery flows. Normal
	// launches must not feed the stored ID back as SessionIDPreset; that
	// would turn every boot into an implicit `--resume`.
	var onSessionID func(string)
	if providerRecordsSessionID(plan.ProviderID) {
		logicalAgentID := plan.LogicalAgentID
		storeRef := s.Store
		providerID := plan.ProviderID
		canonicalSessionID := sessionID
		onSessionID = func(id string) {
			// Legacy compatibility mirror -- unchanged. See T01 contract
			// §3.1: logical_agents.claude_session_id is write-only in
			// production (nothing reads it back) and shared across every
			// provider kind rather than scoped per session; kept as-is so
			// nothing that might still depend on it regresses.
			if err := storeRef.SetClaudeSessionID(logicalAgentID, id); err != nil {
				log.Printf("%s: persist session_id for %q failed: %v", providerID, logicalAgentID, err)
			}
			// Canonical mapping (T02, messaging vNext): scoped to THIS
			// session and THIS provider, not a single shared slot on the
			// logical agent. This is the mapping table new callers should
			// read; the column above is compatibility-only.
			if err := storeRef.UpsertSessionProviderMapping(canonicalSessionID, "tether", providerID, id); err != nil {
				log.Printf("%s: persist session provider mapping for session %q failed: %v", providerID, canonicalSessionID, err)
			}
		}
	}

	// One decision, one value. The argv that gets planted and the attribution
	// that gets stamped come from the same call, so nothing here can write a
	// stamp that disagrees with the flags actually planted -- see MuxMCPPlan.
	//
	// extractRefs is false because nothing can turn it on yet
	// (CW-20260912-0112); the resulting "none" is the honest record that this
	// session's proxy was not asked to record refs, which S5's digest renders
	// instead of showing an unexplained empty proxy column.
	mcpPlan := MuxMCPPlant(s.CatalogRoot, sessionID, false)
	prepared, err := s.prepareSharedLaunch(context.Background(), plan, ws.Root, plantContextInput{
		MuxCommand: muxCommandPath(),
		MuxArgs:    mcpPlan.Args,
		MuxEnv:     muxEnvMap(plan.Env),
	})
	if err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(sessionID, string(session.StateFailed), 0, &exit)
		return nil, fmt.Errorf("prepare shared launch: %w", err)
	}
	// Stamped AFTER the planting succeeded, not before: the column records
	// what was planted, and a failed prepare planted nothing. Best-effort --
	// a launch must not fail because an audit field could not be written, and
	// the column's NULL state already means "unknown", which is the truth if
	// this write is the thing that failed.
	if err := s.Store.SetSessionRefAttribution(sessionID, mcpPlan.Attribution); err != nil {
		log.Printf("session %s: record ref attribution %q failed: %v", sessionID, mcpPlan.Attribution, err)
	}

	onBootDirPlanted := makeBootDirPlantedCallback(s.Bus, sessionID, plan.LogicalAgentID)
	onBootDirPlanted(prepared.PlantedBootDir)
	sessionLaunch, err := sessionshim.ToSessionLaunch(prepared)
	if err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(sessionID, string(session.StateFailed), 0, &exit)
		return nil, fmt.Errorf("prepare session launch: %w", err)
	}

	startOpts := sessionLaunch.Options
	startOpts.LogPath = ws.LogPath
	startOpts.WorkspaceDir = ws.Root
	startOpts.Env = mergeEnv(provider.BuildEnv(plan.EnvMode, plan.EnvPassthrough, plan.EnvRedact, plan.Env, os.Environ()), prepared.Env)
	startOpts.ExtraArgs = sharedExtraArgs(prepared.Argv, plan.Args)
	startOpts.Profile = profile
	startOpts.OnSessionID = onSessionID
	startOpts.SessionIDPreset = plan.ResumeProviderSessionID
	startOpts.AttachEnabled = true
	startOpts.AutoPlantBootDir = false
	// Answer codex app-server's server-initiated approval requests. Only
	// the jsonrpc-stdio runtime ever consults this, so setting it for every
	// launch is inert elsewhere rather than conditional here. Without it
	// agentkit's nil-hook fallback refuses every MCP tool call — see
	// codex_approval.go.
	startOpts.JsonRpcRequestHook = jsonRPCRequestHook(sessionID)

	deferPTYStdinBootPrompt(rt.Caps(), &startOpts)

	req := agentsessions.StartRequest{
		ID:      sessionID,
		Runtime: rt,
		Options: startOpts,
		SessionMeta: map[string]string{
			"logical_agent_id": plan.LogicalAgentID,
			"project_id":       plan.ProjectID,
			"launch_id":        plan.LaunchID,
			"provider_id":      plan.ProviderID,
		},
	}
	if err := s.Manager.Start(context.Background(), req); err != nil {
		return nil, err
	}

	// Record this launch profile on the logical agent so the resume
	// endpoint knows which catalog config to use next time.
	if err := s.Store.SetLogicalAgentLaunchID(plan.LogicalAgentID, plan.LaunchID); err != nil {
		log.Printf("app: set launch_id on logical_agent %q: %v (non-fatal)", plan.LogicalAgentID, err)
	}

	// T06 (messaging vNext): this session becomes the durable actor's home
	// host. Leasing a binding here is what makes ResolveActorSession's
	// binding-first resolution (wake.go) actually authoritative in
	// practice, rather than dead T02 infrastructure -- LeaseBinding always
	// mints max-generation+1, so this call alone fences out whatever
	// session previously held the binding (concurrent-actor-session /
	// stale-generation handling), with no separate revoke step required on
	// the replaced side. Best-effort: a lease failure must not fail an
	// otherwise-successful launch (established enhancement-write pattern,
	// e.g. SetClaudeSessionID above).
	s.leaseActorBinding(sessionID, plan.LogicalAgentID)

	return &Launched{
		SessionID:    sessionID,
		Workspace:    ws,
		Plan:         plan,
		ProviderKind: rt.Kind(),
		Wait: func(ctx context.Context) (int, error) {
			return s.Manager.WaitSession(ctx, sessionID)
		},
	}, nil
}

// deferPTYStdinBootPrompt avoids writing large generated boot prompts to PTY
// stdin before the reader is active. AutoFireFirstTurn runs after startup has
// established the read side, so large prompts cannot block Runtime.Start on a
// full PTY input buffer.
func deferPTYStdinBootPrompt(caps agentsessions.Capabilities, opts *agentsessions.StartOptions) {
	if opts == nil || !caps.PTY || opts.BootMode != "stdin" || opts.BootPrompt == "" {
		return
	}
	bootPrompt := opts.BootPrompt
	opts.BootPrompt = ""
	opts.BootMode = ""
	_ = sessionkit.ApplyFirstTurnPolicy(opts, sessionkit.FirstTurnPolicy{
		Mode:   sessionkit.AutoFireFirstTurn,
		Prompt: bootPrompt,
		Turn: turn.Options{
			Runtime: runtimekind.PTY,
		},
	})
}

// ListSessions delegates to the store with the given list filter.
func (s *Service) ListSessions(opts store.ListSessionsOptions) ([]store.SessionRow, error) {
	return s.Store.ListSessions(opts)
}

// GetSession returns a single session row by ID.
func (s *Service) GetSession(id string) (*store.SessionRow, error) {
	return s.Store.GetSession(id)
}

// StopSession routes through agentsessions.Manager.Stop.
func (s *Service) StopSession(id string) error {
	if _, ok := s.Manager.Get(id); !ok {
		return agentsessions.ErrSessionNotRunning
	}
	row, err := s.Store.GetSession(id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(row.LogicalAgentID) != "" {
		policy, err := s.Store.GetLogicalAgentPolicy(row.LogicalAgentID)
		if err != nil {
			return err
		}
		if policy.CheckpointPolicy == agent.CheckpointPolicyOnStop {
			if err := s.CreatePolicyCheckpoint(row, policy); err != nil {
				return err
			}
		}
	}
	err = s.Manager.Stop(context.Background(), id)
	if err == nil && strings.TrimSpace(row.LogicalAgentID) != "" {
		s.revokeActorBindingIfCurrent(id, row.LogicalAgentID)
	}
	return err
}

// leaseActorBinding best-effort-leases a T02 RuntimeBinding for a durable
// actor's session at launch time (T06). No-op when LogicalAgentID is empty
// (a one-off session that never manufactures a durable actor record just
// by existing -- architecture: "A one-off session need not manufacture a
// permanent actor record merely to send a message") or when Registry isn't
// wired (lighter composition contexts, e.g. read-only `mux mcp`).
// hostID is a fixed literal: Tether has no multi-host clustering model
// within one daemon instance (ADR 0045) -- every session a given daemon
// manages IS that one host, so a constant is accurate, not invented.
// attemptID reuses sessionID: Tether's runtime model has no finer-grained
// "attempt" identity distinct from a session today (a resumed session gets
// a NEW SessionID with parent lineage, per T02, rather than reattaching a
// prior attempt under the same ID), so session-scoped is the honest
// current granularity, not a placeholder for something more precise that
// already exists.
func (s *Service) leaseActorBinding(sessionID, logicalAgentID string) {
	if s.Registry == nil || strings.TrimSpace(logicalAgentID) == "" {
		return
	}
	target := registry.LogicalAgentBindingTarget(logicalAgentID)
	if _, err := s.Registry.LeaseBinding(context.Background(), target, sessionID, localHostID, sessionID, nil, registry.VisibilityPrivateLocal, 0); err != nil {
		log.Printf("app: lease runtime binding for logical agent %q session %q failed (non-fatal): %v", logicalAgentID, sessionID, err)
	}
}

// revokeActorBindingIfCurrent best-effort-revokes id's own binding on
// graceful stop, but only when it is STILL the current (highest,
// non-revoked) generation for logicalAgentID -- a delayed stop call for a
// session already superseded by a newer launch must never revoke the
// newer session's active binding. Not calling this at all would still be
// correct (a stale binding is already treated as offline by
// ResolveActorSession); this is hygiene, not a correctness requirement.
func (s *Service) revokeActorBindingIfCurrent(sessionID, logicalAgentID string) {
	if s.Registry == nil {
		return
	}
	ctx := context.Background()
	target := registry.LogicalAgentBindingTarget(logicalAgentID)
	current, err := s.Registry.CurrentBinding(ctx, target)
	if err != nil {
		return // ErrBindingNotFound or a lookup failure: nothing to revoke.
	}
	if current.SessionID != sessionID {
		return // superseded by a newer launch; leave the newer binding alone.
	}
	if err := s.Registry.RevokeBinding(ctx, current.ID); err != nil {
		log.Printf("app: revoke runtime binding %q for logical agent %q session %q failed (non-fatal): %v", current.ID, logicalAgentID, sessionID, err)
	}
}

// WaitSession blocks until the named session reaches a terminal state and
// returns its exit code. Thin wrapper over agentsessions.Manager.WaitSession
// so callers (including the daemon's HTTP handlers) don't reach past Service
// into the lib.
func (s *Service) WaitSession(ctx context.Context, id string) (int, error) {
	return s.Manager.WaitSession(ctx, id)
}

// AttachSession streams the named session's live output to w until ctx is
// canceled or the session exits. sinceSeq is a byte-offset hint for resume;
// 0 means "replay full ring then go live". Thin wrapper over
// agentsessions.Manager.AttachWith.
func (s *Service) AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error {
	return s.Manager.AttachWith(ctx, id, w, agentsessions.AttachOptions{SinceSeq: sinceSeq})
}

// AttachedClients reports the in-memory count of live attach subscribers
// for id, or 0 if the session is not currently registered.
func (s *Service) AttachedClients(id string) int {
	info, ok := s.Manager.Get(id)
	if !ok {
		return 0
	}
	return info.AttachedClients
}

// RuntimeHealth returns the live health snapshot for a running session by
// delegating to agentsessions.Manager.Health. Returns (zero, false) when
// the session is not currently registered.
func (s *Service) RuntimeHealth(id string) (api.RuntimeHealthResult, bool) {
	snap, ok := s.Manager.Health(id)
	if !ok {
		return api.RuntimeHealthResult{}, false
	}
	return api.RuntimeHealthResult{
		SessionID:    snap.SessionID,
		ProviderID:   snap.RuntimeID,
		ProviderKind: snap.RuntimeKind,
		Caps:         snap.Caps,
		Health:       snap.Health,
	}, true
}
