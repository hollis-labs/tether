package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gopevents "github.com/hollis-labs/libs/harness/core/adapters/provider/events"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestMissingEventBusRefusesBeforeMutation(t *testing.T) {
	for _, op := range []string{"force close", "worker state", "worker output", "reaper sweep", "lifecycle stop", "sandbox downgrade"} {
		t.Run(op, func(t *testing.T) {
			svc := stopHarness(t)
			ghostRow(t, svc, "ghost", "")
			svc.Bus = nil
			var err error
			switch op {
			case "force close":
				_, err = svc.ForceStopSession(context.Background(), "ghost", api.ForceStopOptions{})
			case "worker state":
				err = svc.ingestWorkerFrame("worker", workerwire.Frame{T: "state", Session: "ghost", State: "exited"})
			case "worker output":
				err = svc.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "ghost", Seq: 1, Data: []byte("output")})
			case "reaper sweep":
				err = svc.SweepSessionReaper(context.Background(), time.Now().Add(24*time.Hour))
			case "lifecycle stop":
				err = svc.stopSessionForLifecycle(context.Background(), "ghost", "test", launchprofile.LifecycleDurations{})
			case "sandbox downgrade":
				err = svc.recordUnenforcedUserServiceManager(context.Background(), "ghost")
			}
			if !errors.Is(err, errEventBusUnavailable) {
				t.Fatalf("error = %v", err)
			}
			row, err := svc.Store.GetSession("ghost")
			if err != nil || row.State != "running" || row.EndedAt.Valid {
				t.Fatalf("row=%+v err=%v", row, err)
			}
			if _, err := os.Stat(filepath.Join(row.Workspace, "logs", "session.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worker log created: %v", err)
			}
			es, err := svc.Store.ListEventsBySession("ghost", 10, 0)
			if err != nil || len(es) != 0 {
				t.Fatalf("unaudited fallback wrote events: %+v %v", es, err)
			}
		})
	}
}

func TestMissingEventBusDoesNotCancelTurn(t *testing.T) {
	called := false
	svc, output, _ := interruptHarness(t, func(context.Context) error { called = true; return nil })
	output.observeProvider(gopevents.Delta{Text: "working"})
	svc.Bus = nil
	_, err := svc.CancelTurnAndWait(context.Background(), "s1", "operator")
	if !errors.Is(err, errEventBusUnavailable) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestSandboxDowngradeRequiresDurableAudit(t *testing.T) {
	svc := stopHarness(t)
	if _, err := svc.Store.DB().Exec(`CREATE TRIGGER reject_sandbox_audit BEFORE INSERT ON events BEGIN SELECT RAISE(FAIL,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.recordUnenforcedUserServiceManager(context.Background(), "s1"); err == nil {
		t.Fatal("sandbox downgrade accepted without audit")
	}
	if _, err := svc.Store.DB().Exec(`DROP TRIGGER reject_sandbox_audit`); err != nil {
		t.Fatal(err)
	}
	if err := svc.recordUnenforcedUserServiceManager(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	es, err := svc.Store.ListEventsBySession("s1", 10, 0)
	if err != nil || len(es) != 1 || es[0].Kind != sandboxRuleUnenforcedEvent {
		t.Fatalf("audit=%+v err=%v", es, err)
	}
	if es[0].EventID == "" {
		t.Fatal("audit bypassed event persistence")
	}
}
