package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// shutdownEvent returns the payload of the one daemon.shutdown_sessions_ended
// event.
func shutdownEvent(t *testing.T, svc *Service) shutdownSessionsPayload {
	t.Helper()
	evs, err := svc.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var found []shutdownSessionsPayload
	for _, ev := range evs {
		if ev.Kind != events.KindDaemonShutdownSessionsEnded {
			continue
		}
		var p shutdownSessionsPayload
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &p); err != nil {
			t.Fatal(err)
		}
		found = append(found, p)
	}
	if len(found) != 1 {
		t.Fatalf("got %d daemon.shutdown_sessions_ended events, want 1", len(found))
	}
	return found[0]
}

// drainAsync runs DrainSessions and returns once every live session has
// been marked, so the caller can then end them as the shutdown's SIGTERM
// would.
func drainAsync(ctx context.Context, t *testing.T, svc *Service, ids ...string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- svc.DrainSessions(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for _, id := range ids {
		for !svc.stops.requested(id) {
			if time.Now().After(deadline) {
				t.Fatalf("%s was never marked for the shutdown", id)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return done
}

// A planned shutdown records a session that ends during the drain as
// `killed` with reason daemon-shutdown, not as a failure, and reports it.
func TestDrainSessions_PlannedShutdownRecordsKilled(t *testing.T) {
	svc := stopHarness(t)
	release := make(chan struct{})
	// The process exits with -1, as a signaled one reports, once released.
	startSession(t, svc, "s-up", exitRuntime{code: -1, stopCode: -1, release: release})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := drainAsync(ctx, t, svc, "s-up")
	close(release) // the shutdown's SIGTERM reaches the agent
	if err := <-done; err != nil {
		t.Fatalf("DrainSessions: %v", err)
	}

	assertTerminal(t, svc, "s-up", string(session.StateKilled), -1)
	if ev := terminalEvent(t, svc, "s-up"); ev.Reason != ShutdownStopReason {
		t.Errorf("terminal event reason = %q, want %q", ev.Reason, ShutdownStopReason)
	}
	p := shutdownEvent(t, svc)
	if p.Ended != 1 || !slices.Equal(p.EndedSessionIDs, []string{"s-up"}) || p.StillRunning != 0 {
		t.Errorf("daemon.shutdown_sessions_ended = %+v", p)
	}
}

// A session that has not exited when the drain ends stays non-terminal for
// the next start's sweep: the daemon cannot tell whether it survived.
func TestDrainSessions_UnexitedSessionLeftForTheSweep(t *testing.T) {
	svc := stopHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the Manager's watcher finish
	startSession(t, svc, "s-stuck", exitRuntime{code: 0, stopCode: 0, release: release})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := <-drainAsync(ctx, t, svc, "s-stuck"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DrainSessions = %v, want the drain deadline", err)
	}

	row, err := svc.Store.GetSession("s-stuck")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != string(session.StateRunning) {
		t.Errorf("state = %s, want running (not settled by the drain)", row.State)
	}
	p := shutdownEvent(t, svc)
	if p.Ended != 0 || p.StillRunning != 1 || !slices.Equal(p.StillRunningSessionIDs, []string{"s-stuck"}) {
		t.Errorf("daemon.shutdown_sessions_ended = %+v", p)
	}
}

// A crash leaves no shutdown record: the next start's sweep still fails the
// session with exit_code -1, while a session a planned shutdown already
// recorded as killed is left as it is.
func TestCrash_NextStartStillSweepsFailed(t *testing.T) {
	svc := bindingHarness(t)
	svc.procs = fakeProcesses{} // no process survived
	runningSession(t, svc, "s-crashed", "claude", 4321, "2026-10-01T03:00:00Z")
	row := store.SessionRow{ID: "s-planned", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "worker", ProviderID: "stub", Workspace: t.TempDir(), State: string(session.StateCreated)}
	if err := svc.Store.CreateSession(row, nil); err != nil {
		t.Fatal(err)
	}
	exit := -1
	if err := svc.Store.UpdateSessionState("s-planned", string(session.StateKilled), 0, &exit); err != nil {
		t.Fatal(err)
	}

	svc.ReconcileStaleState()

	if st, code := sessionState(t, svc, "s-crashed"); st != string(session.StateFailed) || code != -1 {
		t.Errorf("crashed session = %s exit %d, want failed -1", st, code)
	}
	if st, _ := sessionState(t, svc, "s-planned"); st != string(session.StateKilled) {
		t.Errorf("planned-shutdown session = %s, want killed (the sweep must not touch it)", st)
	}
}
