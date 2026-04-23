// Package opencode is the provider adapter that invokes the `opencode run`
// CLI with `--format json`. Like the claudestream adapter, each user turn
// spawns a fresh subprocess; session continuity is maintained via the
// `--session <id>` flag, with the session ID extracted from the top-level
// "sessionID" field that appears on every opencode JSON event.
//
// Boot prompt injection: on the first turn of a fresh session (no session ID
// yet), opts.BootPrompt is prepended to the user message so the agent
// receives its context before any conversation history is established.
// On resumed sessions (--session <id>), the prior context is already in
// opencode's stored history, so the boot prompt is not re-injected.
package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/sandbox"
)

// ErrTurnInFlight is returned by SendInput when the previous turn's
// subprocess is still running.
var ErrTurnInFlight = errors.New("opencode: previous turn still in flight")

// Adapter satisfies provider.Runtime for the opencode CLI.
type Adapter struct{}

func (Adapter) ID() string                 { return "opencode" }
func (Adapter) Kind() provider.RuntimeKind { return provider.RuntimeKindCLI }

// Caps declares the opencode adapter's capabilities. It is a turn-based
// CLI adapter with provider session ID continuity (--session flag) and
// requires the binary to be present. No PTY, no resize.
func (Adapter) Caps() provider.Capabilities {
	return provider.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	}
}

// Prepare validates that the catalog references a runnable binary.
func (Adapter) Prepare(_ context.Context, plan *launch.Plan) error {
	if plan.Command == "" {
		return fmt.Errorf("opencode: provider command empty")
	}
	return nil
}

// Start creates a Session ready to receive turns. No subprocess is spawned
// until the first SendInput call.
func (a Adapter) Start(_ context.Context, plan *launch.Plan, opts provider.StartOptions) (provider.Session, error) {
	if err := a.Prepare(context.Background(), plan); err != nil {
		return nil, err
	}
	logF, err := os.Create(opts.LogPath) //nolint:gosec // G304: workspace-managed path
	if err != nil {
		return nil, fmt.Errorf("opencode: open log: %w", err)
	}
	return &Session{
		plan:      plan,
		opts:      opts,
		logFile:   logF,
		stoppedCh: make(chan struct{}),
		// Pre-seed session ID for --session continuity if caller observed a
		// prior session from the durable store.
		sessionID: opts.ClaudeSessionIDPreset,
	}, nil
}

// Session drives one logical opencode session. Each SendInput spawns a fresh
// `opencode run --format json` subprocess; the session ID from the first
// JSON event is stored for --session continuity across turns.
type Session struct {
	plan    *launch.Plan
	opts    provider.StartOptions
	logFile *os.File

	mu        sync.Mutex
	sessionID string // opencode session ID for --session flag
	current   *exec.Cmd
	stopOnce  sync.Once
	stoppedCh chan struct{}
	stopped   bool
}

// Wait blocks until Stop is called.
func (s *Session) Wait() (int, error) {
	<-s.stoppedCh
	return 0, nil
}

// Stop kills any in-flight turn and unblocks Wait.
func (s *Session) Stop(_ context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		if s.current != nil && s.current.Process != nil {
			_ = s.current.Process.Kill()
		}
		s.mu.Unlock()
		_ = s.logFile.Close()
		close(s.stoppedCh)
	})
	return nil
}

