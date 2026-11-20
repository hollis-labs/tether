package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func recordedExitFixture(t *testing.T, svc *Service, id, worker string, exit int) {
	t.Helper()
	if err := svc.Store.CreateSession(store.SessionRow{ID: id, WorkerID: worker, State: "running", Workspace: t.TempDir()}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET pid=0,ended_at='2026-11-19T00:00:00Z',exit_code=? WHERE id=?`, exit, id); err != nil {
		t.Fatal(err)
	}
}

func TestRecordedExitStartupAndPeriodic(t *testing.T) {
	for _, entry := range []string{"startup", "periodic"} {
		t.Run(entry, func(t *testing.T) {
			svc := stopHarness(t)
			recordedExitFixture(t, svc, "failed", "", 1)
			recordedExitFixture(t, svc, "completed", "", 0)
			recordedExitFixture(t, svc, "remote-ended", "disconnected", 1)
			reasonID, _, err := svc.Store.InsertEvent(events.ScopeSession, "failed", "session.failed", `{"reason":"provider refused request"}`)
			if err != nil {
				t.Fatal(err)
			}

			for i := 0; i < 2; i++ {
				if entry == "startup" {
					svc.ReconcileStaleState()
				} else if err := svc.SweepSessionReaper(context.Background(), time.Now()); err != nil {
					t.Fatal(err)
				}

				var reason string
				if err := svc.Store.DB().QueryRow(`SELECT payload_json FROM events WHERE id=?`, reasonID).Scan(&reason); err != nil {
					t.Fatal(err)
				}
				if reason != `{"reason":"provider refused request"}` {
					t.Fatalf("failure reason changed: %s", reason)
				}
				for _, id := range []string{"failed", "completed", "remote-ended"} {
					row, err := svc.Store.GetSession(id)
					if err != nil {
						t.Fatal(err)
					}
					want := "failed"
					exit := int64(1)
					if id == "completed" {
						want = "completed"
						exit = 0
					}
					if row.State != want || row.EndedAt.String != "2026-11-19T00:00:00Z" || row.ExitCode.Int64 != exit {
						t.Fatalf("lost observed exit: %+v", row)
					}
					if err := svc.StopSession(id); err != nil {
						t.Fatalf("repeat stop: %v", err)
					}
				}
			}
		})
	}
}

func TestRecordedExitStopAndGuards(t *testing.T) {
	svc := stopHarness(t)
	recordedExitFixture(t, svc, "orphan", "", 1)
	for i := 0; i < 2; i++ {
		if err := svc.StopSession("orphan"); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.StopSession("unknown"); !errors.Is(err, agentsessions.ErrSessionNotRunning) {
		t.Fatalf("unknown stop=%v", err)
	}
	// A runtime with PID zero remains authoritative even with stale exit metadata.
	rt, err := stub.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	startSession(t, svc, "live", rt)
	t.Cleanup(func() { _ = svc.StopSession("live") })
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET pid=0,ended_at='old',exit_code=1 WHERE id='live'`); err != nil {
		t.Fatal(err)
	}
	recordedExitFixture(t, svc, "launch-held", "", 1)
	unlock, err := svc.lockSessionLaunch(context.Background(), "launch-held")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, id := range []string{"remote-unknown", "in-flight"} {
		worker := "remote"
		state := "running"
		if id == "in-flight" {
			state = "launching"
			worker = ""
		}
		if err := svc.Store.CreateSession(store.SessionRow{ID: id, WorkerID: worker, State: state, PID: sql.NullInt64{Valid: true}}, &launch.Plan{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.SweepSessionReaper(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live", "launch-held", "remote-unknown", "in-flight"} {
		row, err := svc.Store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		want := "running"
		if id == "in-flight" {
			want = "launching"
		}
		if row.State != want {
			t.Fatalf("%s incorrectly recovered: %+v", id, row)
		}
	}
}

func TestRecordedExitStorageError(t *testing.T) {
	svc := stopHarness(t)
	recordedExitFixture(t, svc, "orphan", "", 1)
	if err := svc.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.StopSession("orphan"); err == nil {
		t.Fatal("closed storage error hidden")
	}
}

func TestRecordedExitRemoteLiveAndTransportError(t *testing.T) {
	r := newIdentityRig(t, workerwire.FeatureSessionCred)
	if _, err := r.launch("remote-live", r.urn); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.DB().Exec(`UPDATE sessions SET pid=0,ended_at='old',exit_code=1 WHERE id='remote-live'`); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.svc.reconcileRecordedExit(context.Background(), "remote-live"); err != nil || changed {
		t.Fatalf("live remote reconciled: %t %v", changed, err)
	}
	r.failStops.Store(true)
	if err := r.svc.StopSession("remote-live"); err == nil {
		t.Fatal("worker refusal hidden")
	}
	row, err := r.db.GetSession("remote-live")
	if err != nil || row.State != "running" {
		t.Fatalf("live remote row: %+v %v", row, err)
	}
	// Neither an absent hub handle nor a disconnected worker proves exit.
	if err := r.db.CreateSession(store.SessionRow{ID: "unreachable", WorkerID: "absent", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.StopSession("unreachable"); err == nil {
		t.Fatal("unreachable worker hidden")
	}
}

func TestRecordedExitPeriodicBudgetAndCursor(t *testing.T) {
	svc := stopHarness(t)
	// An entire first page remains in flight. The cursor must still advance.
	var unlocks []func()
	for i := 0; i < 96; i++ {
		id := fmt.Sprintf("recorded-%03d", i)
		recordedExitFixture(t, svc, id, "", 1)
		if i < 32 {
			unlock, err := svc.lockSessionLaunch(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			unlocks = append(unlocks, unlock)
		}
	}
	for i := 0; i < 40; i++ {
		if err := svc.Store.CreateSession(store.SessionRow{ID: fmt.Sprintf("ordinary-%03d", i), State: "launching"}, &launch.Plan{}); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}()
	for tick := 0; tick < 3; tick++ {
		if err := svc.SweepSessionReaper(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := svc.reaperState().lastExamined; got != reaperMaxPerSweep {
			t.Fatalf("ordinary work starved or budget exceeded: examined=%d", got)
		}
		var settled int
		if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM sessions WHERE state='failed'`).Scan(&settled); err != nil {
			t.Fatal(err)
		}
		want := tick * 32
		if settled != want {
			t.Fatalf("tick %d settled=%d want=%d", tick, settled, want)
		}
		var orphaned int
		if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM sessions WHERE state='orphaned'`).Scan(&orphaned); err != nil {
			t.Fatal(err)
		}
		if orphaned != 0 {
			t.Fatal("unvisited recorded exits were orphaned")
		}
	}
}

func TestRecordedExitStartupWalksPages(t *testing.T) {
	svc := stopHarness(t)
	for i := 0; i < 70; i++ {
		recordedExitFixture(t, svc, fmt.Sprintf("recorded-%03d", i), "", 0)
	}
	svc.ReconcileStaleState()
	var settled int
	if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM sessions WHERE state='completed' AND exit_code=0 AND ended_at='2026-11-19T00:00:00Z'`).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled != 70 {
		t.Fatalf("settled=%d", settled)
	}
}

func TestRecordedExitStartupFailurePreservesObservedOutcome(t *testing.T) {
	for _, failure := range []string{"write-error", "expired-budget"} {
		t.Run(failure, func(t *testing.T) {
			svc := stopHarness(t)
			recordedExitFixture(t, svc, "recorded", "", 1)
			ctx := context.Background()
			if failure == "write-error" {
				if _, err := svc.Store.DB().Exec(`CREATE TRIGGER refuse_recorded_exit BEFORE UPDATE OF state ON sessions WHEN NEW.id='recorded' AND NEW.state='failed' AND NEW.exit_code=1 BEGIN SELECT RAISE(ABORT,'injected repair failure'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				cancel()
				ctx = expired
			}
			svc.reconcileStaleStateContext(ctx)
			row, err := svc.Store.GetSession("recorded")
			if err != nil || row.State != "running" || row.ExitCode.Int64 != 1 || row.EndedAt.String != "2026-11-19T00:00:00Z" {
				t.Fatalf("recorded outcome overwritten: %+v %v", row, err)
			}
			if failure == "write-error" {
				if _, err := svc.Store.DB().Exec(`DROP TRIGGER refuse_recorded_exit`); err != nil {
					t.Fatal(err)
				}
			}
			svc.ReconcileStaleState()
			row, err = svc.Store.GetSession("recorded")
			if err != nil || row.State != "failed" || row.ExitCode.Int64 != 1 || row.EndedAt.String != "2026-11-19T00:00:00Z" {
				t.Fatalf("repair did not remain retryable: %+v %v", row, err)
			}
		})
	}
}

func TestRecordedExitCancelledConcurrentSweeps(t *testing.T) {
	svc := stopHarness(t)
	for i := 0; i < 70; i++ {
		recordedExitFixture(t, svc, fmt.Sprintf("canceled-%03d", i), "", 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- svc.SweepSessionReaper(ctx, time.Now()) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation hidden: %v", err)
		}
	}
	var running int
	if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM sessions WHERE state='running'`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 70 {
		t.Fatalf("canceled sweep changed %d rows", 70-running)
	}
	// Concurrent successful passes serialize and share the same cursor/budget.
	for i := 0; i < 2; i++ {
		go func() { done <- svc.SweepSessionReaper(context.Background(), time.Now()) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM sessions WHERE state='failed'`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 64 {
		t.Fatalf("two bounded sweeps settled=%d", running)
	}
}

func TestTeamSessionRecoveryReadyGuards(t *testing.T) {
	svc := stopHarness(t)
	for _, id := range []string{"ended-ready", "pending-ready", "remote-ready", "pid-ready"} {
		worker := ""
		if id == "remote-ready" {
			worker = "worker"
		}
		recordedExitFixture(t, svc, id, worker, 1)
		if _, err := svc.reconcileRecordedExit(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET pid=123456 WHERE id='pid-ready'`); err != nil {
		t.Fatal(err)
	}
	unlock, err := svc.lockSessionLaunch(context.Background(), "pending-ready")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	rt, err := stub.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	startSession(t, svc, "live-ready", rt)
	if err = svc.Store.UpdateSessionState("live-ready", "failed", 0, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.Store.UpdateSessionState("live-ready", "running", 0, nil)
		_ = svc.StopSession("live-ready")
	})
	for _, id := range []string{"ended-ready", "pending-ready", "remote-ready", "pid-ready", "live-ready"} {
		ready, err := svc.TeamSessionRecoveryReady(context.Background(), id)
		if err != nil || ready != (id == "ended-ready") {
			t.Fatalf("%s ready=%v err=%v", id, ready, err)
		}
	}
}
