// Package goprovider adapts go-providers CLIAdapters to the agent-mux
// provider.Runtime / provider.Session contract.
//
// Architecture: the CLIAdapter (from github.com/hollis-labs/go-providers)
// handles binary detection and argument-building; agent-mux owns the
// subprocess lifecycle, raw-stdout fanout, and session-ID continuity —
// the same model as the claudestream adapter. Using the CLIAdapter layer
// means any go-providers CLI tool (Claude, Codex, Kiro, …) can be wired
// in without re-implementing subprocess plumbing per tool.
//
// # Wire format
//
// Subprocess stdout is streamed directly to opts.Fanout as raw NDJSON
// bytes — identical to claudestream. The TUI ChatScreen's
// pkg/claudestream.Scanner parses the same format, so goprovider sessions
// render identically to claudestream sessions.
package goprovider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/sandbox"
	"github.com/chrispian/agent-mux/pkg/claudestream"
)

// ErrTurnInFlight is returned by SendInput when the previous turn's
// subprocess is still running.
var ErrTurnInFlight = errors.New("goprovider: previous turn still in flight")

// Runtime wraps a go-providers CLIAdapter as an agent-mux provider.Runtime.
type Runtime struct {
	id      string
	adapter gop.CLIAdapter
	binary  string
}

// NewRuntime constructs a Runtime. binary is the resolved path to the CLI
// executable; pass "" to force a Prepare-time resolution via adapter.Detect().
func NewRuntime(id string, adapter gop.CLIAdapter, binary string) *Runtime {
	return &Runtime{id: id, adapter: adapter, binary: binary}
}

func (r *Runtime) ID() string                 { return r.id }
func (r *Runtime) Kind() provider.RuntimeKind { return provider.RuntimeKindCLI }

// Caps declares the goprovider adapter's capabilities. It is a turn-based
// CLI adapter with provider session ID continuity (ClaudeSessionIDPreset /
// OnClaudeSessionID hooks) and requires a binary to be present.
// No PTY, no resize.
func (r *Runtime) Caps() provider.Capabilities {
	return provider.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	}
}

// Prepare validates that the CLI binary is reachable. If binary was not
// set at construction time, Detect() is called here; the resolved path
// is stored on the Runtime for Start.
func (r *Runtime) Prepare(_ context.Context, _ *launch.Plan) error {
	if r.binary != "" {
		if _, err := os.Stat(r.binary); err != nil {
			return fmt.Errorf("goprovider %q: binary %q not found: %w", r.id, r.binary, err)
		}
		return nil
	}
	path, ok := r.adapter.Detect()
	if !ok {
		return fmt.Errorf("goprovider %q: %s CLI not found in PATH", r.id, r.adapter.Name())
	}
	r.binary = path
	return nil
}

// Start creates a new Session. The session is idle until the first
// SendInput call, which spawns the subprocess for that turn.
//
// When opts.BootMode is "agents_md" and opts.BootPrompt is non-empty, the
// boot prompt is written to AGENTS.md in opts.Workdir before any turn runs.
// This is the correct boot mechanism for providers (e.g. Codex) that read
// their system context from a file rather than a CLI flag. The file is only
// created if it does not already exist — an existing AGENTS.md is preserved.
func (r *Runtime) Start(_ context.Context, _ *launch.Plan, opts provider.StartOptions) (provider.Session, error) {
	if opts.BootMode == "agents_md" && opts.BootPrompt != "" {
		agentsPath := filepath.Join(opts.Workdir, "AGENTS.md")
		if _, err := os.Stat(agentsPath); os.IsNotExist(err) {
			if err := os.WriteFile(agentsPath, []byte(opts.BootPrompt), 0o600); err != nil { //nolint:gosec // G306: boot prompt written to session workdir
				return nil, fmt.Errorf("goprovider: write AGENTS.md: %w", err)
			}
		}
	}

	logF, err := os.Create(opts.LogPath) //nolint:gosec // G304: workspace-managed path
	if err != nil {
		return nil, fmt.Errorf("goprovider: open log %s: %w", opts.LogPath, err)
	}
	s := &Session{
		adapter:   r.adapter,
		binary:    r.binary,
		opts:      opts,
		logFile:   logF,
		stoppedCh: make(chan struct{}),
		// Pre-seed session_id for --resume if a prior session was checkpointed.
		sessionID: opts.ClaudeSessionIDPreset,
	}
	return s, nil
}

