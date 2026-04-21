package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/chrispian/agent-mux/internal/agent"
	"github.com/chrispian/agent-mux/internal/broker"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/events"
	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/provider/api/stub"
	"github.com/chrispian/agent-mux/internal/provider/cli/claudecode"
	"github.com/chrispian/agent-mux/internal/provider/cli/claudestream"
	"github.com/chrispian/agent-mux/internal/runtime"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/store"
	"github.com/chrispian/agent-mux/internal/workspace"
)

// Service is the composition root: it wires catalog, store, providers, and
// the runtime manager. Lifecycle ownership of running sessions lives in
// internal/runtime; Service only orchestrates launch preconditions (plan
// resolution, workspace creation, persistence of the initial row) and then
// hands the handle off to the manager.
type Service struct {
	CatalogRoot string
	Catalog     *config.Catalog
	Store       *store.Store
	Providers   *provider.Registry
	Runtime     *runtime.Manager
	Bus         events.Bus
	Broker      *broker.Service
}

func New(catalogRoot string) (*Service, error) {
	cat, err := config.Load(catalogRoot)
	if err != nil {
		return nil, err
	}
	if err := cat.Validate(); err != nil {
		return nil, err
	}
	dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
	if dbPath == "" {
		return nil, fmt.Errorf("global.defaults.state_db missing")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	reg := provider.NewRegistry()
	reg.Register(claudecode.Adapter{})
	reg.Register(claudestream.Adapter{})
	reg.Register(stub.Runtime{})
	// Reconcile stale rows from a prior daemon instance: sessions stuck in
	// launching/running are orphaned (the process they tracked is gone),
	// and open client_attachments are no longer live.
	now := time.Now().UTC().Format(time.RFC3339)
	if swept, err := db.SweepStaleSessions(now); err == nil && swept > 0 {
		log.Printf("store: swept %d stale session(s) to failed", swept)
	}
	if swept, err := db.SweepStaleAttachments(now); err == nil && swept > 0 {
		log.Printf("store: swept %d stale client_attachments row(s)", swept)
	}
	// Seed logical_agents from the catalog. Upsert — idempotent across
	// restarts; preserves created_at + any future operator-set fields.
	// See ADR 0003.
	if n, err := seedLogicalAgents(db, cat.Agents); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("seed logical_agents: %w", err)
	} else if n > 0 {
		log.Printf("store: seeded %d logical_agent row(s) from catalog", n)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})
	mgr := runtime.NewManager(db).WithAttachmentSink(db).WithEventPublisher(bus)
	brk := broker.NewService(db, bus)
	return &Service{
		CatalogRoot: catalogRoot,
		Catalog:     cat,
		Store:       db,
		Providers:   reg,
		Runtime:     mgr,
		Bus:         bus,
		Broker:      brk,
	}, nil
}

func (s *Service) Close() error {
	if err := s.Runtime.Shutdown(context.Background()); err != nil {
		return err
	}
	return s.Store.Close()
}

func (s *Service) ListProjects() []config.Project {
	out := make([]config.Project, 0, len(s.Catalog.Projects))
	for _, p := range s.Catalog.Projects {
		out = append(out, p)
	}
	return out
}

func (s *Service) ListAgents() []config.Agent {
	out := make([]config.Agent, 0, len(s.Catalog.Agents))
	for _, a := range s.Catalog.Agents {
		out = append(out, a)
	}
	return out
}

func (s *Service) ListProviders() []config.Provider {
	out := make([]config.Provider, 0, len(s.Catalog.Providers))
	for _, p := range s.Catalog.Providers {
		out = append(out, p)
	}
	return out
}

func (s *Service) Resolve(launchID string) (*launch.Plan, error) {
	return launch.Resolve(s.Catalog, launch.Input{LaunchID: launchID, CatalogRoot: s.CatalogRoot})
}

type Launched struct {
	SessionID string
	Workspace *workspace.Session
	Plan      *launch.Plan

	// Wait blocks until the session reaches a terminal state and returns
	// its exit code. Routes through runtime.Manager; safe to call from any
	// goroutine, safe to call after the session has already exited.
	// Nil on the result of CreateSession — launching first is required
	// before there's anything to wait on.
	Wait func(ctx context.Context) (int, error)
}

// ErrSessionNotCreated is returned by LaunchSession when the target
// session is in any state other than "created". Once launched, a
// session cannot be launched again — clients create a new session for
// a retry.
var ErrSessionNotCreated = fmt.Errorf("session is not in 'created' state")

// CreateSession resolves the launch plan, materializes the workspace,
// and persists the session row in state=created along with the plan
// JSON. It does not start the runtime — call LaunchSession for that.
// Splitting create from launch lets external clients inspect the
// prepared session (plan, workspace paths) before committing to run,
// and gives the HTTP surface the two-endpoint shape promised by the
// context pack.
func (s *Service) CreateSession(launchID string) (*Launched, error) {
	plan, err := s.Resolve(launchID)
	if err != nil {
		return nil, err
	}
	sessID := uuid.NewString()

	wsRoot := plan.WriteHome
	if wsRoot == "" {
		wsRoot = filepath.Join(config.Expand(s.Catalog.Global.Catalog.Defaults.WorkspaceRoot), plan.ProjectID)
	}
	ws, err := workspace.Create(wsRoot, sessID, plan)
	if err != nil {
		return nil, err
	}

	// Fail fast if the catalog references a provider the registry can't
	// satisfy. Better to error here than at launch time when the user
	// thinks they have a created session.
	if _, ok := s.Providers.Get(plan.ProviderID); !ok {
		return nil, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}

	row := store.SessionRow{
		ID:             sessID,
		LaunchID:       plan.LaunchID,
		ProjectID:      plan.ProjectID,
		LogicalAgentID: plan.LogicalAgentID,
		ProviderID:     plan.ProviderID,
		Workspace:      ws.Root,
		State:          string(session.StateCreated),
	}
	if err := s.Store.CreateSession(row, plan); err != nil {
		return nil, err
	}

	return &Launched{
		SessionID: sessID,
		Workspace: ws,
		Plan:      plan,
	}, nil
}

