package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// stopHarness is a Service over a real state DB and event bus, with the
// Manager wired the way New wires it.
func stopHarness(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus := events.NewBus(events.BusOptions{Persister: db})
	mgr, stops := newSessionManager(db, bus)
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	return &Service{Store: db, Bus: bus, Manager: mgr, stops: stops}
}

// startSession creates a session row and starts rt under the Manager.
func startSession(t *testing.T, svc *Service, id string, rt agentsessions.Runtime) {
	t.Helper()
	row := store.SessionRow{ID: id, LaunchID: "l1", ProjectID: "p", ProviderID: "stub", Workspace: t.TempDir(), State: string(session.StateCreated)}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Manager.Start(context.Background(), agentsessions.StartRequest{ID: id, Runtime: rt}); err != nil {
		t.Fatal(err)
	}
}

func waitExit(t *testing.T, svc *Service, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := svc.WaitSession(ctx, id); err != nil {
		t.Fatalf("wait %s: %v", id, err)
	}
}

// terminalEvent returns the newest session.state_changed payload for id, as
// GET /sessions/{id}/events reads it (newest first).
func terminalEvent(t *testing.T, svc *Service, id string) sessionStateChangedPayload {
	t.Helper()
	evs, err := svc.Store.ListEventsBySession(id, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind != events.KindSessionStateChanged {
			continue
		}
		var p sessionStateChangedPayload
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Fatalf("no %s event for %s", events.KindSessionStateChanged, id)
	return sessionStateChangedPayload{}
}

func assertTerminal(t *testing.T, svc *Service, id, wantState string, wantExit int) {
	t.Helper()
	row, err := svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != wantState {
		t.Errorf("row state = %q; want %q", row.State, wantState)
	}
	if !row.ExitCode.Valid || int(row.ExitCode.Int64) != wantExit {
		t.Errorf("row exit_code = %+v; want %d", row.ExitCode, wantExit)
	}
	if !row.EndedAt.Valid {
		t.Error("row ended_at not set")
	}
	ev := terminalEvent(t, svc, id)
	if ev.From != string(session.StateRunning) || ev.To != wantState {
		t.Errorf("event %s → %s; want running → %s", ev.From, ev.To, wantState)
	}
	if ev.ExitCode == nil || *ev.ExitCode != wantExit {
		t.Errorf("event exit_code = %v; want %d", ev.ExitCode, wantExit)
	}
}

// exitRuntime is a session that exits with code on its own once released,
// or with stopCode when stopped first — a signaled process reports -1.
type exitRuntime struct {
	code, stopCode int
	release        chan struct{}
}

func (r exitRuntime) ID() string                       { return "exit-fake" }
func (r exitRuntime) Kind() string                     { return "api" }
func (r exitRuntime) Caps() agentsessions.Capabilities { return agentsessions.Capabilities{} }
func (r exitRuntime) Prepare(context.Context) error    { return nil }
func (r exitRuntime) Start(context.Context, agentsessions.StartOptions) (agentsessions.Session, error) {
	return &exitSession{rt: r, stopped: make(chan struct{})}, nil
}

type exitSession struct {
	rt      exitRuntime
	stopped chan struct{}
}

func (s *exitSession) Wait() (int, error) {
	select {
	case <-s.rt.release:
		return s.rt.code, nil
	case <-s.stopped:
		return s.rt.stopCode, nil
	}
}

func (s *exitSession) Stop(context.Context) error {
	select {
	case <-s.stopped:
	default:
		close(s.stopped)
	}
	return nil
}

func (s *exitSession) SendInput(context.Context, []byte) error {
	return agentsessions.ErrNoInputChannel
}
func (s *exitSession) Resize(context.Context, uint16, uint16) error { return nil }
func (s *exitSession) Health() agentsessions.HealthStatus {
	return agentsessions.HealthStatus{Alive: true, PID: 4242}
}
func (s *exitSession) CheckpointHints() (agentsessions.CheckpointHint, bool) {
	return agentsessions.CheckpointHint{}, false
}

// The smoke case (CW-20260930-0250): a session hung waiting on input exits 0
// when stopped. It was recorded as completed/0; it must be killed.
func TestStopSession_HungSessionRecordsKilled(t *testing.T) {
	svc := stopHarness(t)
	rt, err := stub.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	startSession(t, svc, "s-hung", rt)

	if err := svc.StopSession("s-hung"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, svc, "s-hung")
	assertTerminal(t, svc, "s-hung", string(session.StateKilled), 0)
}

// A stop that ends the process by signal (exit -1) is killed too, not the
// completed/-1 Hadron read as an agent that finished without a result.
func TestStopSession_SignaledSessionRecordsKilled(t *testing.T) {
	svc := stopHarness(t)
	startSession(t, svc, "s-sig", exitRuntime{stopCode: -1, release: make(chan struct{})})

	if err := svc.StopSession("s-sig"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, svc, "s-sig")
	assertTerminal(t, svc, "s-sig", string(session.StateKilled), -1)
}

// Sessions that end on their own keep completed (exit 0) and failed
// (non-zero), and a stop of one session does not leak onto another.
func TestNaturalExit_CompletedAndFailedUnchanged(t *testing.T) {
	svc := stopHarness(t)
	stopped := exitRuntime{stopCode: -1, release: make(chan struct{})}
	startSession(t, svc, "s-stopped", stopped)
	if err := svc.StopSession("s-stopped"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, svc, "s-stopped")

	for _, tc := range []struct {
		id    string
		code  int
		state session.State
	}{
		{"s-ok", 0, session.StateCompleted},
		{"s-err", 1, session.StateFailed},
	} {
		release := make(chan struct{})
		startSession(t, svc, tc.id, exitRuntime{code: tc.code, stopCode: -1, release: release})
		close(release)
		waitExit(t, svc, tc.id)
		assertTerminal(t, svc, tc.id, string(tc.state), tc.code)
	}
}

// The stop request is dropped once the terminal state is written, so it
// cannot outlive the session it was for.
func TestStopSession_ClearsRequestAfterExit(t *testing.T) {
	svc := stopHarness(t)
	startSession(t, svc, "s-clear", exitRuntime{stopCode: -1, release: make(chan struct{})})
	if err := svc.StopSession("s-clear"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, svc, "s-clear")
	deadline := time.Now().Add(5 * time.Second)
	for svc.stops.requested("s-clear") {
		if time.Now().After(deadline) {
			t.Fatal("stop request still recorded after the session exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Stopping a session that is not running is an error and records nothing.
func TestStopSession_NotRunningRecordsNothing(t *testing.T) {
	svc := stopHarness(t)
	if err := svc.StopSession("s-missing"); !errors.Is(err, agentsessions.ErrSessionNotRunning) {
		t.Fatalf("err = %v; want ErrSessionNotRunning", err)
	}
	if svc.stops.requested("s-missing") {
		t.Fatal("stop request recorded for a session that was not running")
	}
}

// A second stop that finds the session already gone clears only its own
// mark, not the first stop's.
func TestStopRequests_CountsMarks(t *testing.T) {
	var r stopRequests
	r.mark("s")
	r.mark("s")
	r.clear("s")
	if !r.requested("s") {
		t.Fatal("one clear dropped both marks")
	}
	r.clear("s")
	if r.requested("s") {
		t.Fatal("still requested after every mark was cleared")
	}
	r.clear("s") // an extra clear is harmless
	if r.requested("s") || len(r.ids) != 0 {
		t.Fatalf("ids = %v after extra clear; want empty", r.ids)
	}
	var nilReqs *stopRequests
	nilReqs.mark("s")
	if nilReqs.requested("s") {
		t.Fatal("nil stopRequests reported a request")
	}
}

func TestMapLifecycleStates(t *testing.T) {
	ev := func(to agentsessions.State, reason string) agentsessions.LifecycleEvent {
		return agentsessions.LifecycleEvent{From: agentsessions.StateRunning, To: to, Reason: reason}
	}
	for _, tc := range []struct {
		name    string
		ev      agentsessions.LifecycleEvent
		stopped bool
		want    string
	}{
		{"done", ev(agentsessions.StateDone, ""), false, "completed"},
		{"failed", ev(agentsessions.StateFailed, ""), false, "failed"},
		{"done after stop", ev(agentsessions.StateDone, ""), true, "killed"},
		{"failed after stop", ev(agentsessions.StateFailed, ""), true, "killed"},
		{"done with killed reason", ev(agentsessions.StateDone, "killed"), false, "killed"},
		{"running after stop", ev(agentsessions.StateRunning, ""), true, "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, to := mapLifecycleStates(tc.ev, tc.stopped)
			if from != "running" || to != tc.want {
				t.Fatalf("got %s → %s; want running → %s", from, to, tc.want)
			}
		})
	}
}