// Session drives one long-lived logical agent session over the
// go-providers CLIAdapter. Each SendInput call is a fresh turn: a new
// subprocess is spawned, its stdout tee'd to the fanout broker as raw
// NDJSON bytes. The session_id observed in a system/init event is
// preserved across turns for --resume continuity.
type Session struct {
	adapter gop.CLIAdapter
	binary  string
	opts    provider.StartOptions
	logFile *os.File

	mu        sync.Mutex
	sessionID string
	current   *exec.Cmd
	stopOnce  sync.Once
	stoppedCh chan struct{}
	stopped   bool
}

// SendInput runs one CLI turn. data is the user's message for this turn.
// The subprocess's stdout is written to opts.Fanout as raw NDJSON bytes;
// the session_id extracted from a system/init event updates internal state
// and calls opts.OnClaudeSessionID when set.
//
// Returns provider.ErrTurnInFlight if a prior turn is still running.
func (s *Session) SendInput(ctx context.Context, data []byte) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return provider.ErrNoInputChannel
	}
	if s.current != nil && s.current.ProcessState == nil {
		s.mu.Unlock()
		return ErrTurnInFlight
	}

	sessionID := s.sessionID

	systemPrompt := s.opts.BootPrompt
	args := s.adapter.BuildArgs(string(data), systemPrompt, sessionID)

	cmd := exec.CommandContext(ctx, s.binary, args...) //nolint:gosec // G204: catalog-sourced binary + adapter-built args
	cmd.Dir = s.opts.Workdir
	cmd.Env = os.Environ()

	if s.opts.Sandbox != nil {
		if err := sandbox.Apply(cmd, *s.opts.Sandbox, s.opts.Workdir); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("goprovider: sandbox: %w", err)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("goprovider: stdout pipe: %w", err)
	}
	cmd.Stderr = s.logFile

	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("goprovider: start: %w", err)
	}
	s.current = cmd
	s.mu.Unlock()

	// Stream stdout to fanout and extract session_id from system/init events.
	s.streamTurn(stdout)
	return cmd.Wait()
}

// streamTurn reads stdout lines, writes each line to the fanout, and
// extracts the session_id from any system/init event encountered.
func (s *Session) streamTurn(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Write raw NDJSON line to fanout (same format the TUI ChatScreen expects).
		if s.opts.Fanout != nil {
			_, _ = s.opts.Fanout.Write(append(line, '\n'))
		}
		// Also write to log file.
		if s.logFile != nil {
			_, _ = s.logFile.Write(append(line, '\n'))
		}
		// Extract session_id from system/init events.
		events, err := claudestream.Parse(line)
		if err != nil {
			continue
		}
		for _, ev := range events {
			if ev.Kind == claudestream.KindSessionID && ev.SessionID != "" {
				s.mu.Lock()
				s.sessionID = ev.SessionID
				cb := s.opts.OnClaudeSessionID
				s.mu.Unlock()
				if cb != nil {
					cb(ev.SessionID)
				}
			}
		}
	}
}

// Wait blocks until Stop is called. The session has no natural "end"
// independent of turns — it's active until explicitly stopped.
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
		close(s.stoppedCh)
	})
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
	return nil
}

// Health reports current liveness. PID is 0 — individual turns spawn
// transient subprocesses; the session itself has no persistent PID.
func (s *Session) Health() provider.HealthStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return provider.HealthStatus{Alive: false, PID: 0, State: provider.LiveStateStopped}
	}
	state := provider.LiveStateIdle
	if s.current != nil && s.current.ProcessState == nil {
		state = provider.LiveStateProcessing
	}
	return provider.HealthStatus{Alive: true, PID: 0, State: state}
}

// ProviderSessionID returns the provider session ID captured from the
// go-providers CLIAdapter's system/init event, or "" if no turn has run yet.
// Satisfies provider.SessionIDer (checked by Caps().ProviderSessionID == true).
func (s *Session) ProviderSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// CheckpointHints returns the current session_id as JSON for --resume
// continuity. The hints blob is round-tripped through the checkpoint
// payload and back into StartOptions.ClaudeSessionIDPreset on resume.
func (s *Session) CheckpointHints() (provider.CheckpointHint, bool) {
	return provider.CheckpointHint{}, false
}

// Resize is a no-op for the goprovider adapter — there is no PTY to resize.
func (s *Session) Resize(_ context.Context, _, _ uint16) error { return nil }

// Static interface checks.
var (
	_ provider.Runtime = (*Runtime)(nil)
	_ provider.Session = (*Session)(nil)
)
