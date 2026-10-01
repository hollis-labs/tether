package app

// CW-20260912-0134: a binding must not outlive its session. Revoking the
// current generation used to promote a superseded one whose session had
// already ended, and the startup sweep failed sessions without touching
// their bindings, so both left an actor bound to a dead session.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// bindingHarness is a Service with a real store, Manager and registry, the
// way New composes them.
func bindingHarness(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: "worker", Name: "worker"}, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})
	mgr, stops := newSessionManager(db, bus)
	t.Cleanup(func() {
		// Shutdown waits for every session's watch goroutine, so end any a
		// test left running first.
		for _, info := range mgr.List() {
			_ = mgr.Stop(context.Background(), info.ID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})
	return &Service{
		Store:    db,
		Bus:      bus,
		Manager:  mgr,
		stops:    stops,
		Registry: registry.NewService(registry.NewStorage(db.DB())),
	}
}

// launchBound starts a stub session for logical agent "worker" and leases
// its binding, as LaunchSession does.
func launchBound(t *testing.T, svc *Service, id string) {
	t.Helper()
	row := store.SessionRow{ID: id, LaunchID: "l1", ProjectID: "p", LogicalAgentID: "worker", ProviderID: "stub", Workspace: t.TempDir(), State: string(session.StateCreated)}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatal(err)
	}
	rt, err := stub.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Manager.Start(context.Background(), agentsessions.StartRequest{ID: id, Runtime: rt}); err != nil {
		t.Fatal(err)
	}
	svc.leaseActorBinding(id, "worker")
	svc.watchSessionBindings(id)
}

func stopAndWait(t *testing.T, svc *Service, id string) {
	t.Helper()
	if err := svc.StopSession(id); err != nil {
		t.Fatalf("stop %s: %v", id, err)
	}
	waitExit(t, svc, id)
}

func currentWorkerBinding(svc *Service) (registry.RuntimeBinding, error) {
	return svc.Registry.CurrentBinding(context.Background(), registry.LogicalAgentBindingTarget("worker"))
}

// waitNoCurrentBinding waits for the exit-time revocation, which runs once
// the session's terminal state is recorded.
func waitNoCurrentBinding(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := currentWorkerBinding(svc)
		if errors.Is(err, registry.ErrBindingNotFound) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("current binding = session %s gen %d; want none -- every session has ended", b.SessionID, b.Generation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The task's scenario: A takes generation 1, C supersedes it at 2, then
// both stop, A first. Stopping C used to promote A's binding, though A had
// already ended.
func TestStopSession_TwoGenerationsLeaveNoDeadBinding(t *testing.T) {
	svc := bindingHarness(t)
	launchBound(t, svc, "s-a")
	launchBound(t, svc, "s-c")
	if b, err := currentWorkerBinding(svc); err != nil || b.SessionID != "s-c" || b.Generation != 2 {
		t.Fatalf("before = %+v, %v; want s-c at generation 2", b, err)
	}

	stopAndWait(t, svc, "s-a")
	if b, err := currentWorkerBinding(svc); err != nil || b.SessionID != "s-c" {
		t.Fatalf("after stopping A = %+v, %v; want s-c still current", b, err)
	}
	stopAndWait(t, svc, "s-c")
	waitNoCurrentBinding(t, svc)
}

// Stopping the newer session while the older one still runs hands the actor
// back to the older one: superseded generations are kept while they live.
func TestStopSession_LiveSupersededGenerationBecomesCurrent(t *testing.T) {
	svc := bindingHarness(t)
	launchBound(t, svc, "s-a")
	launchBound(t, svc, "s-c")

	stopAndWait(t, svc, "s-c")
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := currentWorkerBinding(svc)
		if err == nil && b.SessionID == "s-a" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("current = %+v, %v; want running s-a", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A session that ends on its own, without a stop, gives up its binding too.
func TestSessionExit_RevokesItsBinding(t *testing.T) {
	svc := bindingHarness(t)
	release := make(chan struct{})
	row := store.SessionRow{ID: "s-exit", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "worker", ProviderID: "stub", Workspace: t.TempDir(), State: string(session.StateCreated)}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Manager.Start(context.Background(), agentsessions.StartRequest{ID: "s-exit", Runtime: exitRuntime{code: 0, release: release}}); err != nil {
		t.Fatal(err)
	}
	svc.leaseActorBinding("s-exit", "worker")
	svc.watchSessionBindings("s-exit")

	close(release)
	waitExit(t, svc, "s-exit")
	waitNoCurrentBinding(t, svc)
}

// After a daemon restart the sweep fails every session that was running, and
// must revoke their bindings with them: a restart must not leave an actor
// bound to a session that no longer exists.
func TestReconcileStaleState_RevokesBindingsOfSweptSessions(t *testing.T) {
	svc := bindingHarness(t)
	for _, id := range []string{"s-old", "s-new"} {
		row := store.SessionRow{ID: id, LaunchID: "l1", ProjectID: "p", LogicalAgentID: "worker", ProviderID: "stub", Workspace: t.TempDir(), State: string(session.StateCreated)}
		if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
			t.Fatal(err)
		}
		// Running when the previous daemon went away.
		if err := svc.Store.UpdateSessionState(id, string(session.StateRunning), 4242, nil); err != nil {
			t.Fatal(err)
		}
		svc.leaseActorBinding(id, "worker")
	}
	// A binding whose session_id is not a Tether session -- a published-local
	// bridge -- is not the sweep's to revoke.
	bridgeTarget := registry.LogicalAgentBindingTarget("bridged")
	if _, err := svc.Registry.LeaseBinding(context.Background(), bridgeTarget, "bridge-proc-1", "bridge-host", "a1", nil, registry.VisibilityPublishedLocal, 0); err != nil {
		t.Fatal(err)
	}

	svc.ReconcileStaleState()

	if b, err := currentWorkerBinding(svc); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("current binding after restart = %+v, %v; want none", b, err)
	}
	if b, err := svc.Registry.CurrentBinding(context.Background(), bridgeTarget); err != nil || b.SessionID != "bridge-proc-1" {
		t.Fatalf("bridge binding after restart = %+v, %v; want it untouched", b, err)
	}
}
