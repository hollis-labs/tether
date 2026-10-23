package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func bindingFixture(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "taken.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Service{Store: st, Registry: registry.NewService(registry.NewStorage(st.DB()))}
}

func addSession(t *testing.T, svc *Service, id string, state session.State) {
	t.Helper()
	row := store.SessionRow{ID: id, LaunchID: "l1", ProjectID: "p", LogicalAgentID: "general", ProviderID: "stub", Workspace: t.TempDir(), State: string(state)}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatal(err)
	}
}

// CW-20261023-0018: a launch still takes the logical agent's address (last launch wins,
// unchanged), and says who it displaced when that session is still running.
func TestLeaseActorBindingNamesTheRunningSessionItDisplaces(t *testing.T) {
	svc := bindingFixture(t)
	addSession(t, svc, "a", session.StateRunning)
	addSession(t, svc, "b", session.StateRunning)
	addSession(t, svc, "c", session.StateRunning)

	if got := svc.leaseActorBinding("a", "general"); got != "" {
		t.Fatalf("first launch displaced %q, want nobody", got)
	}
	if got := svc.leaseActorBinding("b", "general"); got != "a" {
		t.Fatalf("second launch displaced %q, want a (still running)", got)
	}
	cur, err := svc.Registry.CurrentBinding(context.Background(), registry.LogicalAgentBindingTarget("general"))
	if err != nil || cur.SessionID != "b" {
		t.Fatalf("current binding = %+v err=%v, want session b (last launch wins is unchanged)", cur, err)
	}
	// An ended holder is not reported.
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET state=? WHERE id='b'`, string(session.StateCompleted)); err != nil {
		t.Fatal(err)
	}
	if got := svc.leaseActorBinding("c", "general"); got != "" {
		t.Fatalf("launch over an ended holder displaced %q, want nobody", got)
	}
	// Re-leasing the session that already holds it displaces nobody.
	if got := svc.leaseActorBinding("c", "general"); got != "" {
		t.Fatalf("re-lease by the holder displaced %q", got)
	}
}

// Under concurrent launches binding_taken_from is the holder each insert actually
// displaced: compared with the committed generations in order (Reviewer hold on #504).
func TestConcurrentReplaceReportsTheActualDisplacedHolder(t *testing.T) {
	svc := bindingFixture(t)
	const n = 20
	for i := 0; i < n; i++ {
		addSession(t, svc, fmt.Sprintf("s%d", i), session.StateRunning)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	reported := map[string]string{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("s%d", i)
			<-start
			taken := svc.leaseActorBinding(id, "general")
			mu.Lock()
			reported[id] = taken
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	rows, err := svc.Store.DB().Query(`SELECT session_id FROM runtime_bindings WHERE target_urn=? ORDER BY generation`, registry.LogicalAgentBindingTarget("general"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	prev, seen := "", 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		seen++
		if reported[id] != prev {
			t.Errorf("%s reported displaced=%q, actual preceding holder=%q", id, reported[id], prev)
		}
		prev = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != n {
		t.Fatalf("%d generations, want %d", seen, n)
	}
}

func currentHolder(t *testing.T, svc *Service) string {
	t.Helper()
	cur, err := svc.Registry.CurrentBinding(context.Background(), registry.LogicalAgentBindingTarget("general"))
	if err != nil {
		return ""
	}
	return cur.SessionID
}

// lifecycle.binding: replace is today's behavior; keep and none leave a running holder
// alone; a resume of the holder still takes the address under keep.
func TestLeaseActorBindingModes(t *testing.T) {
	svc := bindingFixture(t)
	for _, id := range []string{"holder", "x", "y", "z", "resume"} {
		addSession(t, svc, id, session.StateRunning)
	}
	svc.leaseActorBinding("holder", "general")

	// none never takes the address.
	if taken, held := svc.leaseActorBindingMode("x", "general", launchprofile.BindingNone, ""); taken != "" || held != "" || currentHolder(t, svc) != "holder" {
		t.Fatalf("none: taken=%q held=%q holder=%q", taken, held, currentHolder(t, svc))
	}
	// keep leaves a running holder in place and says who kept it.
	if taken, held := svc.leaseActorBindingMode("y", "general", launchprofile.BindingKeep, ""); taken != "" || held != "holder" || currentHolder(t, svc) != "holder" {
		t.Fatalf("keep over a running holder: taken=%q held=%q holder=%q", taken, held, currentHolder(t, svc))
	}
	// keep still takes it for a resume or replacement of the holder.
	if taken, held := svc.leaseActorBindingMode("resume", "general", launchprofile.BindingKeep, "holder"); taken != "holder" || held != "" || currentHolder(t, svc) != "resume" {
		t.Fatalf("keep for the holder's resume: taken=%q held=%q holder=%q", taken, held, currentHolder(t, svc))
	}
	// keep takes a free address, and one whose holder has ended.
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET state=? WHERE id='resume'`, string(session.StateCompleted)); err != nil {
		t.Fatal(err)
	}
	if taken, held := svc.leaseActorBindingMode("y", "general", launchprofile.BindingKeep, ""); taken != "" || held != "" || currentHolder(t, svc) != "y" {
		t.Fatalf("keep over an ended holder: taken=%q held=%q holder=%q", taken, held, currentHolder(t, svc))
	}
	// replace (and an unset mode) is last launch wins, unchanged.
	if taken, _ := svc.leaseActorBindingMode("z", "general", launchprofile.BindingReplace, ""); taken != "y" || currentHolder(t, svc) != "z" {
		t.Fatalf("replace: taken=%q holder=%q", taken, currentHolder(t, svc))
	}
	if got := (*launchprofile.LifecyclePolicy)(nil).BindingMode(); got != launchprofile.BindingReplace {
		t.Fatalf("unset mode = %q, want replace", got)
	}
}

