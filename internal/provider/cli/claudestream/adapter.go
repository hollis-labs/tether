// Package claudestream is the provider adapter that invokes the
// `claude` CLI with `--output-format stream-json --verbose --print`.
// Unlike the PTY-backed `claudecode` adapter, this one runs each
// user turn as a fresh subprocess and surfaces structured events
// rather than raw PTY bytes. See ADR 0017 for the event-routing
// rationale and `pkg/claudestream/README.md` for the parser's
// extractability posture.
package claudestream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/sandbox"
	"github.com/chrispian/agent-mux/pkg/claudestream"
)

// Adapter satisfies provider.Runtime. ID is "claude-stream" to make
// it visually distinct from the PTY adapter's "claude-code".
type Adapter struct{}

func (Adapter) ID() string                 { return "claude-stream" }
func (Adapter) Kind() provider.RuntimeKind { return provider.RuntimeKindCLI }

// Caps declares the claudestream adapter's capabilities. It is a turn-based
// CLI adapter with provider session ID continuity (--resume) and requires the
// binary to be present. No PTY, no resize.
func (Adapter) Caps() provider.Capabilities {
	return provider.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	}
}

// Prepare validates that the catalog references a binary we can run.
// Same shape as claudecode's Prepare for consistency.
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
	// Open the session log here so the existing attach/tail-log
	// paths see the JSON event stream on disk too. Same convention
	// as session.Start uses for PTY sessions.
	logF, err := os.Create(opts.LogPath) //nolint:gosec // G304: workspace-managed path
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	s := &Session{
		plan:      plan,
		opts:      opts,
		logFile:   logF,
		stoppedCh: make(chan struct{}),
		// Resume continuity (T-v004-s02-05): if the caller observed a
		// prior session_id via the durable store, preload it so the
		// very first turn runs with `--resume <id>`. Empty string falls
		// back to the fresh-session path.
		sessionID: opts.ClaudeSessionIDPreset,
	}
	return s, nil
}

// Session owns one long-lived logical claude session. Each user turn
// spawns a fresh `claude -p <prompt>` subprocess; the session_id
// returned in the first `system/init` event is persisted locally for
// subsequent turns' `--resume` flag. Idle between turns — no child
// process runs unless SendInput launches one.
type Session struct {
	plan    *launch.Plan
	opts    provider.StartOptions
	logFile *os.File

	mu        sync.Mutex
	sessionID string    // claude CLI session_id for --resume
	current   *exec.Cmd // nil between turns
	stopOnce  sync.Once
	stoppedCh chan struct{}
	stopped   bool
}

// Wait blocks until Stop is called. Turn-based providers have no
// natural "end" — the session is active until explicitly stopped.
// Returns exit code 0 on clean stop.
func (s *Session) Wait() (int, error) {
	<-s.stoppedCh
	return 0, nil
}

// Stop kills any in-flight turn and signals Wait to return.
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

// ErrTurnInFlight is returned by SendInput when a previous turn
// hasn't finished yet. Callers should wait for the done event on the
// attach stream before sending another.
var ErrTurnInFlight = errors.New("claudestream: previous turn still running")

// SendInput starts a fresh claude run with `data` as the user prompt
// (plain-text). Returns ErrTurnInFlight if a turn is already active.
//
// The new subprocess emits stream-json events on stdout; each line is
// forwarded verbatim to opts.Fanout (so attach clients see the raw
// NDJSON stream) and to the session log. The first `system/init`
// event's session_id is captured for the next turn's `--resume`.
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

	// Build args: required flags, optional --resume, plan.Args last,
	// then the prompt via -p. Order matters for claude CLI — the -p
	// value must not be positionally confused with a subcommand.
	args := append([]string{}, s.plan.Args...)
	args = append(args, "--print", "--output-format", "stream-json", "--verbose")
	if s.sessionID != "" {
		args = append(args, "--resume", s.sessionID)
	}
	args = append(args, "-p", string(data))

	cmd := exec.CommandContext(ctx, s.plan.Command, args...) //nolint:gosec // G204: plan.Command is catalog-sourced
	cmd.Dir = s.opts.Workdir
	cmd.Env = provider.BuildEnv(s.plan.EnvMode, s.plan.EnvPassthrough, s.plan.EnvRedact, s.plan.Env, os.Environ())

	if s.opts.Sandbox != nil {
		if err := sandbox.Apply(cmd, *s.opts.Sandbox, s.opts.Workdir); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("claudestream: sandbox: %w", err)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("claudestream: stdout pipe: %w", err)
	}
	// Swallow stderr into the log file so claude's debug spam doesn't
	// escape. Attach clients only see structured events on stdout.
	cmd.Stderr = s.logFile

	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("claudestream: start: %w", err)
	}
	s.current = cmd
	s.mu.Unlock()

	go s.readTurn(stdout, cmd)
	return nil
}

// readTurn runs per-turn. Drains stdout line-by-line, forwards to
// Fanout + log, captures session_id. Returns when the subprocess
// closes stdout (claude emits `result` then exits).
func (s *Session) readTurn(stdout io.ReadCloser, cmd *exec.Cmd) {
	defer stdout.Close()

	scanner := bufio.NewScanner(stdout)
	// claude can emit ~100KB assistant blocks; bump the default 64KB cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Fanout + log: preserve newline delimiters.
		if s.opts.Fanout != nil {
			_, _ = s.opts.Fanout.Write(append(line, '\n'))
		}
		if s.logFile != nil {
			_, _ = s.logFile.Write(append(line, '\n'))
		}
		// Parse locally to capture session_id for --resume.
		events, err := claudestream.Parse(line)
		if err != nil {
			continue
		}
		for _, ev := range events {
			if ev.Kind != claudestream.KindSessionID || ev.SessionID == "" {
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
			// Fire the persistence callback only when we observed a
			// fresh id (preset carried from the durable store stays
			// write-through-free — claude echoes it back and this
			// avoids redundant store writes).
			if learned != "" && cb != nil {
				cb(learned)
			}
		}
	}

	_ = cmd.Wait()

	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()
}

// Resize is a no-op for claudestream — there's no PTY to resize.
// Satisfies the ADR 0014 contract.
func (s *Session) Resize(_ context.Context, _, _ uint16) error { return nil }

// Health reports liveness based on whether a turn is currently
// running (PID of the live child) or idle (session is alive with no
// child).
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

// ProviderSessionID returns the claude CLI session_id observed on this
// session's first turn, or "" if no turn has run yet.
// Satisfies provider.SessionIDer (checked by Caps().ProviderSessionID == true).
func (s *Session) ProviderSessionID() string {
	return s.SessionID()
}

// CheckpointHints — no-op in v0.0.4. Claude has its own session_id
// continuity (see SendInput's --resume logic) that's separate from
// agent-mux's generic checkpoint mechanism. A future extension could
// expose the session_id as a hint for cross-provider handoff.
func (s *Session) CheckpointHints() (provider.CheckpointHint, bool) {
	return provider.CheckpointHint{}, false
}

// SessionID is a test/inspection hook. Exposes the CLI session_id
// agent-mux has observed on this session's first turn, or "" if no
// turn has run yet.
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
