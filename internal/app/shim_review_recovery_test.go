//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-runner/runner"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

func TestShimTerminalStopPreservesOutcome(t *testing.T) {
	for _, state := range []string{"failed", "completed", "killed", "orphaned"} {
		t.Run(state, func(t *testing.T) {
			f := shimFixture(t)
			r := f.svc.shimHosting.provider.PlacementIdentity(f.req.ID+":1", shimLaunchIdentity(f))
			if err := f.svc.persistShim(context.Background(), f.req.ID, "claude", "boot", r); err != nil {
				t.Fatal(err)
			}
			if err := shimhost.PrivateDir(filepath.Dir(r.DescriptorPath)); err != nil {
				t.Fatal(err)
			}
			if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(r.DescriptorPath), "placement.json"), r); err != nil {
				t.Fatal(err)
			}
			exit := 7
			if err := f.svc.Store.UpdateSessionState(f.req.ID, state, 0, &exit); err != nil {
				t.Fatal(err)
			}
			f.svc.shimHosting.stop = func(context.Context, shimhost.Receipt) error { t.Error("terminal stop signalled host"); return nil }
			err := f.svc.StopSession(f.req.ID)
			if !errors.Is(err, agentsessions.ErrSessionNotRunning) {
				t.Fatalf("terminal stop=%v", err)
			}
			row, err := f.svc.Store.GetSession(f.req.ID)
			if err != nil || row.State != state || !row.ExitCode.Valid || row.ExitCode.Int64 != 7 {
				t.Fatalf("terminal outcome changed: %+v %v", row, err)
			}
		})
	}
}

func TestShimCancelledRequestStillRecordsPlacement(t *testing.T) {
	f := shimFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.shimHosting.place = func(ctx context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
		r, err := f.svc.shimHosting.provider.Place(ctx, key, spec)
		if err != nil {
			return r, err
		}
		cancel()
		return r, nil
	}
	req, err := f.svc.prepareShimStart(ctx, f.plan, f.req)
	if err != nil || req.Runtime == f.req.Runtime {
		t.Fatalf("cancelled request abandoned placement: %v", err)
	}
	row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	if err != nil || row.HostPID == 0 || row.ProviderPID == 0 {
		t.Fatalf("placement not durably recorded: %+v %v", row, err)
	}
}

func TestShimUnsubmittedIntentIsRecoverable(t *testing.T) {
	f := shimFixture(t)
	r := f.svc.shimHosting.provider.PlacementIdentity(f.req.ID+":1", shimLaunchIdentity(f))
	if err := f.svc.persistShim(context.Background(), f.req.ID, "claude", "boot", r); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Store.UpdateSessionState(f.req.ID, "launching", 0, nil); err != nil {
		t.Fatal(err)
	}
	f.svc.ReconcileStaleState()
	row, err := f.svc.Store.GetSession(f.req.ID)
	if err != nil || row.State != "failed" {
		t.Fatalf("unsubmitted intent wedged: %+v %v", row, err)
	}
	if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
		t.Fatalf("unsubmitted shim retained: %v", err)
	}
}

func TestShimCheckpointFailureBeforePlacementFallsBack(t *testing.T) {
	f := shimFixture(t)
	prepare := f.svc.shimHosting.prepare
	f.svc.shimHosting.prepare = func(spec shim.Launch, policy *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
		if err := os.Mkdir(filepath.Join(f.svc.shimHosting.provider.SessionDir(f.req.ID), "bridge.json"), 0700); err != nil {
			t.Fatal(err)
		}
		return prepare(spec, policy, limits)
	}
	f.svc.shimHosting.place = func(context.Context, string, shim.Launch) (shimhost.Receipt, error) {
		t.Fatal("checkpoint failure submitted host")
		return shimhost.Receipt{}, nil
	}
	req, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err != nil || req.Runtime != f.req.Runtime {
		t.Fatalf("pre-child checkpoint failure did not fall back: %v", err)
	}
	if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
		t.Fatalf("unstarted intent retained: %v", err)
	}
}

func TestShimFailureCodeNilIsEmpty(t *testing.T) {
	if got := shimFailureCode(nil); got != "" {
		t.Fatalf("nil error classified as %q", got)
	}
}

