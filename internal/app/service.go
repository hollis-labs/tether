package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/provider/claudecode"
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
	return &Service{
		CatalogRoot: catalogRoot,
		Catalog:     cat,
		Store:       db,
		Providers:   reg,
		Runtime:     runtime.NewManager(db, nil),
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
	Wait func(ctx context.Context) (int, error)
}

func (s *Service) Launch(launchID string) (*Launched, error) {
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

	adapter, ok := s.Providers.Get(plan.ProviderID)
	if !ok {
		return nil, fmt.Errorf("no adapter for provider %q", plan.ProviderID)
	}

	row := store.SessionRow{
		ID:         sessID,
		LaunchID:   plan.LaunchID,
		ProjectID:  plan.ProjectID,
		AgentID:    plan.AgentID,
		ProviderID: plan.ProviderID,
		Workspace:  ws.Root,
		State:      string(session.StateCreated),
	}
	if err := s.Store.CreateSession(row, plan); err != nil {
		return nil, err
	}

	cmd, err := adapter.Build(plan, plan.RepoRoot)
	if err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(sessID, string(session.StateFailed), 0, &exit)
		return nil, err
	}

	req := runtime.StartRequest{
		ID:         sessID,
		Plan:       plan,
		Workspace:  ws,
		Cmd:        cmd,
		BootPrompt: plan.BootPrompt,
		BootMode:   plan.BootMode,
	}
	if err := s.Runtime.Start(context.Background(), req); err != nil {
		return nil, err
	}

	return &Launched{
		SessionID: sessID,
		Workspace: ws,
		Plan:      plan,
		Wait: func(ctx context.Context) (int, error) {
			return s.Runtime.WaitSession(ctx, sessID)
		},
	}, nil
}

func (s *Service) ListSessions() ([]store.SessionRow, error) {
	return s.Store.ListSessions()
}

func (s *Service) GetSession(id string) (*store.SessionRow, error) {
	return s.Store.GetSession(id)
}

func (s *Service) StopSession(id string) error {
	return s.Runtime.Stop(context.Background(), id)
}
