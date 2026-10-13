package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
)

func ghostRow(t *testing.T, svc *Service, id, worker string) {
	t.Helper()
	row := store.SessionRow{ID: id, LaunchID: "l1", ProjectID: "p", ProviderID: "stub", Workspace: t.TempDir(), State: string(session.StateRunning), WorkerID: worker}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatal(err)
	}
}

func TestForceStopOfflineWorkerGhost(t *testing.T) {
	svc := stopHarness(t)
	svc.Workers = workerhub.New(svc.Store.DB())
	ghostRow(t, svc, "ghost", "w1")
	res, err := svc.ForceStopSession(context.Background(), "ghost", api.ForceStopOptions{Reason: "orphan"})
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "running" || res.To != "killed" || res.Reason != "orphan" || res.WasLive {
		t.Fatalf("res = %+v", res)
	}
	row, _ := svc.Store.GetSession("ghost")
	if row.State != "killed" || !row.EndedAt.Valid || row.ExitCode.Int64 != -1 {
		t.Fatalf("row = %+v", row)
	}
	evs, _ := svc.Store.ListEventsBySession("ghost", 10, 0)
	found := false
	for _, e := range evs {
		found = found || e.Kind == sessionForceClosedEvent
	}
	if !found {
		t.Fatal("no session.force_closed event")
	}
	if _, err = svc.ForceStopSession(context.Background(), "ghost", api.ForceStopOptions{}); !errors.Is(err, api.ErrSessionAlreadyTerminal) {
		t.Fatalf("second close err = %v", err)
	}
}

func TestForceStopUnknownRuntimeGhostAndDefaultReason(t *testing.T) {
	svc := stopHarness(t)
	ghostRow(t, svc, "ghost", "")
	res, err := svc.ForceStopSession(context.Background(), "ghost", api.ForceStopOptions{})
	if err != nil || res.Reason == "" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestForceStopRefusesLiveSessionWithoutAllowLive(t *testing.T) {
	svc := stopHarness(t)
	startSession(t, svc, "live", exitRuntime{release: make(chan struct{}), stopCode: -1})
	row, _ := svc.Store.GetSession("live")
	if _, ok := svc.Manager.Get("live"); !ok || row == nil {
		t.Fatal("setup: session not live")
	}
	if err := svc.Store.UpdateSessionState("live", "running", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ForceStopSession(context.Background(), "live", api.ForceStopOptions{}); !errors.Is(err, api.ErrSessionLive) {
		t.Fatalf("err = %v, want ErrSessionLive", err)
	}
	if r, _ := svc.Store.GetSession("live"); r.State != "running" {
		t.Fatalf("live row changed: %s", r.State)
	}
	res, err := svc.ForceStopSession(context.Background(), "live", api.ForceStopOptions{AllowLive: true})
	if err != nil || !res.WasLive {
		t.Fatalf("allow_live res = %+v err = %v", res, err)
	}
	_ = svc.Manager.Stop(context.Background(), "live")
}

func TestForceStopUnknownSession(t *testing.T) {
	svc := stopHarness(t)
	if _, err := svc.ForceStopSession(context.Background(), "nope", api.ForceStopOptions{}); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
}
