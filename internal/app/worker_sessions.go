package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
	"github.com/hollis-labs/tether/internal/workspace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (s *Service) createWorkerSession(in CreateSessionInput) (*Launched, error) {
	if s.Workers == nil {
		return nil, fmt.Errorf("worker channel unavailable")
	}
	workerID, err := s.Workers.Resolve(context.Background(), in.Worker)
	if err != nil {
		return nil, err
	}
	if !s.Workers.Connected(workerID) {
		return nil, fmt.Errorf("worker offline")
	}
	var key *store.SessionIdempotency
	if in.IdempotencyKey != "" {
		principal := idempotencyPrincipal(in.IdempotencyPrincipal)
		unlock := s.lockIdempotencyKey(principal, in.IdempotencyKey)
		defer unlock()
		digest := createRequestDigest(in)
		if l, err := s.replayIfKeyed(principal, in.IdempotencyKey, store.IdempotencyOpCreate, digest); l != nil || err != nil {
			return l, err
		}
		key = &store.SessionIdempotency{PrincipalID: principal, Key: in.IdempotencyKey, Operation: store.IdempotencyOpCreate, RequestDigest: digest}
	}
	plan, err := s.BuildLaunchPlan(in)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	root := filepath.Join(config.ResolveWorkspaceRoot(s.Catalog.Global.Catalog.Defaults, s.Catalog.Paths), "worker-sessions")
	ws, err := workspace.Create(root, id, plan)
	if err != nil {
		return nil, err
	}
	row := store.SessionRow{ID: id, WorkerID: workerID, LaunchID: plan.LaunchID, ProjectID: plan.ProjectID, LogicalAgentID: plan.LogicalAgentID, ProviderID: plan.ProviderID, ProviderKind: "cli", Workspace: ws.Root, State: "created", Publication: "tether-hosted"}
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
	spec := workerwire.LaunchSpec{Session: id, Actor: registry.LogicalAgentBindingTarget(p.LogicalAgentID), Provider: brand, RuntimeKind: p.RuntimeKind, Args: append([]string(nil), p.Args...), Env: p.Env, BootPrompt: p.BootPrompt, BootMode: p.BootMode, BootFiles: p.BootDirOverlay, NativeFiles: p.NativeFiles, Workspace: workerwire.Workspace{Kind: "scratch"}}
	if strings.HasPrefix(p.RepoRoot, "https://") || strings.HasPrefix(p.RepoRoot, "git@") {
		spec.Workspace = workerwire.Workspace{Kind: "git", URL: p.RepoRoot}
	} else if p.RepoRoot != "" {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(probe, "git", "-C", p.RepoRoot, "remote", "get-url", "origin").Output()
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
				b, err := os.ReadFile(path)
				if err != nil {
					return spec, err
				}
				spec.BootFiles[name] = string(b)
			}
		}
	}
	var err error
	if p.SandboxProfile != "" {
		profile, ok, e := s.Catalog.SandboxProfile(p.CatalogAgent(), p.SandboxProfile)
		err = e
		if ok {
			spec.Sandbox = &profile
		}
	} else {
		profile, ok, e := s.Catalog.AgentSandbox(p.CatalogAgent())
		err = e
		if ok {
			spec.Sandbox = &profile
		}
	}
	return spec, err
}
func (s *Service) launchWorkerSession(ctx context.Context, row *store.SessionRow, p *launch.Plan) (*Launched, error) {
	spec, err := s.workerSpec(ctx, row.ID, p)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	if err = s.Store.UpdateSessionState(row.ID, "launching", 0, nil); err != nil {
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := s.Workers.Send(wait, row.WorkerID, workerwire.Frame{T: "launch", CmdID: "launch:" + row.ID, Session: row.ID, Spec: raw})
	if err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(row.ID, "failed", 0, &exit)
		return nil, err
	}
	if err = s.Store.UpdateSessionState(row.ID, "running", result.PID, nil); err != nil {
		return nil, err
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
