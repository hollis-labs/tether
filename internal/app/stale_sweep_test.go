package app

import (
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// fakeProcesses is a processInspector over a fixed process table.
type fakeProcesses map[int]struct{ started, cmd string }

func (f fakeProcesses) alive(pid int) bool { _, ok := f[pid]; return ok }
func (f fakeProcesses) startTime(pid int) (string, bool) {
	p, ok := f[pid]
	return p.started, ok
}
func (f fakeProcesses) command(pid int) (string, bool) {
	p, ok := f[pid]
	return p.cmd, ok
}

// runningSession creates a session that was running under pid when the
// previous daemon went away, with startedAt recorded unless empty.
func runningSession(t *testing.T, svc *Service, id, command string, pid int, startedAt string) {
	t.Helper()
	row := store.SessionRow{ID: id, LaunchID: "l1", ProjectID: "p", LogicalAgentID: "worker", ProviderID: "stub", Workspace: t.TempDir(), State: string(session.StateCreated)}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1", Command: command}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.UpdateSessionState(id, string(session.StateRunning), pid, nil); err != nil {
		t.Fatal(err)
	}
	if startedAt != "" {
		if err := svc.Store.SetSessionProcessStart(id, pid, startedAt); err != nil {
			t.Fatal(err)
		}
	}
}

func sessionState(t *testing.T, svc *Service, id string) (string, int64) {
	t.Helper()
	row, err := svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return row.State, row.ExitCode.Int64
}

// sweptEvent returns the payload of the one daemon.sessions_swept event.
func sweptEvent(t *testing.T, svc *Service) sessionsSweptPayload {
	t.Helper()
	evs, err := svc.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var found []sessionsSweptPayload
	for _, ev := range evs {
		if ev.Kind != events.KindDaemonSessionsSwept {
			continue
		}
		var p sessionsSweptPayload
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &p); err != nil {
			t.Fatal(err)
		}
		found = append(found, p)
	}
	if len(found) != 1 {
		t.Fatalf("got %d daemon.sessions_swept events, want 1", len(found))
	}
	return found[0]
}

// The startup sweep spares a session whose own process is still alive,
// condemns one whose process is gone, and condemns one whose pid is alive
// but now names a different process (CW-20260912-0085).
func TestReconcileStaleState_ChecksProcessLiveness(t *testing.T) {
	svc := bindingHarness(t)
	const launched = "2026-10-01T03:00:00Z"
	svc.procs = fakeProcesses{
		101: {started: launched, cmd: "claude --print"},
		303: {started: "2026-10-01T03:40:00Z", cmd: "vim notes.txt"}, // pid 303 reused later
	}
	runningSession(t, svc, "s-live", "claude", 101, launched)
	runningSession(t, svc, "s-dead", "claude", 202, launched)
	runningSession(t, svc, "s-recycled", "claude", 303, launched)
	svc.leaseActorBinding("s-live", "worker")

	svc.ReconcileStaleState()

	if st, _ := sessionState(t, svc, "s-live"); st != string(session.StateRunning) {
		t.Errorf("s-live state = %s, want running (its process survived)", st)
	}
	for _, id := range []string{"s-dead", "s-recycled"} {
		if st, code := sessionState(t, svc, id); st != string(session.StateFailed) || code != -1 {
			t.Errorf("%s = %s exit %d, want failed -1", id, st, code)
		}
	}
	// The surviving session keeps its binding (#71 revokes only ended ones).
	if b, err := currentWorkerBinding(svc); err != nil || b.SessionID != "s-live" {
		t.Errorf("binding after restart = %+v, %v; want it still held by s-live", b, err)
	}

	p := sweptEvent(t, svc)
	if p.Swept != 2 || p.Spared != 1 || !slices.Equal(p.SparedSessionIDs, []string{"s-live"}) ||
		!slices.Contains(p.SweptSessionIDs, "s-dead") || !slices.Contains(p.SweptSessionIDs, "s-recycled") {
		t.Errorf("daemon.sessions_swept = %+v", p)
	}
}

// A session launched before start times were recorded is spared only when
// its pid runs the session's launch command and started after the session
// was created.
func TestReconcileStaleState_LegacyRowsWithoutStartTime(t *testing.T) {
	svc := bindingHarness(t)
	future := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	past := "2020-01-01T00:00:00Z"
	svc.procs = fakeProcesses{
		11: {started: future, cmd: "/usr/local/bin/claude --print"},
		12: {started: future, cmd: "/usr/bin/python3 other.py"},
		13: {started: past, cmd: "/usr/local/bin/claude --print"},
	}
	runningSession(t, svc, "s-match", "/usr/local/bin/claude", 11, "")
	runningSession(t, svc, "s-other-cmd", "/usr/local/bin/claude", 12, "")
	runningSession(t, svc, "s-too-old", "/usr/local/bin/claude", 13, "")

	svc.ReconcileStaleState()

	if st, _ := sessionState(t, svc, "s-match"); st != string(session.StateRunning) {
		t.Errorf("s-match = %s, want running", st)
	}
	for _, id := range []string{"s-other-cmd", "s-too-old"} {
		if st, _ := sessionState(t, svc, id); st != string(session.StateFailed) {
			t.Errorf("%s = %s, want failed", id, st)
		}
	}
}

// End to end against a real process: launching records the process start
// time through the state sink, a restart spares the session while the
// process lives, and condemns it once the process is gone.
func TestReconcileStaleState_RealProcess(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not available")
	}
	svc := bindingHarness(t)
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })
	pid := child.Process.Pid

	row := store.SessionRow{ID: "s-real", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "worker", ProviderID: "stub", Workspace: t.TempDir(), State: string(session.StateCreated)}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1", Command: "sleep"}); err != nil {
		t.Fatal(err)
	}
	sink := stateSinkAdapter{db: svc.Store, stops: &stopRequests{}}
	if err := sink.UpdateSessionState("s-real", agentsessions.StateRunning, pid, nil); err != nil {
		t.Fatal(err)
	}
	var recorded string
	if err := svc.Store.DB().QueryRow(`SELECT COALESCE(pid_started_at, '') FROM sessions WHERE id='s-real'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, recorded); err != nil {
		t.Fatalf("pid_started_at = %q after the running transition, want an RFC3339 start time", recorded)
	}

	svc.ReconcileStaleState()
	if st, _ := sessionState(t, svc, "s-real"); st != string(session.StateRunning) {
		t.Fatalf("s-real = %s while its process lives, want running", st)
	}

	_ = child.Process.Kill()
	_, _ = child.Process.Wait()
	svc.ReconcileStaleState()
	if st, code := sessionState(t, svc, "s-real"); st != string(session.StateFailed) || code != -1 {
		t.Fatalf("s-real = %s exit %d after its process died, want failed -1", st, code)
	}
	if b, err := currentWorkerBinding(svc); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("binding = %+v, %v; want none for an ended session", b, err)
	}
}