// SendInput starts one opencode turn. The message in data is sent as a
// positional argument to `opencode run`. On the first turn of a fresh
// session, opts.BootPrompt (if set) is prepended to the message.
//
// Returns ErrTurnInFlight if a prior turn is still running.
func (s *Session) SendInput(ctx context.Context, data []byte) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return provider.ErrNoInputChannel
	}
	if s.current != nil {
		s.mu.Unlock()
		return ErrTurnInFlight
	}

	sessionID := s.sessionID

	// Build message: prepend boot prompt on the very first turn of a
	// fresh session. On resumed sessions the prior context is already
	// present in opencode's stored history.
	msg := string(data)
	if sessionID == "" && s.opts.BootPrompt != "" {
		msg = s.opts.BootPrompt + "\n\n" + msg
	}

	// Build args: plan.Args carries the subcommand (e.g. "run"); the
	// adapter appends --format json, optional --session, and the message.
	// Catalog convention: args: ["run"] for the standard run subcommand.
	args := append([]string{}, s.plan.Args...)
	args = append(args, "--format", "json")
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	args = append(args, msg)

	cmd := exec.CommandContext(ctx, s.plan.Command, args...) //nolint:gosec // G204: catalog-sourced binary
	cmd.Dir = s.opts.Workdir
	cmd.Env = provider.BuildEnv(s.plan.EnvMode, s.plan.EnvPassthrough, s.plan.EnvRedact, s.plan.Env, os.Environ())

	if s.opts.Sandbox != nil {
		if err := sandbox.Apply(cmd, *s.opts.Sandbox, s.opts.Workdir); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("opencode: sandbox: %w", err)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("opencode: stdout pipe: %w", err)
	}
	cmd.Stderr = s.logFile

	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("opencode: start: %w", err)
	}
	s.current = cmd
	s.mu.Unlock()

	go s.readTurn(stdout, cmd)
	return nil
}

// eventEnvelope holds the fields we need from any opencode JSON event line.
// Every event carries a top-level "sessionID" (e.g. "ses_...").
type eventEnvelope struct {
	SessionID string `json:"sessionID"`
}

// readTurn drains stdout line-by-line, forwarding to Fanout + log and
// extracting the session ID for --session continuity on the next turn.
func (s *Session) readTurn(stdout io.ReadCloser, cmd *exec.Cmd) {
	defer stdout.Close()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if s.opts.Fanout != nil {
			_, _ = s.opts.Fanout.Write(append(line, '\n'))
		}
		if s.logFile != nil {
			_, _ = s.logFile.Write(append(line, '\n'))
		}

		var ev eventEnvelope
		if err := json.Unmarshal(line, &ev); err != nil || ev.SessionID == "" {
			continue
		}

		s.mu.Lock()
		learned := ""
		if s.sessionID == "" {
			s.sessionID = ev.SessionID
			learned = ev.SessionID
		}
		cb := s.opts.OnClaudeSessionID
		s.mu.Unlock()

		// Fire persistence callback only when we observe a fresh ID.
		// A preset ID echoed back by opencode must not trigger a
		// redundant store write.
		if learned != "" && cb != nil {
			cb(learned)
		}
	}

	_ = cmd.Wait()

	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()
}

// Resize is a no-op — opencode run has no PTY to resize.
func (s *Session) Resize(_ context.Context, _, _ uint16) error { return nil }

// Health reports liveness and the PID of any in-flight turn subprocess.
func (s *Session) Health() provider.HealthStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return provider.HealthStatus{Alive: false, PID: 0, State: provider.LiveStateStopped}
	}
	pid := 0
	state := provider.LiveStateIdle
	if s.current != nil && s.current.Process != nil {
		pid = s.current.Process.Pid
		state = provider.LiveStateProcessing
	}
	return provider.HealthStatus{Alive: true, PID: pid, State: state}
}

// ProviderSessionID returns the opencode session ID observed on this
// session's first turn, or "" if no turn has run yet.
// Satisfies provider.SessionIDer (checked by Caps().ProviderSessionID == true).
func (s *Session) ProviderSessionID() string {
	return s.SessionID()
}

// CheckpointHints returns no hints; opencode session continuity is handled
// via the --session flag using the stored session ID, not generic checkpoints.
func (s *Session) CheckpointHints() (provider.CheckpointHint, bool) {
	return provider.CheckpointHint{}, false
}

// SessionID exposes the opencode session ID observed on this session's first
// turn, or "" if no turn has run yet. Useful for tests and inspection.
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Static interface checks.
var (
	_ provider.Runtime = Adapter{}
	_ provider.Session = (*Session)(nil)
)
