package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	"github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/store"
	"github.com/chrispian/agent-mux/internal/workspace"
)

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
	if in.LaunchID == "" {
		return nil, fmt.Errorf("launch id required")
	}
	plan, err := s.Resolve(in.LaunchID)
	if err != nil {
		return nil, err
	}
	if err := s.applyAgentOps(plan, in); err != nil {
		return nil, err
	}
	return s.createSessionFromPlan(plan)
}

func (s *Service) createSessionFromPlan(plan *launch.Plan) (*Launched, error) {
	sessID := uuid.NewString()

	wsRoot := plan.WriteHome
	if wsRoot == "" {
		wsRoot = filepath.Join(config.Expand(s.Catalog.Global.Catalog.Defaults.WorkspaceRoot), plan.ProjectID)
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
		return nil, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}
	probe, err := factory(plan)
	if err != nil {
		return nil, fmt.Errorf("build runtime for %q: %w", plan.ProviderID, err)
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
	if err := s.Store.CreateSession(row, plan); err != nil {
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

	// Resume continuity for turn-based providers that use a CLI session ID
	// (--resume / --session). Adapters that don't use this are inert.
	var sessionIDPreset string
	var onSessionID func(string)
	if providerHasSessionIDContinuity(plan.ProviderID) {
		if preset, err := s.Store.GetClaudeSessionID(plan.LogicalAgentID); err == nil {
			sessionIDPreset = preset
		}
		logicalAgentID := plan.LogicalAgentID
		storeRef := s.Store
		providerID := plan.ProviderID
		onSessionID = func(id string) {
			if err := storeRef.SetClaudeSessionID(logicalAgentID, id); err != nil {
				log.Printf("%s: persist session_id for %q failed: %v", providerID, logicalAgentID, err)
			}
		}
	}

	onBootDirPlanted := makeBootDirPlantedCallback(s.Bus, sessionID, plan.LogicalAgentID)

	req := agentsessions.StartRequest{
		ID:      sessionID,
		Runtime: rt,
		Options: agentsessions.StartOptions{
			Workdir:          plan.RepoRoot,
			WorkspaceDir:     ws.Root,
			LogPath:          ws.LogPath,
			BootPrompt:       plan.BootPrompt,
			BootMode:         plan.BootMode,
			Env:              provider.BuildEnv(plan.EnvMode, plan.EnvPassthrough, plan.EnvRedact, plan.Env, os.Environ()),
			Profile:          profile,
			SessionIDPreset:  sessionIDPreset,
			OnSessionID:      onSessionID,
			AttachEnabled:    true,
			AutoPlantBootDir: true,
			OnBootDirPlanted: onBootDirPlanted,
		},
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
	return s.Manager.Stop(context.Background(), id)
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
