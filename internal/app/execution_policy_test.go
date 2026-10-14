package app

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/execpolicy"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func TestLaunchSession_RefusedByExecutionPolicy(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := &Service{Store: db}
	add := func(id, project, state string) {
		row := store.SessionRow{ID: id, LaunchID: "l", ProjectID: project, LogicalAgentID: "a", ProviderID: "pv", ProviderKind: "cli", Workspace: t.TempDir(), State: state}
		if err := db.CreateSession(row, &launch.Plan{LaunchID: "l"}); err != nil {
			t.Fatal(err)
		}
	}
	add("live", "p1", string(session.StateRunning))
	add("new", "p1", string(session.StateCreated))

	for _, tc := range []struct {
		name   string
		policy execpolicy.Policy
		code   string
	}{
		{"paused", execpolicy.Policy{Paused: true, Reason: "night"}, execpolicy.CodePaused},
		{"environment cap", execpolicy.Policy{MaxSessions: 1}, execpolicy.CodeEnvironmentCapFull},
		{"project cap", execpolicy.Policy{ProjectCaps: map[string]int{"p1": 1}}, execpolicy.CodeProjectCapFull},
	} {
		if _, err := svc.SetExecutionPolicy(tc.policy); err != nil {
			t.Fatal(err)
		}
		_, err := svc.LaunchSession("new")
		var r *execpolicy.Refusal
		if !errors.As(err, &r) || r.Code != tc.code {
			t.Fatalf("%s: err = %v, want refusal %s", tc.name, err, tc.code)
		}
		if row, _ := db.GetSession("new"); row.State != string(session.StateCreated) {
			t.Fatalf("%s: refused session moved to %q", tc.name, row.State)
		}
	}

	// A cap on another project, or a roomy cap, admits.
	if _, err := svc.SetExecutionPolicy(execpolicy.Policy{MaxSessions: 5, ProjectCaps: map[string]int{"other": 1}}); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.admitLaunch("s-new", "p1")
	if err == nil {
		rel()
	}
	if err != nil {
		t.Fatalf("admitLaunch = %v, want nil", err)
	}
	st, err := svc.ExecutionStatus()
	if err != nil || st.Usage.Total != 1 || st.Policy.MaxSessions != 5 {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestAdmitLaunch_ReservesSlot(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := &Service{Store: db}
	for _, id := range []string{"a", "b"} {
		row := store.SessionRow{ID: id, LaunchID: "l", ProjectID: "p1", LogicalAgentID: "x", ProviderID: "pv", ProviderKind: "cli", Workspace: t.TempDir(), State: string(session.StateCreated)}
		if err := db.CreateSession(row, &launch.Plan{LaunchID: "l"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.SetExecutionPolicy(execpolicy.Policy{MaxSessions: 1}); err != nil {
		t.Fatal(err)
	}
	relA, err := svc.admitLaunch("a", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.admitLaunch("b", "p1"); err == nil {
		t.Fatal("second concurrent admit passed a cap of 1")
	}
	if v, err := svc.Capacity(); err != nil || v.Admission != "full" || v.ActiveSessions != 1 {
		t.Fatalf("capacity must count the in-flight reservation: %+v, %v", v, err)
	}
	relA()
	relB, err := svc.admitLaunch("b", "p1")
	if err != nil {
		t.Fatalf("admit after release: %v", err)
	}
	relB()

	v, err := svc.Capacity()
	if err != nil || v.Admission != "open" || v.MaxSessions != 1 || v.ActiveSessions != 0 {
		t.Fatalf("capacity = %+v, %v", v, err)
	}
}
