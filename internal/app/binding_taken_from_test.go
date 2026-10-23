package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
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
