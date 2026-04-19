package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/workspace"
)

// fakeSession is a manually-driven provider.Session. Tests use complete(code)
// to unblock Wait and emit(bytes) to push PTY-like output through the
// Fanout writer recorded at Start time.
type fakeSession struct {
	pid    int
	done   chan struct{}
	once   sync.Once
	code   atomic.Int32
	killed atomic.Bool
	fanout io.Writer
	// inputSink, if non-nil, receives SendInput bytes. Nil inputSink
	// returns provider.ErrNoInputChannel — the manager surfaces that as-is.
	inputSink io.Writer
}

func newFakeSession(pid int) *fakeSession {
	return &fakeSession{pid: pid, done: make(chan struct{})}
}

func (f *fakeSession) Wait() (int, error) {
	<-f.done
	return int(f.code.Load()), nil
}

func (f *fakeSession) Stop(_ context.Context) error {
	f.killed.Store(true)
	f.code.Store(-1)
	f.once.Do(func() { close(f.done) })
	return nil
}

func (f *fakeSession) SendInput(_ context.Context, data []byte) error {
	if f.inputSink == nil {
		return provider.ErrNoInputChannel
	}
	_, err := f.inputSink.Write(data)
	return err
}

func (f *fakeSession) Health() provider.HealthStatus {
	return provider.HealthStatus{Alive: true, PID: f.pid}
}

func (f *fakeSession) CheckpointHints() (provider.CheckpointHint, bool) {
	return provider.CheckpointHint{}, false
}

func (f *fakeSession) complete(code int) {
	f.code.Store(int32(code))
	f.once.Do(func() { close(f.done) })
}

// emit simulates the session writing output bytes; they route through the
// fanout writer the Runtime was given at Start so Manager.Attach subscribers
// see the data.
func (f *fakeSession) emit(p []byte) (int, error) {
	if f.fanout == nil {
		return 0, nil
	}
	return f.fanout.Write(p)
}

// fakeRuntime satisfies provider.Runtime. It records the Session it creates
// keyed by LogPath so tests can reach in and drive lifecycle events on a
// specific session.
type fakeRuntime struct {
	mu       sync.Mutex
	sessions map[string]*fakeSession
	nextPID  int
	err      error
}

func (r *fakeRuntime) ID() string                 { return "fake" }
func (r *fakeRuntime) Kind() provider.RuntimeKind { return provider.RuntimeKindCLI }
func (r *fakeRuntime) Prepare(_ context.Context, _ *launch.Plan) error {
	return nil
}

func (r *fakeRuntime) Start(_ context.Context, _ *launch.Plan, opts provider.StartOptions) (provider.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	if r.sessions == nil {
		r.sessions = map[string]*fakeSession{}
	}
	r.nextPID++
	s := newFakeSession(1000 + r.nextPID)
	s.fanout = opts.Fanout
	r.sessions[opts.LogPath] = s
	return s, nil
}

func (r *fakeRuntime) get(logPath string) *fakeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[logPath]
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

// newRequestFor is the per-request constructor used throughout the test
// suite. Each StartRequest carries the fakeRuntime so the manager dispatches
// Start into it and the test can look up the resulting fakeSession via
// runtime.get(logPath).
func newRequestFor(id string, rt provider.Runtime) StartRequest {
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
		Runtime: rt,
	}
}

