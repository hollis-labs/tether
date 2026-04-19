// Package runtime owns the registry of active session handles and their
// lifecycle. It is the long-lived counterpart to internal/app, which remains
// a thin composition root. The Manager is safe for concurrent use: all
// registry access is guarded by a sync.RWMutex and lifecycle goroutines
// cooperate via a WaitGroup so Shutdown can block until terminal states are
// recorded.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/workspace"
)

// StateSink persists session state transitions. The production implementation
// is *store.Store; tests use an in-memory fake.
type StateSink interface {
	UpdateSessionState(id, state string, pid int, exit *int) error
}

// Handle is the minimum contract Manager needs from a running session.
// *session.Handle implements it.
type Handle interface {
	Wait() (int, error)
	Kill() error
	PID() int
	PTYWriter() io.Writer
}

// Starter produces a Handle from a command + log path + boot prompt.
// DefaultStarter wraps session.Start; tests inject fakes. fanout, if non-nil,
// receives a copy of PTY output; Manager uses it to route live bytes to
// attach subscribers.
type Starter interface {
	Start(cmd *exec.Cmd, logPath, bootPrompt, bootMode string, fanout io.Writer) (Handle, error)
}

// DefaultStarter wraps session.Start so Manager does not depend directly on
// PTY internals.
type DefaultStarter struct{}

// Start satisfies Starter by delegating to session.Start.
func (DefaultStarter) Start(cmd *exec.Cmd, logPath, bootPrompt, bootMode string, fanout io.Writer) (Handle, error) {
	return session.Start(cmd, logPath, bootPrompt, bootMode, fanout)
}

// StartRequest bundles the inputs for launching a session under Manager
// ownership. The caller (typically app.Service) resolves the plan, provisions
// the workspace, and builds the provider command; Manager owns PTY start,
// state transitions, and the watch goroutine.
type StartRequest struct {
	ID         string
	Plan       *launch.Plan
	Workspace  *workspace.Session
	Cmd        *exec.Cmd
	BootPrompt string
	BootMode   string
}

// SessionInfo is the public snapshot of a registered session. It deliberately
// excludes the raw *session.Handle so callers cannot reach into PTY internals.
type SessionInfo struct {
	ID         string
	PID        int
	State      session.State
	LaunchID   string
	ProjectID  string
	AgentID    string
	ProviderID string
	Workspace  string
}

// Manager owns the registry of running sessions and their lifecycle
// transitions.
type Manager struct {
	sink    StateSink
	starter Starter

	mu       sync.RWMutex
	registry map[string]*entry
	results  map[string]*sessionResult
	stopped  bool

	wg sync.WaitGroup
}

type entry struct {
	info    SessionInfo
	handle  Handle
	broker  *attachBroker
	killing bool
	// inputMu serialises SendInput writes so concurrent callers never
	// interleave partial writes on the PTY master.
	inputMu sync.Mutex
}

// sessionResult holds the exit code of a terminated session. done is closed
// once the watch goroutine records the terminal state; exitCode is safe to
// read after that (close is a synchronization barrier).
type sessionResult struct {
	done     chan struct{}
	exitCode int
}

// Sentinel errors returned by Manager methods.
var (
	ErrManagerStopped    = errors.New("runtime manager is stopped")
	ErrSessionNotRunning = errors.New("session not running")
	ErrNoPTYWriter       = errors.New("session has no PTY writer")
)

// NewManager constructs a Manager. If starter is nil, DefaultStarter is used.
func NewManager(sink StateSink, starter Starter) *Manager {
	if starter == nil {
		starter = DefaultStarter{}
	}
	return &Manager{
		sink:     sink,
		starter:  starter,
		registry: map[string]*entry{},
		results:  map[string]*sessionResult{},
	}
}

// Start launches a session under Manager ownership. It records state
// transitions (launching → running), registers the handle, and spawns a
// watch goroutine that will record the terminal state when the process exits.
//
// Errors from the starter are recorded as StateFailed before returning.
// Start returns ErrManagerStopped if called after Shutdown.
func (m *Manager) Start(_ context.Context, req StartRequest) error {
	m.mu.RLock()
	stopped := m.stopped
	m.mu.RUnlock()
	if stopped {
		return ErrManagerStopped
	}

	if err := m.sink.UpdateSessionState(req.ID, string(session.StateLaunching), 0, nil); err != nil {
		return fmt.Errorf("mark launching: %w", err)
	}

	broker := newAttachBroker(defaultRingBytes, defaultSubscriberDepth)
	h, err := m.starter.Start(req.Cmd, req.Workspace.LogPath, req.BootPrompt, req.BootMode, broker)
	if err != nil {
		broker.close()
		exit := 1
		_ = m.sink.UpdateSessionState(req.ID, string(session.StateFailed), 0, &exit)
		return err
	}

	pid := h.PID()
	if err := m.sink.UpdateSessionState(req.ID, string(session.StateRunning), pid, nil); err != nil {
		_ = h.Kill()
		return fmt.Errorf("mark running: %w", err)
	}

	info := SessionInfo{
		ID:         req.ID,
		PID:        pid,
		State:      session.StateRunning,
		LaunchID:   req.Plan.LaunchID,
		ProjectID:  req.Plan.ProjectID,
		AgentID:    req.Plan.AgentID,
		ProviderID: req.Plan.ProviderID,
		Workspace:  req.Workspace.Root,
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		broker.close()
		_ = h.Kill()
		return ErrManagerStopped
	}
	m.registry[req.ID] = &entry{info: info, handle: h, broker: broker}
	m.results[req.ID] = &sessionResult{done: make(chan struct{})}
	m.wg.Add(1)
	m.mu.Unlock()

	go m.watch(req.ID, h, broker)
	return nil
}

