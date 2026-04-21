package claudecode

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/sandbox"
	"github.com/chrispian/agent-mux/internal/session"
)

// Adapter is the CLI-runtime implementation for the Claude Code provider.
// It satisfies provider.Runtime; Start returns a *cliSession that satisfies
// provider.Session over a PTY-backed *session.Handle.
type Adapter struct{}

func (Adapter) ID() string                 { return "claude-code" }
func (Adapter) Kind() provider.RuntimeKind { return provider.RuntimeKindCLI }

// Prepare validates the plan is runnable before the manager provisions a
// workspace. Surfacing errors here (vs. inside Start) lets the caller abort
// cleanly without leaving half-created state.
func (Adapter) Prepare(_ context.Context, plan *launch.Plan) error {
	if plan.Command == "" {
		return fmt.Errorf("provider command empty")
	}
	return nil
}

func (a Adapter) Start(ctx context.Context, plan *launch.Plan, opts provider.StartOptions) (provider.Session, error) {
	if err := a.Prepare(ctx, plan); err != nil {
		return nil, err
	}
	// Command + args come from the trusted catalog (operator-authored
	// YAML), not from an HTTP/user boundary.
	cmd := exec.Command(plan.Command, plan.Args...) //nolint:gosec // G204: catalog-sourced command, not untrusted input

	cmd.Dir = opts.Workdir
	cmd.Env = provider.BuildEnv(plan.EnvMode, plan.EnvPassthrough, plan.EnvRedact, plan.Env, os.Environ())

	if opts.Sandbox != nil {
		if err := sandbox.Apply(cmd, *opts.Sandbox, opts.Workdir); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
	}

	h, err := session.Start(cmd, opts.LogPath, opts.BootPrompt, opts.BootMode, opts.Fanout)
	if err != nil {
		return nil, err
	}
	return &cliSession{handle: h}, nil
}

// cliSession wraps a *session.Handle to satisfy provider.Session. It keeps
// PTY internals encapsulated so the runtime manager can treat CLI and API
// runtimes identically.
type cliSession struct {
	handle *session.Handle
}

func (s *cliSession) Wait() (int, error)           { return s.handle.Wait() }
func (s *cliSession) Stop(_ context.Context) error { return s.handle.Kill() }

func (s *cliSession) SendInput(_ context.Context, data []byte) error {
	w := s.handle.PTYWriter()
	if w == nil {
		return provider.ErrNoInputChannel
	}
	_, err := w.Write(data)
	return err
}

func (s *cliSession) Resize(_ context.Context, rows, cols uint16) error {
	return s.handle.Resize(rows, cols)
}

func (s *cliSession) Health() provider.HealthStatus {
	return provider.HealthStatus{Alive: true, PID: s.handle.PID()}
}

// CheckpointHints is a no-op for the CLI runtime in v0.0.2. The checkpoint
// shape will be pinned in v0.0.3 alongside Sprint v003-04.
func (s *cliSession) CheckpointHints() (provider.CheckpointHint, bool) {
	return provider.CheckpointHint{}, false
}

// Static interface checks. If these fail to compile, the Runtime / Session
// contracts have drifted and the CLI adapter must be updated.
var (
	_ provider.Runtime = Adapter{}
	_ provider.Session = (*cliSession)(nil)
)