// LaunchSession starts a previously-created session. Rehydrates the
// plan from the store, opens the workspace non-destructively, runs
// Runtime.Prepare and hands off to runtime.Manager.Start. Returns
// ErrSessionNotCreated when the target is in any state other than
// "created" — v0.0.2 does not support relaunch of terminated sessions.
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

	rt, ok := s.Providers.Get(plan.ProviderID)
	if !ok {
		return nil, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}

	ws := workspace.Open(row.Workspace, sessionID)

	if err := rt.Prepare(context.Background(), plan); err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(sessionID, string(session.StateFailed), 0, &exit)
		return nil, err
	}

	req := runtime.StartRequest{
		ID:        sessionID,
		Plan:      plan,
		Workspace: ws,
		Runtime:   rt,
	}
	// Resolve sandbox profile if the agent specifies one.
	if a, ok := s.Catalog.Agents[plan.LogicalAgentID]; ok {
		if name := a.Permissions.DefaultSandbox; name != "" {
			if sp, ok := s.Catalog.SandboxProfiles[name]; ok {
				req.SandboxProfile = &sp
			}
		}
	}
	// Resume continuity for claudestream sessions (T-v004-s02-05). For
	// other provider kinds the preset + callback are inert. Missing /
	// empty session_id falls through to fresh-session behavior.
	if plan.ProviderID == "claude-stream" {
		preset, err := s.Store.GetClaudeSessionID(plan.LogicalAgentID)
		if err == nil {
			req.ClaudeSessionIDPreset = preset
		}
		logicalAgentID := plan.LogicalAgentID
		store := s.Store
		req.OnClaudeSessionID = func(claudeSessionID string) {
			if err := store.SetClaudeSessionID(logicalAgentID, claudeSessionID); err != nil {
				log.Printf("claudestream: persist session_id for %q failed: %v", logicalAgentID, err)
			}
		}
	}
	if err := s.Runtime.Start(context.Background(), req); err != nil {
		return nil, err
	}

	return &Launched{
		SessionID: sessionID,
		Workspace: ws,
		Plan:      plan,
		Wait: func(ctx context.Context) (int, error) {
			return s.Runtime.WaitSession(ctx, sessionID)
		},
	}, nil
}

func (s *Service) ListSessions(opts store.ListSessionsOptions) ([]store.SessionRow, error) {
	return s.Store.ListSessions(opts)
}

func (s *Service) GetSession(id string) (*store.SessionRow, error) {
	return s.Store.GetSession(id)
}

func (s *Service) StopSession(id string) error {
	return s.Runtime.Stop(context.Background(), id)
}

// WaitSession blocks until the named session reaches a terminal state and
// returns its exit code. Thin wrapper over runtime.Manager.WaitSession so
// callers (including the daemon's HTTP handlers) don't reach past Service
// into the runtime package.
func (s *Service) WaitSession(ctx context.Context, id string) (int, error) {
	return s.Runtime.WaitSession(ctx, id)
}

// SendInput writes data to the named session's PTY. Thin wrapper over
// runtime.Manager.SendInput.
func (s *Service) SendInput(id string, data []byte) error {
	return s.Runtime.SendInput(id, data)
}

// ResizeSession forwards a (rows, cols) winsize update to the named
// session's PTY. Thin wrapper over runtime.Manager.Resize.
func (s *Service) ResizeSession(id string, rows, cols uint16) error {
	return s.Runtime.Resize(id, rows, cols)
}

// AttachSession streams the named session's live output to w until ctx is
// canceled or the session exits. sinceSeq is a byte-offset hint for
// resume; 0 means "replay full ring then go live" (pre-resume default).
// Thin wrapper over runtime.Manager.AttachWith.
func (s *Service) AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error {
	return s.Runtime.AttachWith(ctx, id, w, runtime.AttachOptions{SinceSeq: sinceSeq})
}

// AttachedClients reports the in-memory count of live attach subscribers for
// id, or 0 if the session is not currently registered in the runtime (e.g.,
// already exited).
func (s *Service) AttachedClients(id string) int {
	info, ok := s.Runtime.Get(id)
	if !ok {
		return 0
	}
	return info.AttachedClients
}

// seedLogicalAgents upserts a logical_agents row for every catalog agent.
// Returns the number of rows touched. Idempotent: re-running refreshes
// name/role and updated_at while preserving created_at per the Upsert
// contract. See ADR 0003.
func seedLogicalAgents(db *store.Store, agents map[string]config.Agent) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var n int
	for _, a := range agents {
		la := agent.LogicalAgent{
			ID:   a.ID,
			Name: a.Name,
			Role: strings.Join(a.Roles, ", "),
		}
		if err := db.UpsertLogicalAgent(la, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
