// Package acp runs ACP agents (Copilot, Pi) under Tether's daemon-owned
// session model by hosting go-agent-wrapper (CW-20260930-0106 stage 1).
//
// The agent is chosen by go-agent-wrapper's registry-driven launch.Select, so
// the runtime id and mode come from the go-providers registry rather than a
// Tether table. The wrapper owns the ACP client lifecycle (spawn, initialize,
// session/new, prompts, permission requests, shutdown); this package adapts
// one wrapper run to agentsessions.Runtime and agentsessions.Session so the
// Manager, attach and session.log work as they do for every other runtime.
//
// Native runtimes launch through agentkit. This host adapts ACP sessions only.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	wacp "github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/launch"
	"github.com/hollis-labs/go-agent-wrapper/wrapper"
	"github.com/hollis-labs/go-providers/registry"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Kind is the agentsessions.Runtime Kind of an ACP runtime. It is distinct
// from "cli" so the subprocess-runtime handling keyed on that kind (the
// per-turn session.log and turn_failed mapping) does not apply.
const Kind = "acp"

// readyTimeout bounds how long Start waits for the ACP session to be
// established (process spawned, initialize and session/new answered).
const readyTimeout = 60 * time.Second

// New returns a runtime for providerID that launches runtimeID in mode
// through launch.Select. binary pins the executable (the catalog's command);
// empty resolves it the registry's way (env override, then PATH).
func New(providerID, runtimeID string, mode runtimes.Mode, binary string) (agentsessions.Runtime, error) {
	d, ok := registry.Lookup(runtimeID)
	if !ok {
		return nil, fmt.Errorf("acp: runtime %q is not in the registry", runtimeID)
	}
	if !mode.ACP() {
		return nil, fmt.Errorf("acp: mode %q is not an ACP mode", mode)
	}
	return &acpRuntime{
		id:   providerID,
		desc: d,
		sel:  launch.Selection{Runtime: string(d.ID), Mode: mode, Binary: binary},
		caps: agentsessions.Capabilities{ProviderSessionID: true, BinaryRequired: true},
	}, nil
}

type acpRuntime struct {
	id       string
	desc     registry.Descriptor
	sel      launch.Selection
	caps     agentsessions.Capabilities
	observer func(runtimeevents.Event)
}

// SetEventObserver installs a synchronous observer before Start. It receives
// the wrapper's complete Activity feed, including events not rendered to logs.
func (r *acpRuntime) SetEventObserver(observer func(runtimeevents.Event)) { r.observer = observer }

func (r *acpRuntime) ID() string                       { return r.id }
func (r *acpRuntime) Kind() string                     { return Kind }
func (r *acpRuntime) Caps() agentsessions.Capabilities { return r.caps }

// Prepare checks that the agent's executable resolves and that the wrapper
// drives this (runtime, mode), so a launch fails before any session state.
func (r *acpRuntime) Prepare(_ context.Context) error {
	if r.sel.Binary != "" {
		// launch.Select takes an absolute path; a catalog may name the
		// command bare ("copilot").
		bin := r.sel.Binary
		if !filepath.IsAbs(bin) {
			p, err := exec.LookPath(bin)
			if err != nil {
				return fmt.Errorf("acp: %s binary %s: %w", r.desc.ID, bin, err)
			}
			if bin, err = filepath.Abs(p); err != nil {
				return fmt.Errorf("acp: %s binary %s: %w", r.desc.ID, p, err)
			}
			r.sel.Binary = bin
		}
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("acp: %s binary %s: %w", r.desc.ID, bin, err)
		}
	} else if _, err := r.desc.LookPath(); err != nil {
		return fmt.Errorf("acp: %s binary not found: %w", r.desc.ID, err)
	}
	if _, err := launch.Select(r.sel); err != nil {
		return fmt.Errorf("acp: %w", err)
	}
	return nil
}

