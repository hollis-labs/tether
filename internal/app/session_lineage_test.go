package app

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func lineageSession(t *testing.T, svc *Service, id, parent string) {
	t.Helper()
	row := store.SessionRow{ID: id, LaunchID: "l", ProjectID: "p", LogicalAgentID: "", ProviderID: "pv", Workspace: t.TempDir(), State: "created",
		ParentSessionID: sql.NullString{String: parent, Valid: parent != ""}}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l"}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckLineageLimits(t *testing.T) {
	svc := bindingHarness(t)
	lineageSession(t, svc, "root", "")
	lineageSession(t, svc, "kid", "root")

	if err := svc.checkLineage("kid"); err != nil {
		t.Fatalf("zero limits must allow: %v", err)
	}
	svc.LineageLimits = LineageLimits{MaxChildren: 1}
	if err := svc.checkLineage("root"); !errors.Is(err, ErrLineageLimit) {
		t.Fatalf("children cap: %v", err)
	}
	// An ended child no longer counts against the cap.
	if err := svc.Store.UpdateSessionState("kid", "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.checkLineage("root"); err != nil {
		t.Fatalf("ended child must not count: %v", err)
	}
	svc.LineageLimits = LineageLimits{MaxDepth: 1}
	if err := svc.checkLineage("root"); err != nil {
		t.Fatalf("depth 1 child of root allowed: %v", err)
	}
	if err := svc.checkLineage("kid"); !errors.Is(err, ErrLineageLimit) {
		t.Fatalf("depth cap: %v", err)
	}
	if err := svc.checkLineage(""); err != nil {
		t.Fatalf("root session: %v", err)
	}
}

func TestStopSessionCascadeSkipsNotRunning(t *testing.T) {
	svc := bindingHarness(t)
	lineageSession(t, svc, "root", "")
	lineageSession(t, svc, "kid", "root")
	if err := svc.StopSessionCascade("root"); err != nil {
		t.Fatalf("cascade over idle tree: %v", err)
	}
}

func TestAdmitChildIsAtomicUnderConcurrency(t *testing.T) {
	svc := bindingHarness(t)
	lineageSession(t, svc, "root", "")
	svc.LineageLimits = LineageLimits{MaxChildren: 2}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- svc.AdmitChild("root", func() error {
				row := store.SessionRow{ID: fmt.Sprintf("kid-%d", i), LaunchID: "l", ProjectID: "p", ProviderID: "pv", Workspace: t.TempDir(), State: "created", ParentSessionID: sql.NullString{String: "root", Valid: true}}
				return svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l"})
			})
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrLineageLimit) {
			t.Fatal(err)
		}
	}
	if ok != 2 {
		t.Fatalf("admitted %d children, want exactly 2", ok)
	}
}
