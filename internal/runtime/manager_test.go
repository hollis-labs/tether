package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/workspace"
)

type fakeHandle struct {
	pid    int
	done   chan struct{}
	once   sync.Once
	code   atomic.Int32
	killed atomic.Bool
}

func newFakeHandle(pid int) *fakeHandle {
	return &fakeHandle{pid: pid, done: make(chan struct{})}
}

func (f *fakeHandle) Wait() (int, error) {
	<-f.done
	return int(f.code.Load()), nil
}

func (f *fakeHandle) Kill() error {
	f.killed.Store(true)
	f.code.Store(-1)
	f.once.Do(func() { close(f.done) })
	return nil
}

func (f *fakeHandle) PID() int { return f.pid }

func (f *fakeHandle) complete(code int) {
	f.code.Store(int32(code))
	f.once.Do(func() { close(f.done) })
}

type fakeStarter struct {
	mu      sync.Mutex
	handles map[string]*fakeHandle
	nextPID int
	err     error
}

func (f *fakeStarter) Start(cmd *exec.Cmd, logPath, bootPrompt, bootMode string) (Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.handles == nil {
		f.handles = map[string]*fakeHandle{}
	}
	f.nextPID++
	h := newFakeHandle(1000 + f.nextPID)
	f.handles[logPath] = h
	return h, nil
}

func (f *fakeStarter) get(logPath string) *fakeHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handles[logPath]
}

type transition struct {
	id, state string
	pid       int
	exit      *int
}

type fakeSink struct {
	mu          sync.Mutex
	transitions []transition
}

func (f *fakeSink) UpdateSessionState(id, state string, pid int, exit *int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, transition{id, state, pid, exit})
	return nil
}

func (f *fakeSink) has(id, state string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.transitions {
		if t.id == id && t.state == state {
			return true
		}
	}
	return false
}

func (f *fakeSink) snapshot() []transition {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]transition, len(f.transitions))
	copy(out, f.transitions)
	return out
}

func newRequest(id string) StartRequest {
	return StartRequest{
		ID: id,
		Plan: &launch.Plan{
			LaunchID:   "demo",
			ProjectID:  "p1",
			AgentID:    "a1",
			ProviderID: "pr1",
		},
		Workspace: &workspace.Session{
			ID:      id,
			Root:    "/tmp/ws/" + id,
			LogPath: "/tmp/ws/" + id + "/session.log",
		},
		Cmd: &exec.Cmd{Path: "/bin/true"},
	}
}

func TestManager_StartRegistersSession(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}

	info, ok := m.Get("s1")
	if !ok {
		t.Fatal("expected session to be registered")
	}
	if info.ID != "s1" {
		t.Errorf("info.ID = %q, want s1", info.ID)
	}
	if info.State != session.StateRunning {
		t.Errorf("info.State = %q, want running", info.State)
	}
	if info.ProjectID != "p1" || info.AgentID != "a1" || info.ProviderID != "pr1" {
		t.Errorf("info IDs not propagated from plan: %+v", info)
	}
	if info.PID == 0 {
		t.Error("info.PID = 0, expected non-zero from fake handle")
	}
	if info.Workspace != "/tmp/ws/s1" {
		t.Errorf("info.Workspace = %q", info.Workspace)
	}

	list := m.List()
	if len(list) != 1 {
		t.Errorf("List len = %d, want 1", len(list))
	}

	if !sink.has("s1", string(session.StateLaunching)) {
		t.Errorf("missing launching transition: %+v", sink.snapshot())
	}
	if !sink.has("s1", string(session.StateRunning)) {
		t.Errorf("missing running transition: %+v", sink.snapshot())
	}

	starter.get(req.Workspace.LogPath).complete(0)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestManager_WaitMarksCompletedOnCleanExit(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	starter.get(req.Workspace.LogPath).complete(0)

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !sink.has("s1", string(session.StateCompleted)) {
		t.Errorf("expected completed; got %+v", sink.snapshot())
	}
	if _, ok := m.Get("s1"); ok {
		t.Error("session should be unregistered after exit")
	}
}

func TestManager_WaitMarksFailedOnNonzeroExit(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	_ = m.Start(context.Background(), req)
	starter.get(req.Workspace.LogPath).complete(7)
	_ = m.Shutdown(context.Background())

	if !sink.has("s1", string(session.StateFailed)) {
		t.Errorf("expected failed; got %+v", sink.snapshot())
	}
}