func (r *acpRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	if opts.Workdir == "" {
		return nil, errors.New("acp: StartOptions.Workdir is required")
	}
	adapter, err := launch.Select(r.sel)
	if err != nil {
		return nil, fmt.Errorf("acp: %w", err)
	}
	out, err := openOutput(opts)
	if err != nil {
		return nil, err
	}
	out.observer = r.observer
	manager := wacp.NewManager()
	cfg := wrapper.Config{
		App:             "tether",
		Adapter:         adapter,
		Activity:        activity.NewBridge(out),
		Workdir:         opts.Workdir,
		WorkspaceDir:    opts.WorkspaceDir,
		LogPath:         opts.LogPath,
		SessionIDPreset: opts.SessionIDPreset,
		ACPManager:      manager,
		OnSessionID:     opts.OnSessionID,
		// Forwarded so the wrapper enforces them or refuses the launch
		// (ErrProtectedPathsUnsupported); it never runs the agent
		// unprotected. Tether refuses ACP launches itself while protection
		// is on (launch.ErrACPLaunchUnprotected, CW-20261001-0162), so this
		// is the backstop rather than the gate.
		ProtectedPaths: opts.ProtectedPaths,
	}
	if len(opts.Env) > 0 {
		cfg.Environment = wrapper.ChildEnvironment{Mode: wrapper.EnvironmentReplace, Set: append([]string(nil), opts.Env...)}
	}
	// The boot prompt is the session's kickoff, delivered as its first
	// prompt as the streaming-stdio runtime does.
	if opts.BootPrompt != "" && opts.BootMode != "none" {
		cfg.AutoFireFirstTurn = true
		cfg.FirstTurnPayload = opts.BootPrompt
	}
	w, err := wrapper.New(cfg)
	if err != nil {
		_ = out.Close()
		return nil, fmt.Errorf("acp: %w", err)
	}

	// The session outlives the launch request, so its run is not bound to
	// ctx; Stop ends it.
	runCtx, cancel := context.WithCancel(context.Background())
	s := &session{w: w, out: out, cancel: cancel, done: make(chan struct{})}
	go func() {
		err := w.Run(runCtx)
		s.finish(err)
	}()
	if err := s.waitEstablished(ctx); err != nil {
		cancel()
		<-s.done
		return nil, err
	}
	return s, nil
}

type session struct {
	w      *wrapper.Wrapper
	out    *output
	cancel context.CancelFunc

	done     chan struct{}
	runErr   error
	exitCode atomic.Int32
	stopped  atomic.Bool
}

func (s *session) finish(err error) {
	s.runErr = err
	if err != nil && !s.stopped.Load() {
		s.exitCode.Store(1)
		// The run's reason would otherwise be lost: Wait reports only the
		// exit code, and the wrapper's process.exited event is not part of
		// the transcript.
		log.Printf("acp: session ended: %v", err)
		s.out.writeText("\n[error] " + err.Error() + "\n")
	}
	_ = s.out.Close()
	close(s.done)
}

// waitEstablished returns once the ACP session is committed (the agent
// answered initialize and session/new) or the run has ended. A boot prompt
// may still be running then; that is the session's first turn.
func (s *session) waitEstablished(ctx context.Context) error {
	deadline := time.NewTimer(readyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, ok := s.w.ACPSnapshot(); ok {
			return nil
		}
		select {
		case <-s.done:
			if s.runErr != nil {
				return fmt.Errorf("acp: session ended before it was established: %w", s.runErr)
			}
			return errors.New("acp: session ended before it was established")
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("acp: session not established within %s", readyTimeout)
		case <-tick.C:
		}
	}
}

func (s *session) Wait() (int, error) {
	<-s.done
	return int(s.exitCode.Load()), nil
}

func (s *session) Stop(ctx context.Context) error {
	s.stopped.Store(true)
	err := s.w.Stop(ctx)
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return err
}

// SendInput sends data as one ACP prompt (session/prompt). It returns once
// the agent has accepted the prompt, not when the turn ends: the turn's end
// arrives as its own event ([turn_done] or [error] in the transcript).
func (s *session) SendInput(ctx context.Context, data []byte) error {
	select {
	case <-s.done:
		return agentsessions.ErrNoInputChannel
	default:
	}
	if err := s.w.SendInput(ctx, data); err != nil {
		if errors.Is(err, wrapper.ErrSessionNotStarted) {
			return agentsessions.ErrNoInputChannel
		}
		// The run is still up, so "not live" means busy (a turn in
		// flight) or still booting: a conflict with the session's state
		// the caller can retry, not a daemon fault.
		if errors.Is(err, wacp.ErrNotLive) {
			return fmt.Errorf("%w: %w", agentsessions.ErrTurnInFlight, err)
		}
		return err
	}
	return nil
}

func (s *session) Resize(context.Context, uint16, uint16) error { return nil }

// DeliveryCapabilities forwards the wrapper's real advertisement so callers
// can distinguish cancel-turn support from merely keeping the session alive.
func (s *session) DeliveryCapabilities() adapters.DeliveryCapabilities {
	return s.w.DeliveryCapabilities()
}

// InterruptTurn preserves the ACP session and uses the wrapper's existing
// session/cancel protocol. The manager-facing refusal is the native runtime's
// ErrInterruptUnsupported, rather than a wrapper-specific error.
func (s *session) InterruptTurn(ctx context.Context) error {
	if !s.DeliveryCapabilities().Supports(adapters.DeliveryCapabilityCancelTurn) {
		return agentsessions.ErrInterruptUnsupported
	}
	err := s.w.CancelTurn(ctx)
	if errors.Is(err, wrapper.ErrTurnCancelUnsupported) {
		return agentsessions.ErrInterruptUnsupported
	}
	return err
}

