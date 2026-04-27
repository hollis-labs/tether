package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	"github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/session"
)

// New constructs an agentsessions.Runtime backed by a PTY-spawned claude
// CLI subprocess. plan is baked into the Runtime; per-launch StartOptions
// supply workspace, log path, boot prompt, sandbox profile, and fanout.
//
// claudecode is the only mux adapter that takes the PTY path —
// claudestream and opencode run subprocess-per-turn via the lib's
// NewFromAdapter. PTY state crosses turn boundaries and can't be modeled
// as a stateless go-providers CLIAdapter.
func New(plan *launch.Plan) (agentsessions.Runtime, error) {
	if plan == nil {
		return nil, errors.New("claudecode: plan is required")
	}
	return &runtime{plan: plan}, nil
}

type runtime struct {
	plan *launch.Plan
}

func (r *runtime) ID() string   { return "claude-code" }
func (r *runtime) Kind() string { return "cli" }
func (r *runtime) Caps() agentsessions.Capabilities {
	return agentsessions.Capabilities{
		PTY:               true,
		Resize:            true,
		ProviderSessionID: false,
		CheckpointResume:  false,
		BinaryRequired:    true,
	}
}

func (r *runtime) Prepare(_ context.Context) error {
	if r.plan.Command == "" {
		return errors.New("claudecode: plan.Command empty")
	}
	return nil
}

func (r *runtime) Start(_ context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	if r.plan.Command == "" {
		return nil, errors.New("claudecode: plan.Command empty")
	}
	cmd := exec.Command(r.plan.Command, r.plan.Args...) //nolint:gosec // G204: catalog-sourced command
	cmd.Dir = opts.Workdir
	// opts.Env is the env policy already applied by service.go's call
	// to provider.BuildEnv at LaunchSession time. Falling back to
	// os.Environ() keeps tests / direct callers (compliance harness)
	// usable without manually composing env.
	if len(opts.Env) > 0 {
		cmd.Env = opts.Env
	} else {
		cmd.Env = os.Environ()
	}

	var sandboxCleanup func()
	if opts.Profile.ID != "" {
		cleanup, err := sandbox.Apply(cmd, opts.Profile, opts.Workdir)
		if err != nil {
			return nil, fmt.Errorf("claudecode: sandbox: %w", err)
		}
		sandboxCleanup = cleanup
	}

	h, err := session.Start(cmd, opts.LogPath, opts.BootPrompt, opts.BootMode, opts.Fanout)
	if err != nil {
		if sandboxCleanup != nil {
			sandboxCleanup()
		}
		return nil, err
	}
	return &ptySession{handle: h, sandboxCleanup: sandboxCleanup}, nil
}

// ptySession wraps a *session.Handle to satisfy agentsessions.Session.
// It keeps PTY internals encapsulated so the Manager treats CLI and API
// runtimes identically.
type ptySession struct {
	handle             *session.Handle
	mu                 sync.Mutex
	stopped            bool
	stopOnce           sync.Once
	sandboxCleanup     func()
	sandboxCleanupOnce sync.Once
}

func (s *ptySession) doSandboxCleanup() {
	s.sandboxCleanupOnce.Do(func() {
		if s.sandboxCleanup != nil {
			s.sandboxCleanup()
		}
	})
}

func (s *ptySession) Wait() (int, error) {
	code, err := s.handle.Wait()
	s.doSandboxCleanup()
	return code, err
}

func (s *ptySession) Stop(_ context.Context) error {
	var killErr error
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		killErr = s.handle.Kill()
	})
	s.doSandboxCleanup()
	return killErr
}

func (s *ptySession) SendInput(_ context.Context, data []byte) error {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		return agentsessions.ErrNoInputChannel
	}
	if _, err := s.handle.Write(data); err != nil {
		if session.IsPTYClosed(err) {
			return agentsessions.ErrNoInputChannel
		}
		return err
	}
	return nil
}

func (s *ptySession) Resize(_ context.Context, rows, cols uint16) error {
	return s.handle.Resize(rows, cols)
}

func (s *ptySession) Health() agentsessions.HealthStatus {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		return agentsessions.HealthStatus{Alive: false, PID: 0, State: agentsessions.LiveStateStopped}
	}
	// PTY sessions stay in LiveStateIdle while alive — the PTY is
	// continuously active; there is no discrete "turn" concept here.
	return agentsessions.HealthStatus{Alive: true, PID: s.handle.PID(), State: agentsessions.LiveStateIdle}
}

func (s *ptySession) CheckpointHints() (agentsessions.CheckpointHint, bool) {
	return nil, false
}

var (
	_ agentsessions.Runtime = (*runtime)(nil)
	_ agentsessions.Session = (*ptySession)(nil)
)
