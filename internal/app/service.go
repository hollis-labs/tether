package app

import (
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/provider/claudecode"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/store"
	"github.com/chrispian/agent-mux/internal/workspace"
)

type Service struct {
	CatalogRoot string
	Catalog     *config.Catalog
	Store       *store.Store
	Providers   *provider.Registry

	// running/killing are accessed by the main goroutine (Launch/StopSession)
	// and the wait goroutine. v0 CLI is one-shot so the window is narrow, but
	// this is not safe under concurrent calls. TODO: add a sync.Mutex once the
	// service gains more than one caller (HTTP/TUI/workflow).
	running map[string]*session.Handle
	killing map[string]bool
	wg      sync.WaitGroup
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
		running:     map[string]*session.Handle{},
		killing:     map[string]bool{},
	}, nil
}

func (s *Service) Close() error {
	s.wg.Wait()
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
	Handle    *session.Handle
}

func (s *Service) Launch(launchID string, attach io.Writer) (*Launched, error) {
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
		_ = s.Store.UpdateSessionState(sessID, string(session.StateFailed), 0, intPtr(1))
		return nil, err
	}
	_ = s.Store.UpdateSessionState(sessID, string(session.StateLaunching), 0, nil)

	h, err := session.Start(cmd, ws.LogPath, plan.BootPrompt, plan.BootMode)
	if err != nil {
		_ = s.Store.UpdateSessionState(sessID, string(session.StateFailed), 0, intPtr(1))
		return nil, err
	}
	_ = s.Store.UpdateSessionState(sessID, string(session.StateRunning), h.Cmd.Process.Pid, nil)
	s.running[sessID] = h

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		code, _ := h.Wait()
		var state session.State
		switch {
		case s.killing[sessID]:
			state = session.StateKilled
		case code == 0:
			state = session.StateCompleted
		default:
			state = session.StateFailed
		}
		_ = s.Store.UpdateSessionState(sessID, string(state), h.Cmd.Process.Pid, &code)
		delete(s.running, sessID)
		delete(s.killing, sessID)
	}()

	return &Launched{SessionID: sessID, Workspace: ws, Plan: plan, Handle: h}, nil
}

func (s *Service) ListSessions() ([]store.SessionRow, error) {
	return s.Store.ListSessions()
}

func (s *Service) GetSession(id string) (*store.SessionRow, error) {
	return s.Store.GetSession(id)
}

func (s *Service) StopSession(id string) error {
	h, ok := s.running[id]
	if !ok {
		return fmt.Errorf("session %q not running locally", id)
	}
	s.killing[id] = true
	return h.Kill()
}

func intPtr(i int) *int { return &i }
