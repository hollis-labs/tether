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
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/workspace"
)

// StateSink persists session state transitions. The production implementation
// is *store.Store; tests use an in-memory fake.
type StateSink interface {
	UpdateSessionState(id, state string, pid int, exit *int) error
}

// AttachmentSink persists the lifecycle of a client attach subscription.
// Implemented by *store.Store. Nil is safe: Manager.Attach works without
// persistence (only the in-memory attachCount and broker subscription run).
type AttachmentSink interface {
	CreateClientAttachment(id, sessionID, clientKind, attachedAt string) error
	DetachClientAttachment(id, detachedAt string) error
}

// StartRequest bundles the inputs for launching a session under Manager
// ownership. The caller (typically app.Service) resolves the plan, provisions
// the workspace, and picks the provider Runtime; Manager owns the Session's
// registration, state transitions, and watch goroutine.
type StartRequest struct {
	ID        string
	Plan      *launch.Plan
	Workspace *workspace.Session
	Runtime   provider.Runtime
}

// SessionInfo is the public snapshot of a registered session. It deliberately
// excludes the raw provider.Session so callers cannot reach into runtime
// internals.
type SessionInfo struct {
	ID              string
	PID             int
	State           session.State
	LaunchID        string
	ProjectID       string
	AgentID         string
	ProviderID      string
	Workspace       string
	AttachedClients int
}

// Manager owns the registry of running sessions and their lifecycle
// transitions.
type Manager struct {
	sink       StateSink
	attachSink AttachmentSink
	nowFn      func() time.Time
	idFn       func() string

	mu       sync.RWMutex
	registry map[string]*entry
	results  map[string]*sessionResult
	stopped  bool

	wg sync.WaitGroup
}

type entry struct {
	info        SessionInfo
	sess        provider.Session
	broker      *attachBroker
	killing     bool
	attachCount int
	// inputMu serialises SendInput writes so concurrent callers never
	// interleave partial writes on the session's input channel.
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
)

// NewManager constructs a Manager. attachSink is optional; pass nil to
// disable client_attachments persistence (useful for tests that don't care
// about the durability path).
func NewManager(sink StateSink) *Manager {
	return &Manager{
		sink:     sink,
		nowFn:    time.Now,
		idFn:     uuid.NewString,
		registry: map[string]*entry{},
		results:  map[string]*sessionResult{},
	}
}

// WithAttachmentSink returns m with the attachment persistence sink set. It
// is safe to call on a freshly-constructed Manager before any Start.
func (m *Manager) WithAttachmentSink(sink AttachmentSink) *Manager {
	m.attachSink = sink
	return m
}

// Start launches a session under Manager ownership. It records state
// transitions (launching → running), registers the session, and spawns a
// watch goroutine that will record the terminal state when the session exits.
//
// Errors from the provider Runtime are recorded as StateFailed before
// returning. Start returns ErrManagerStopped if called after Shutdown.
func (m *Manager) Start(ctx context.Context, req StartRequest) error {
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
	sess, err := req.Runtime.Start(ctx, req.Plan, provider.StartOptions{
		Workdir:    req.Plan.RepoRoot,
		LogPath:    req.Workspace.LogPath,
		BootPrompt: req.Plan.BootPrompt,
		BootMode:   req.Plan.BootMode,
		Fanout:     broker,
	})
	if err != nil {
		broker.close()
		exit := 1
		_ = m.sink.UpdateSessionState(req.ID, string(session.StateFailed), 0, &exit)
		return err
	}

	pid := sess.Health().PID
	if err := m.sink.UpdateSessionState(req.ID, string(session.StateRunning), pid, nil); err != nil {
		_ = sess.Stop(ctx)
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
		_ = sess.Stop(ctx)
		return ErrManagerStopped
	}
	m.registry[req.ID] = &entry{info: info, sess: sess, broker: broker}
	m.results[req.ID] = &sessionResult{done: make(chan struct{})}
	m.wg.Add(1)
	m.mu.Unlock()

	go m.watch(req.ID, sess, broker)
	return nil
}

