package app

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/hollis-labs/tether/internal/workerhub"
	"strings"
	"sync"
	"testing"
	"time"

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

// A cascade reaches worker-placed descendants through their worker: an
// unreachable worker fails the cascade for a live row, but not for a row the
// hub already records as ended, and the rest of the tree is still attempted.
func TestStopSessionCascadeWorkerPlacedDescendants(t *testing.T) {
	svc := bindingHarness(t)
	if _, err := svc.Store.DB().Exec(`INSERT INTO workers(id,label,device_id,platform,caps_json,last_seen,created_at) VALUES('w1','agent-os','d1','linux','{}','','')`); err != nil {
		t.Fatal(err)
	}
	svc.Workers = workerhub.New(svc.Store.DB())
	svc.stops = &stopRequests{}
	lineageSession(t, svc, "root", "")
	workerKid := func(id, parent, state string) {
		t.Helper()
		row := store.SessionRow{ID: id, WorkerID: "w1", LaunchID: "l", ProjectID: "p", ProviderID: "pv", Workspace: t.TempDir(), State: state,
			ParentSessionID: sql.NullString{String: parent, Valid: true}}
		if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l"}); err != nil {
			t.Fatal(err)
		}
	}
	workerKid("ended", "root", "failed")
	if err := svc.StopSessionCascade("root"); err != nil {
		t.Fatalf("ended worker descendant must not fail the cascade: %v", err)
	}
	workerKid("live", "root", "running")
	err := svc.StopSessionCascade("root")
	if err == nil || !strings.Contains(err.Error(), "stop live") {
		t.Fatalf("live worker descendant with unreachable worker: err = %v", err)
	}
}

func TestCheckLineageLaunchRate(t *testing.T) {
	svc := bindingHarness(t)
	lineageSession(t, svc, "root", "")
	lineageSession(t, svc, "kid", "root")
	if err := svc.checkLineage("root"); err != nil {
		t.Fatalf("rate cap off by default: %v", err)
	}
	svc.LineageLimits = LineageLimits{MaxLaunchesPerWindow: 1, LaunchWindow: time.Hour}
	if err := svc.checkLineage("root"); !errors.Is(err, ErrLineageLimit) {
		t.Fatalf("rate cap: %v", err)
	}
	// Ended children still count: the cap bounds rate, not concurrency.
	if err := svc.Store.UpdateSessionState("kid", "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.checkLineage("root"); !errors.Is(err, ErrLineageLimit) {
		t.Fatalf("ended child must count toward the rate: %v", err)
	}
	if err := svc.checkLineage("kid"); err != nil {
		t.Fatalf("other parent unaffected: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET created_at=? WHERE id='kid'`, old); err != nil {
		t.Fatal(err)
	}
	if err := svc.checkLineage("root"); err != nil {
		t.Fatalf("child outside the window must not count: %v", err)
	}
}