func TestManager_StopKillsHandleAndMarksKilled(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := m.Stop(context.Background(), "s1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	h := starter.get(req.Workspace.LogPath)
	if !h.killed.Load() {
		t.Error("handle was not killed")
	}
	if !sink.has("s1", string(session.StateKilled)) {
		t.Errorf("expected killed; got %+v", sink.snapshot())
	}
	if _, ok := m.Get("s1"); ok {
		t.Error("session should be unregistered after kill")
	}
}

func TestManager_StopNonexistentReturnsError(t *testing.T) {
	m := NewManager(&fakeSink{}, &fakeStarter{})
	err := m.Stop(context.Background(), "nope")
	if !errors.Is(err, ErrSessionNotRunning) {
		t.Errorf("expected ErrSessionNotRunning; got %v", err)
	}
}

func TestManager_StartAfterShutdownRejected(t *testing.T) {
	m := NewManager(&fakeSink{}, &fakeStarter{})
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := m.Start(context.Background(), newRequest("x"))
	if !errors.Is(err, ErrManagerStopped) {
		t.Errorf("expected ErrManagerStopped; got %v", err)
	}
}

func TestManager_StartFailurePersistsFailedState(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{err: errors.New("pty boom")}
	m := NewManager(sink, starter)

	err := m.Start(context.Background(), newRequest("s1"))
	if err == nil {
		t.Fatal("expected error from starter")
	}
	if !sink.has("s1", string(session.StateFailed)) {
		t.Errorf("expected failed transition; got %+v", sink.snapshot())
	}
	if _, ok := m.Get("s1"); ok {
		t.Error("failed Start should not register session")
	}
}

func TestManager_ShutdownWaitsForInFlight(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- m.Shutdown(context.Background()) }()

	select {
	case <-done:
		t.Fatal("Shutdown returned before session completed")
	case <-time.After(50 * time.Millisecond):
	}

	starter.get(req.Workspace.LogPath).complete(0)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after session completed")
	}
}

func TestManager_ShutdownRespectsContextCancel(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	_ = m.Start(context.Background(), req)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Shutdown(ctx) }()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled; got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Shutdown did not honor ctx cancellation")
	}

	starter.get(req.Workspace.LogPath).complete(0)
}

func TestManager_ShutdownIsIdempotent(t *testing.T) {
	m := NewManager(&fakeSink{}, &fakeStarter{})
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown returned %v; expected nil", err)
	}
}

func TestManager_WaitSessionReturnsExitCode(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		starter.get(req.Workspace.LogPath).complete(42)
	}()

	code, err := m.WaitSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("WaitSession: %v", err)
	}
	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
	_ = m.Shutdown(context.Background())
}

func TestManager_WaitSessionAfterExitReturnsCached(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	starter.get(req.Workspace.LogPath).complete(0)
	// Let watch goroutine finish before calling WaitSession.
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	code, err := m.WaitSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("WaitSession after exit: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestManager_WaitSessionUnknownReturnsError(t *testing.T) {
	m := NewManager(&fakeSink{}, &fakeStarter{})
	if _, err := m.WaitSession(context.Background(), "nope"); !errors.Is(err, ErrSessionNotRunning) {
		t.Errorf("expected ErrSessionNotRunning; got %v", err)
	}
}

func TestManager_WaitSessionRespectsCtxCancel(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	req := newRequest("s1")
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.WaitSession(ctx, "s1")
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected Canceled; got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WaitSession did not honor ctx cancellation")
	}

	starter.get(req.Workspace.LogPath).complete(0)
	_ = m.Shutdown(context.Background())
}

func TestManager_ConcurrentStartStopNoRace(t *testing.T) {
	sink := &fakeSink{}
	starter := &fakeStarter{}
	m := NewManager(sink, starter)

	const N = 20
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		ids[i] = fmt.Sprintf("s%d", i)
	}

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		id := ids[i]
		go func() {
			defer wg.Done()
			if err := m.Start(context.Background(), newRequest(id)); err != nil {
				t.Errorf("Start %s: %v", id, err)
			}
		}()
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		wg.Add(1)
		i, id := i, ids[i]
		go func() {
			defer wg.Done()
			req := newRequest(id)
			if i%2 == 0 {
				_ = m.Stop(context.Background(), id)
				return
			}
			if h := starter.get(req.Workspace.LogPath); h != nil {
				h.complete(0)
			}
		}()
	}
	wg.Wait()

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(m.List()); got != 0 {
		t.Errorf("expected empty List after shutdown; got %d", got)
	}
}