// watch blocks on the session's Wait, records the terminal state, unregisters
// the entry, closes the attach broker so subscribers drain cleanly, and
// delivers the exit code to any WaitSession callers.
func (m *Manager) watch(id string, sess provider.Session, broker *attachBroker) {
	defer m.wg.Done()
	code, _ := sess.Wait()

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
	_ = m.sink.UpdateSessionState(id, string(state), sess.Health().PID, &code)

	if broker != nil {
		broker.close()
	}

	if result != nil {
		result.exitCode = code
		close(result.done)
	}
}

// Stop signals the named session to terminate. It returns after the session's
// Stop has been invoked; the watch goroutine records the terminal state
// asynchronously.
func (m *Manager) Stop(ctx context.Context, id string) error {
	m.mu.Lock()
	e, ok := m.registry[id]
	if !ok {
		m.mu.Unlock()
		return ErrSessionNotRunning
	}
	e.killing = true
	sess := e.sess
	m.mu.Unlock()
	return sess.Stop(ctx)
}

// Get returns a snapshot for a registered session.
func (m *Manager) Get(id string) (SessionInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.registry[id]
	if !ok {
		return SessionInfo{}, false
	}
	info := e.info
	info.AttachedClients = e.attachCount
	return info, true
}

// List returns snapshots of all currently registered sessions. The result is
// a defensive copy; callers may mutate it freely.
func (m *Manager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SessionInfo, 0, len(m.registry))
	for _, e := range m.registry {
		info := e.info
		info.AttachedClients = e.attachCount
		out = append(out, info)
	}
	return out
}

// SendInput writes data to the named session's input channel. Concurrent
// SendInput callers on the same session are serialised through a per-entry
// lock so no two callers can interleave partial writes. Returns
// ErrSessionNotRunning if the session is not registered, or whatever error
// the Session.SendInput surfaces (e.g. provider.ErrNoInputChannel if the
// session's input channel is no longer available).
func (m *Manager) SendInput(id string, data []byte) error {
	m.mu.RLock()
	e, ok := m.registry[id]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotRunning
	}
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	return e.sess.SendInput(context.Background(), data)
}

// AttachOptions controls optional metadata attached to a subscription. The
// zero value picks a default client_kind.
type AttachOptions struct {
	ClientKind string
}

// Attach subscribes w to the named session's live output stream. Attach
// writes any recent history (tail replay) to w first, then streams live
// output until ctx is cancelled or the session exits. Multiple concurrent
// Attach callers on the same session are supported; one detaching does not
// affect the others and does not kill the session.
//
// Returns ErrSessionNotRunning if the session is not registered (already
// exited or never started).
func (m *Manager) Attach(ctx context.Context, id string, w io.Writer) error {
	return m.AttachWith(ctx, id, w, AttachOptions{})
}

// AttachWith is Attach with caller-controlled attachment metadata. It
// records the attachment in the attachSink (if configured) and bumps the
// in-memory attached-clients counter for the session, then calls back into
// the broker's subscribe/stream flow.
func (m *Manager) AttachWith(ctx context.Context, id string, w io.Writer, opts AttachOptions) error {
	m.mu.RLock()
	e, ok := m.registry[id]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotRunning
	}

	kind := opts.ClientKind
	if kind == "" {
		kind = "cli"
	}
	attachID := m.idFn()
	now := m.nowFn().UTC().Format(time.RFC3339)

	if m.attachSink != nil {
		_ = m.attachSink.CreateClientAttachment(attachID, id, kind, now)
	}

	m.mu.Lock()
	e.attachCount++
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		if e.attachCount > 0 {
			e.attachCount--
		}
		m.mu.Unlock()
		if m.attachSink != nil {
			_ = m.attachSink.DetachClientAttachment(attachID, m.nowFn().UTC().Format(time.RFC3339))
		}
	}()

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