func (s *session) Health() agentsessions.HealthStatus {
	select {
	case <-s.done:
		return agentsessions.HealthStatus{State: agentsessions.LiveStateStopped}
	default:
	}
	st := agentsessions.HealthStatus{Alive: true, State: agentsessions.LiveStateIdle}
	if snap, ok := s.w.ACPSnapshot(); ok && snap.State != wacp.StateReady {
		st.State = agentsessions.LiveStateProcessing
	}
	return st
}

func (s *session) CheckpointHints() (agentsessions.CheckpointHint, bool) {
	return agentsessions.CheckpointHint{}, false
}

// ProviderSessionID is the agent's ACP session id.
func (s *session) ProviderSessionID() string { return s.w.ProviderSessionID() }

// output turns the wrapper's event stream into the session's readable
// output: logs/session.log (what `tether sessions tail` reads) and the attach
// fan-out. Every event kind is either rendered or deliberately dropped:
// the wrapper emits lifecycle, raw-IO, heartbeat and policy kinds that a
// reader of the transcript does not need, and new kinds may appear.
type output struct {
	mu       sync.Mutex
	f        *os.File
	fanout   io.Writer
	observer func(runtimeevents.Event)
}

func openOutput(opts agentsessions.StartOptions) (*output, error) {
	path := opts.LogPath
	if path == "" && opts.WorkspaceDir != "" {
		path = filepath.Join(opts.WorkspaceDir, "logs", "session.log")
	}
	o := &output{fanout: opts.Fanout}
	if path == "" {
		return o, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("acp: session log: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: workspace-managed log path
	if err != nil {
		return nil, fmt.Errorf("acp: session log: %w", err)
	}
	o.f = f
	return o, nil
}

// Write implements runtimeevents.Sink. It never fails the wrapper's emit.
func (o *output) Write(_ context.Context, ev runtimeevents.Event) error {
	if o.observer != nil {
		o.observer(ev)
	}
	o.writeText(render(ev))
	return nil
}

func (o *output) writeText(text string) {
	if text == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f != nil {
		_, _ = io.WriteString(o.f, text)
	}
	if o.fanout != nil {
		_, _ = io.WriteString(o.fanout, text)
	}
}

func (o *output) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f == nil {
		return nil
	}
	err := o.f.Close()
	o.f = nil
	return err
}

// render maps one wrapper event to transcript text, in the shape the other
// runtimes' attach output uses: reply text as-is, then bracketed markers.
// Thought/thinking deltas (phase "thought" since go-agent-wrapper v0.17.0,
// "thinking" before) are dropped: they are not part of the reply.
func render(ev runtimeevents.Event) string {
	switch ev.Kind {
	case runtimeevents.KindAgentDelta:
		var p struct {
			Content  string `json:"content"`
			Phase    string `json:"phase"`
			Thinking bool   `json:"thinking"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.Thinking || p.Phase == "thought" {
			return ""
		}
		return p.Content
	case runtimeevents.KindAgentToolUse:
		var p struct {
			ToolUse struct {
				Name  string `json:"name"`
				Title string `json:"title"`
			} `json:"tool_use"`
			Title string `json:"title"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		name := firstNonEmpty(p.ToolUse.Name, p.ToolUse.Title, p.Title, "tool")
		return "\n[tool_use:" + name + "]\n"
	case runtimeevents.KindTurnCompleted:
		return "\n[turn_done]\n"
	case runtimeevents.KindTurnFailed:
		var p struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		return "\n[error] " + firstNonEmpty(p.Error, p.Message, "turn failed") + "\n"
	// The three kinds go-agent-wrapper v0.17.0 added, in the format
	// agentkit's native path writes for the same events
	// (agentsessions/from_adapter.go).
	case runtimeevents.KindAgentPermissionDenied:
		var p struct {
			Action      string `json:"action"`
			DisplayName string `json:"display_name"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		return fmt.Sprintf("\n[permission_denied:%s] %s\n", p.Action, p.DisplayName)
	case runtimeevents.KindSessionAuthFailed:
		var p struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		return fmt.Sprintf("\n[auth_failed] %s\n", p.Error)
	case runtimeevents.KindSessionLost:
		var p struct {
			RequestedID string `json:"requested_id"`
			ActualID    string `json:"actual_id"`
			Reason      string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		return fmt.Sprintf("\n[session_lost] requested=%s actual=%s: %s\n", p.RequestedID, p.ActualID, p.Reason)
	default:
		return ""
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