// watch blocks on the handle's Wait, records the terminal state, unregisters
// the entry, closes the attach broker so subscribers drain cleanly, and
// delivers the exit code to any WaitSession callers.
func (m *Manager) watch(id string, h Handle, broker *attachBroker) {
	defer m.wg.Done()
	code, _ := h.Wait()

	m.mu.Lock()
	var killing bool
	if e, ok := m.registry[id]; ok {
		killing = e.killing
		delete(m.registry, id)
	}
	result := m.results[id]
	m.mu.Unlock()

	var state session.State
	switch {
	case killing:
		state = session.StateKilled
	case code == 0:
		state = session.StateCompleted
	default:
		state = session.StateFailed
	}
	_ = m.sink.UpdateSessionState(id, string(state), h.PID(), &code)

	if broker != nil {
		broker.close()
	}

	if result != nil {
		result.exitCode = code
		close(result.done)
	}
}

// Stop signals the named session's handle to terminate. It returns after Kill
// has been invoked; the watch goroutine records the terminal state
// asynchronously.
func (m *Manager) Stop(_ context.Context, id string) error {
	m.mu.Lock()
	e, ok := m.registry[id]
	if !ok {
		m.mu.Unlock()
		return ErrSessionNotRunning
	}
	e.killing = true
	h := e.handle
	m.mu.Unlock()
	return h.Kill()
}

// Get returns a snapshot for a registered session.
func (m *Manager) Get(id string) (SessionInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.registry[id]
	if !ok {
		return SessionInfo{}, false
	}
	return e.info, true
}

// List returns snapshots of all currently registered sessions. The result is
// a defensive copy; callers may mutate it freely.
func (m *Manager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SessionInfo, 0, len(m.registry))
	for _, e := range m.registry {
		out = append(out, e.info)
	}
	return out
}

// SendInput writes data to the named session's PTY. Concurrent SendInput
// callers on the same session are serialised through a per-entry lock so
// no two callers can interleave partial writes. Returns ErrSessionNotRunning
// if the session is not registered, or ErrNoPTYWriter if the handle does
// not expose a writable PTY (terminal or stubbed session).
func (m *Manager) SendInput(id string, data []byte) error {
	m.mu.RLock()
	e, ok := m.registry[id]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotRunning
	}
	w := e.handle.PTYWriter()
	if w == nil {
		return ErrNoPTYWriter
	}
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	_, err := w.Write(data)
	return err
}

// Attach subscribes w to the named session's live output stream. Attach
// writes any recent history (tail replay) to w first, then streams live PTY
// output until ctx is cancelled or the session exits. Multiple concurrent
// Attach callers on the same session are supported; one detaching does not
// affect the others and does not kill the session.
//
// Returns ErrSessionNotRunning if the session is not registered (already
// exited or never started).
func (m *Manager) Attach(ctx context.Context, id string, w io.Writer) error {
	m.mu.RLock()
	e, ok := m.registry[id]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotRunning
	}
	replay, ch, cancel := e.broker.subscribe(defaultSubscriberDepth)
	defer cancel()
	return copyStream(ctx, w, replay, ch)
}

// WaitSession blocks until the named session's watch goroutine records its
// terminal state and returns the exit code. It returns ErrSessionNotRunning
// if the session was never registered (or its result has been reaped); in
// that case, query the store for the historical exit code.
func (m *Manager) WaitSession(ctx context.Context, id string) (int, error) {
	m.mu.RLock()
	r, ok := m.results[id]
	m.mu.RUnlock()
	if !ok {
		return 0, ErrSessionNotRunning
	}
	select {
	case <-r.done:
		return r.exitCode, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Shutdown marks the Manager stopped (rejecting further Start calls) and
// blocks until all in-flight watch goroutines have returned or ctx is done.
// Subsequent Shutdown calls are no-ops.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