func TestShimReattachedOutputFlushesOnExit(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.svc.DrainSessions(ctx); err != nil {
		t.Fatal(err)
	}
	mgr, stops := newSessionManager(f.svc.Store, f.svc.Bus)
	restarted := &Service{Catalog: f.svc.Catalog, Store: f.svc.Store, Bus: f.svc.Bus, Registry: f.svc.Registry, Manager: mgr, stops: stops, shimHosting: f.svc.shimHosting, turnFeeds: f.svc.turnFeeds}
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	restarted.ReconcileStaleState()
	// Hold queue submission: a finalizer must remember its boundary without
	// starting a queue drain. The real dispatcher records this notification.
	d := newReplyDispatcher(context.Background(), restarted.Store, restarted.Registry, restarted.replyRuntime(), nil)
	d.mu.Lock()
	d.drainFor(f.req.ID).submitters = 1
	d.mu.Unlock()
	restarted.replies.Store(d)
	t.Cleanup(d.stop)
	if err := restarted.SendTurn(context.Background(), f.req.ID, "partial-exit"); err != nil {
		t.Fatal(err)
	}
	_, _ = mgr.WaitSession(ctx, f.req.ID)
	shimAwait(t, "flushed output and queue boundary", func() bool {
		_, retained := restarted.turnOutputs.Load(f.req.ID)
		d.mu.Lock()
		boundary := d.drainFor(f.req.ID).again
		d.mu.Unlock()
		return !retained && boundary && len(outputEvents(t, restarted)) == 1
	})
	ev := outputEvents(t, restarted)[0]
	if ev.Text != "unfinished response" {
		t.Fatalf("partial output lost: %+v", ev)
	}
}

func TestShimStopMarksBeforeHostAndNeverDetaches(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	f.svc.shimHosting.stop = func(ctx context.Context, got shimhost.Receipt) error {
		if !f.svc.stops.requested(f.req.ID) {
			t.Error("host stop preceded stop-request fence")
		}
		// Force bridge terminal observation at the host-stop boundary.
		if err := (stateSinkAdapter{db: f.svc.Store, stops: f.svc.stops}).UpdateSessionState(f.req.ID, agentsessions.StateDone, 0, new(int)); err != nil {
			t.Fatal(err)
		}
		sink := &eventSinkAdapter{db: f.svc.Store, stops: f.svc.stops, bus: f.svc.Bus}
		sink.Emit(ctx, agentsessions.LifecycleEvent{SessionID: f.req.ID, From: agentsessions.StateRunning, To: agentsessions.StateDone})
		return f.svc.shimHosting.provider.Stop(ctx, got)
	}
	if err := f.svc.StopSession(f.req.ID); err != nil {
		t.Fatal(err)
	}
	assertNoShimDetachedEvents(t, f)
	if f.svc.stops.requested(f.req.ID) {
		t.Fatal("stop fence leaked")
	}
	if got, err := f.svc.shimHosting.provider.Inspect(context.Background(), r); err != nil || !got.Gone {
		t.Fatalf("stop left provider: %+v %v", got, err)
	}
}

func TestShimNaturalExitNeverDetaches(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	if err := f.svc.SendTurn(context.Background(), f.req.ID, "exit"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "provider failure", func() bool { row, _ := f.svc.Store.GetSession(f.req.ID); return row.State == "failed" })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.svc.waitShimBinding(ctx, f.req.ID); err != nil {
		t.Fatal(err)
	}
	assertNoShimDetachedEvents(t, f)
	change, changed, err := f.svc.Store.CompleteSessionShim(ctx, f.req.ID, "killed", 0)
	if err != nil || changed {
		t.Fatalf("guard rewrote terminal outcome: %+v %v %v", change, changed, err)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if !row.ExitCode.Valid || row.ExitCode.Int64 != 7 {
		t.Fatalf("lost provider exit: %+v", row)
	}
}

func assertNoShimDetachedEvents(t *testing.T, f *shimAppFixture) {
	t.Helper()
	rows, err := f.svc.Store.QueryEvents(store.EventFilter{SessionID: f.req.ID, Kinds: []string{events.KindSessionStateChanged}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		var ev sessionStateChangedPayload
		if err := json.Unmarshal([]byte(row.PayloadJSON), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.To == "detached" {
			t.Fatalf("spurious detach: %+v", ev)
		}
	}
	for _, ev := range shimStatusEvents(t, f) {
		if ev.State == "detached" {
			t.Fatalf("spurious shim detach: %+v", ev)
		}
	}
}

func TestShimExpiredStartupBudgetRetainsWithoutInspect(t *testing.T) {
	f := shimFixture(t)
	r := f.svc.shimHosting.provider.PlacementIdentity(f.req.ID+":1", shimLaunchIdentity(f))
	if err := f.svc.persistShim(context.Background(), f.req.ID, "claude", "boot", r); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Store.UpdateSessionState(f.req.ID, "launching", 0, nil); err != nil {
		t.Fatal(err)
	}
	f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
		t.Fatal("expired budget inspected")
		return shimhost.Inspection{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !f.svc.reconcileShimContext(ctx, store.StaleSession{ID: f.req.ID, State: "launching"}) {
		t.Fatal("expired budget passed to legacy sweep")
	}
	statuses := shimStatusEvents(t, f)
	if len(statuses) != 1 || statuses[0].Reason != "startup_budget_exhausted" {
		t.Fatalf("missing bounded-startup refusal: %+v", statuses)
	}
	if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); err != nil {
		t.Fatal("budget expiry removed intent")
	}
}
