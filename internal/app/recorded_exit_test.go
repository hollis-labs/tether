package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
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
			for i := 0; i < 2; i++ {
				if entry == "startup" {
					svc.ReconcileStaleState()
				} else if err := svc.SweepSessionReaper(context.Background(), time.Now()); err != nil {
					t.Fatal(err)
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
