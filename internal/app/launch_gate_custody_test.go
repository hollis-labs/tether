package app

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestLaunchAdmissionRefusesPreviousHolderOfSameGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plan := &launch.Plan{LaunchID: "launch", ProviderID: "claude"}
	row := store.SessionRow{ID: "gate-session", LaunchID: "launch", ProviderID: "claude", State: "created", Workspace: t.TempDir()}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	canonical, err := db.GetSession(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: db}
	release, err := service.lockSessionLaunch(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	prior, err := service.launchArtifactAdmission(ctx, row.ID, canonical, plan)
	if err != nil {
		t.Fatal(err)
	}
	service.launchMu.Lock()
	originalGate := service.launches[row.ID]
	service.launchMu.Unlock()
	type acquired struct {
		release func()
		err     error
	}
	waiter := make(chan acquired, 1)
	go func() {
		unlock, err := service.lockSessionLaunch(ctx, row.ID)
		waiter <- acquired{unlock, err}
	}()
	// Observe actual queued acquisition under the gate's mutex, without a
	// scheduling sleep or exposing a production callback/test hook.
	for {
		service.launchMu.Lock()
		queued := originalGate.refs == 2
		service.launchMu.Unlock()
		if queued {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("waiter did not queue")
		}
		runtime.Gosched()
	}
	release()
	release = nil
	var current acquired
	select {
	case current = <-waiter:
	case <-ctx.Done():
		t.Fatal("queued waiter did not acquire")
	}
	if current.err != nil {
		t.Fatal(current.err)
	}
	defer current.release()
	service.launchMu.Lock()
	sameGate := service.launches[row.ID] == originalGate
	service.launchMu.Unlock()
	if !sameGate {
		t.Fatal("fixture did not preserve the queued gate pointer")
	}
	if err := prior.Validate(ctx); err == nil {
		t.Fatal("previous holder's admission accepted a different holder of the same gate")
	}
	admission, err := service.launchArtifactAdmission(ctx, row.ID, canonical, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.Validate(ctx); err != nil {
		t.Fatalf("current holder refused: %v", err)
	}
	// Cancellation of a different waiter must not invalidate this holder.
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := service.lockSessionLaunch(canceled, row.ID); err == nil {
		t.Fatal("canceled queued acquisition succeeded")
	}
	if err := admission.Validate(ctx); err != nil {
		t.Fatalf("canceled waiter revoked current custody: %v", err)
	}
}