func TestManager_StartRegistersSession(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
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
		t.Error("info.PID = 0, expected non-zero from fake session")
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

	rt.get(req.Workspace.LogPath).complete(0)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestManager_WaitMarksCompletedOnCleanExit(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rt.get(req.Workspace.LogPath).complete(0)

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
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	_ = m.Start(context.Background(), req)
	rt.get(req.Workspace.LogPath).complete(7)
	_ = m.Shutdown(context.Background())

	if !sink.has("s1", string(session.StateFailed)) {
		t.Errorf("expected failed; got %+v", sink.snapshot())
	}
}

func TestManager_StopKillsSessionAndMarksKilled(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := m.Stop(context.Background(), "s1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	s := rt.get(req.Workspace.LogPath)
	if !s.killed.Load() {
		t.Error("session was not killed")
	}
	if !sink.has("s1", string(session.StateKilled)) {
		t.Errorf("expected killed; got %+v", sink.snapshot())
	}
	if _, ok := m.Get("s1"); ok {
		t.Error("session should be unregistered after kill")
	}
}

func TestManager_StopNonexistentReturnsError(t *testing.T) {
	m := NewManager(&fakeSink{})
	err := m.Stop(context.Background(), "nope")
	if !errors.Is(err, ErrSessionNotRunning) {
		t.Errorf("expected ErrSessionNotRunning; got %v", err)
	}
}

func TestManager_StartAfterShutdownRejected(t *testing.T) {
	m := NewManager(&fakeSink{})
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := m.Start(context.Background(), newRequestFor("x", &fakeRuntime{}))
	if !errors.Is(err, ErrManagerStopped) {
		t.Errorf("expected ErrManagerStopped; got %v", err)
	}
}

func TestManager_StartFailurePersistsFailedState(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{err: errors.New("boom")}
	m := NewManager(sink)

	err := m.Start(context.Background(), newRequestFor("s1", rt))
	if err == nil {
		t.Fatal("expected error from runtime.Start")
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
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
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

	rt.get(req.Workspace.LogPath).complete(0)

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
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
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

	rt.get(req.Workspace.LogPath).complete(0)
}

func TestManager_ShutdownIsIdempotent(t *testing.T) {
	m := NewManager(&fakeSink{})
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown returned %v; expected nil", err)
	}
}

func TestManager_WaitSessionReturnsExitCode(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		rt.get(req.Workspace.LogPath).complete(42)
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
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rt.get(req.Workspace.LogPath).complete(0)
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
	m := NewManager(&fakeSink{})
	if _, err := m.WaitSession(context.Background(), "nope"); !errors.Is(err, ErrSessionNotRunning) {
		t.Errorf("expected ErrSessionNotRunning; got %v", err)
	}
}

func TestManager_WaitSessionRespectsCtxCancel(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
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

	rt.get(req.Workspace.LogPath).complete(0)
	_ = m.Shutdown(context.Background())
}

// TestManager_InterleavedLifecycleNoRace is the T-v002-s01-04 regression
// gate. It holds Start, Stop, Get, and List calls on the same set of IDs
// behind a shared barrier so they actually collide with each other — and
// with the watch goroutines — at launch time rather than the gentler
// Start-then-Stop fan-out TestManager_ConcurrentStartStopNoRace exercises.
// Removing the mutex from runtime.Manager causes `go test -race` on this
// test to report WARNING: DATA RACE (sanity-checked by hand during T-04).
func TestManager_InterleavedLifecycleNoRace(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	const N = 30
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		ids[i] = fmt.Sprintf("lc%d", i)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < N; i++ {
		wg.Add(1)
		id := ids[i]
		go func() {
			defer wg.Done()
			<-start
			_ = m.Start(context.Background(), newRequestFor(id, rt))
		}()
	}

	for i := 0; i < N; i++ {
		wg.Add(1)
		id := ids[i]
		go func() {
			defer wg.Done()
			<-start
			_ = m.Stop(context.Background(), id)
		}()
	}

	for i := 0; i < N; i++ {
		wg.Add(1)
		id := ids[i]
		go func() {
			defer wg.Done()
			<-start
			for k := 0; k < 5; k++ {
				_, _ = m.Get(id)
				_ = m.List()
			}
		}()
	}

	for i := 0; i < N; i++ {
		wg.Add(1)
		id := ids[i]
		go func() {
			defer wg.Done()
			<-start
			req := newRequestFor(id, rt)
			for attempt := 0; attempt < 20; attempt++ {
				if s := rt.get(req.Workspace.LogPath); s != nil {
					s.complete(0)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	close(start)
	wg.Wait()

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := len(m.List()); got != 0 {
		t.Errorf("expected empty List after shutdown; got %d", got)
	}
}

func TestManager_SendInputUnknownSessionErrors(t *testing.T) {
	m := NewManager(&fakeSink{})
	if err := m.SendInput("nope", []byte("hi")); !errors.Is(err, ErrSessionNotRunning) {
		t.Errorf("expected ErrSessionNotRunning; got %v", err)
	}
}

func TestManager_SendInputNoInputChannelErrors(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// fakeSession with no inputSink returns provider.ErrNoInputChannel.
	if err := m.SendInput("s1", []byte("hi")); !errors.Is(err, provider.ErrNoInputChannel) {
		t.Errorf("expected provider.ErrNoInputChannel; got %v", err)
	}
	rt.get(req.Workspace.LogPath).complete(0)
	_ = m.Shutdown(context.Background())
}

func TestManager_SendInputDeliversBytes(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var tsb threadsafeBuffer
	rt.get(req.Workspace.LogPath).inputSink = &tsb

	if err := m.SendInput("s1", []byte("hello\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if got := tsb.String(); got != "hello\n" {
		t.Errorf("input sink received %q, want %q", got, "hello\n")
	}
	rt.get(req.Workspace.LogPath).complete(0)
	_ = m.Shutdown(context.Background())
}

func TestManager_SendInputSerialisesConcurrentWrites(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	aw := newAtomicWriter(t)
	rt.get(req.Workspace.LogPath).inputSink = aw

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.SendInput("s1", []byte("payloadpayloadpayload\n")); err != nil {
				t.Errorf("SendInput: %v", err)
			}
		}()
	}
	wg.Wait()
	rt.get(req.Workspace.LogPath).complete(0)
	_ = m.Shutdown(context.Background())
}

// atomicWriter verifies that no two Write calls overlap in time. It is used
// to prove the per-entry inputMu actually serialises concurrent SendInput.
type atomicWriter struct {
	t    *testing.T
	busy atomic.Bool
}

func newAtomicWriter(t *testing.T) *atomicWriter { return &atomicWriter{t: t} }

func (a *atomicWriter) Write(p []byte) (int, error) {
	if !a.busy.CompareAndSwap(false, true) {
		a.t.Errorf("concurrent Write detected — inputMu did not serialise")
		return 0, errors.New("concurrent write")
	}
	// Hold the "busy" flag briefly to widen the race window.
	time.Sleep(100 * time.Microsecond)
	a.busy.Store(false)
	return len(p), nil
}

// fakeAttachSink captures CreateClientAttachment / DetachClientAttachment
// calls so tests can assert attach lifecycle persistence.
type fakeAttachSink struct {
	mu       sync.Mutex
	attached map[string]fakeAttachRow
}

type fakeAttachRow struct {
	sessionID  string
	clientKind string
	attachedAt string
	detachedAt string
}

func newFakeAttachSink() *fakeAttachSink {
	return &fakeAttachSink{attached: map[string]fakeAttachRow{}}
}

func (f *fakeAttachSink) CreateClientAttachment(id, sessionID, clientKind, attachedAt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached[id] = fakeAttachRow{sessionID: sessionID, clientKind: clientKind, attachedAt: attachedAt}
	return nil
}

func (f *fakeAttachSink) DetachClientAttachment(id, detachedAt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.attached[id]
	if !ok {
		return nil
	}
	row.detachedAt = detachedAt
	f.attached[id] = row
	return nil
}

func (f *fakeAttachSink) rows() []fakeAttachRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeAttachRow, 0, len(f.attached))
	for _, r := range f.attached {
		out = append(out, r)
	}
	return out
}

func TestManager_AttachPersistsLifecycleThroughSink(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	attachSink := newFakeAttachSink()
	m := NewManager(sink).WithAttachmentSink(attachSink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s := rt.get(req.Workspace.LogPath)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.AttachWith(ctx, "s1", io.Discard, AttachOptions{ClientKind: "http-api"}) }()

	deadline := time.After(500 * time.Millisecond)
	for {
		if len(attachSink.rows()) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("attach row never created; rows=%+v", attachSink.rows())
		case <-time.After(10 * time.Millisecond):
		}
	}

	row := attachSink.rows()[0]
	if row.sessionID != "s1" || row.clientKind != "http-api" {
		t.Errorf("row = %+v, want sessionID=s1 clientKind=http-api", row)
	}
	if row.detachedAt != "" {
		t.Errorf("row detachedAt should be empty while attached, got %q", row.detachedAt)
	}

	info, ok := m.Get("s1")
	if !ok || info.AttachedClients != 1 {
		t.Errorf("SessionInfo.AttachedClients = %d (ok=%v), want 1", info.AttachedClients, ok)
	}

	cancel()
	<-done

	for {
		row := attachSink.rows()[0]
		if row.detachedAt != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("detach never stamped: %+v", row)
		case <-time.After(10 * time.Millisecond):
		}
	}

	s.complete(0)
	_ = m.Shutdown(context.Background())
}

func TestManager_AttachCounterSurvivesConcurrentClients(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	const N = 5
	done := make(chan struct{}, N)
	for i := 0; i < N; i++ {
		go func() {
			_ = m.Attach(ctx, "s1", io.Discard)
			done <- struct{}{}
		}()
	}

	deadline := time.After(500 * time.Millisecond)
	for {
		info, _ := m.Get("s1")
		if info.AttachedClients == N {
			break
		}
		select {
		case <-deadline:
			info, _ := m.Get("s1")
			t.Fatalf("AttachedClients = %d, want %d", info.AttachedClients, N)
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	for i := 0; i < N; i++ {
		<-done
	}
	info, _ := m.Get("s1")
	if info.AttachedClients != 0 {
		t.Errorf("AttachedClients after detach = %d, want 0", info.AttachedClients)
	}

	rt.get(req.Workspace.LogPath).complete(0)
	_ = m.Shutdown(context.Background())
}

func TestManager_AttachReceivesLiveBytes(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s := rt.get(req.Workspace.LogPath)

	var buf threadsafeBuffer
	attachErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { attachErr <- m.Attach(ctx, "s1", &buf) }()

	time.Sleep(20 * time.Millisecond)
	if _, err := s.emit([]byte("live-output")); err != nil {
		t.Fatalf("emit: %v", err)
	}

	deadline := time.After(1 * time.Second)
	for {
		if buf.String() == "live-output" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("buf = %q, want %q", buf.String(), "live-output")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	if err := <-attachErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Attach err = %v", err)
	}

	s.complete(0)
	_ = m.Shutdown(context.Background())
}

func TestManager_AttachMultipleSubscribersBothReceive(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s := rt.get(req.Workspace.LogPath)

	var buf1, buf2 threadsafeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { _ = m.Attach(ctx, "s1", &buf1); done <- struct{}{} }()
	go func() { _ = m.Attach(ctx, "s1", &buf2); done <- struct{}{} }()

	time.Sleep(30 * time.Millisecond)
	if _, err := s.emit([]byte("broadcast")); err != nil {
		t.Fatalf("emit: %v", err)
	}

	deadline := time.After(1 * time.Second)
	for {
		if buf1.String() == "broadcast" && buf2.String() == "broadcast" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("buf1=%q buf2=%q, want both %q", buf1.String(), buf2.String(), "broadcast")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-done
	<-done
	s.complete(0)
	_ = m.Shutdown(context.Background())
}

func TestManager_AttachUnknownSessionReturnsError(t *testing.T) {
	m := NewManager(&fakeSink{})
	err := m.Attach(context.Background(), "nope", &threadsafeBuffer{})
	if !errors.Is(err, ErrSessionNotRunning) {
		t.Errorf("expected ErrSessionNotRunning; got %v", err)
	}
}

func TestManager_AttachSurvivesSiblingDetach(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s := rt.get(req.Workspace.LogPath)

	var survivor threadsafeBuffer
	ctxSurvivor, cancelSurvivor := context.WithCancel(context.Background())
	ctxDropped, cancelDropped := context.WithCancel(context.Background())
	go func() { _ = m.Attach(ctxSurvivor, "s1", &survivor) }()
	go func() { _ = m.Attach(ctxDropped, "s1", io.Discard) }()

	time.Sleep(30 * time.Millisecond)
	cancelDropped()
	time.Sleep(10 * time.Millisecond)

	if _, err := s.emit([]byte("after-drop")); err != nil {
		t.Fatalf("emit: %v", err)
	}

	deadline := time.After(1 * time.Second)
	for survivor.String() != "after-drop" {
		select {
		case <-deadline:
			t.Fatalf("survivor = %q, want %q", survivor.String(), "after-drop")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancelSurvivor()
	s.complete(0)
	_ = m.Shutdown(context.Background())
}

func TestManager_AttachReturnsWhenSessionExits(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

	req := newRequestFor("s1", rt)
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s := rt.get(req.Workspace.LogPath)

	attachDone := make(chan error, 1)
	go func() { attachDone <- m.Attach(context.Background(), "s1", io.Discard) }()

	time.Sleep(20 * time.Millisecond)
	s.complete(0)

	select {
	case err := <-attachDone:
		if err != nil {
			t.Fatalf("Attach returned %v; want nil after broker close", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Attach did not return after session exit")
	}

	_ = m.Shutdown(context.Background())
}

// threadsafeBuffer is a minimal sync-wrapped bytes.Buffer for use across the
// test goroutines above.
type threadsafeBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *threadsafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *threadsafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func TestManager_ConcurrentStartStopNoRace(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink)

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
			if err := m.Start(context.Background(), newRequestFor(id, rt)); err != nil {
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
			req := newRequestFor(id, rt)
			if i%2 == 0 {
				_ = m.Stop(context.Background(), id)
				return
			}
			if s := rt.get(req.Workspace.LogPath); s != nil {
				s.complete(0)
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