// Concurrent keep launches against a free address: exactly one claims it and none
// displaces a running holder (Reviewer hold on #505, CW-20261023-0018).
func TestConcurrentKeepLaunchesClaimTheAddressOnce(t *testing.T) {
	svc := bindingFixture(t)
	const n = 20
	for i := 0; i < n; i++ {
		addSession(t, svc, fmt.Sprintf("s%d", i), session.StateRunning)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	claims := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			taken, held := svc.leaseActorBindingMode(fmt.Sprintf("s%d", i), "general", launchprofile.BindingKeep, "")
			mu.Lock()
			defer mu.Unlock()
			if held == "" {
				claims++
			}
			if taken != "" {
				t.Errorf("keep displaced running holder %s", taken)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if claims != 1 {
		t.Fatalf("%d launches claimed the address, want exactly one", claims)
	}
}

// keep and replace launches racing: a keep launch never displaces anyone, and the
// address ends up with a session that actually claimed it.
func TestConcurrentKeepAndReplaceLaunches(t *testing.T) {
	svc := bindingFixture(t)
	const n = 20
	for i := 0; i < n; i++ {
		addSession(t, svc, fmt.Sprintf("s%d", i), session.StateRunning)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[string]bool{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("s%d", i)
			mode := launchprofile.BindingKeep
			if i%2 == 0 {
				mode = launchprofile.BindingReplace
			}
			<-start
			taken, held := svc.leaseActorBindingMode(id, "general", mode, "")
			mu.Lock()
			defer mu.Unlock()
			if held == "" {
				claimed[id] = true
			}
			if mode == launchprofile.BindingKeep && taken != "" {
				t.Errorf("keep launch %s displaced running holder %s", id, taken)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if h := currentHolder(t, svc); !claimed[h] {
		t.Fatalf("the address ended with %q, which did not claim it (claimed: %v)", h, claimed)
	}
}

// A holder's resume and unrelated keep launches racing: only a resume of the holder
// may take the address from it, and only once.
func TestConcurrentResumeOfTheHolderUnderKeep(t *testing.T) {
	svc := bindingFixture(t)
	addSession(t, svc, "holder", session.StateRunning)
	svc.leaseActorBinding("holder", "general")
	const n = 20
	for i := 0; i < n; i++ {
		addSession(t, svc, fmt.Sprintf("s%d", i), session.StateRunning)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var claimants []string
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resume := ""
			if i%2 == 0 {
				resume = "holder"
			}
			<-start
			taken, held := svc.leaseActorBindingMode(fmt.Sprintf("s%d", i), "general", launchprofile.BindingKeep, resume)
			mu.Lock()
			defer mu.Unlock()
			if held == "" {
				claimants = append(claimants, fmt.Sprintf("s%d(taken=%s,resume=%q)", i, taken, resume))
				if resume == "" || taken != "holder" {
					t.Errorf("s%d claimed without being the holder's resume: taken=%q resume=%q", i, taken, resume)
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if len(claimants) != 1 {
		t.Fatalf("claimants = %v, want exactly one resume of the holder", claimants)
	}
}
