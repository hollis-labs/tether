package app

import (
	"context"
	"path/filepath"
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
