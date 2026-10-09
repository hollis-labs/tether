package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	"github.com/hollis-labs/tether/internal/launchartifacts/testfixture"
	"github.com/hollis-labs/tether/internal/store"
)

func testArtifactAdmission(t testing.TB, plan *launch.Plan) launchartifacts.Admission {
	t.Helper()
	return testfixture.Admission(t, func() any {
		accepted := *plan
		accepted.Shared = nil
		return accepted
	})
}

func TestLaunchArtifactAdmission_RechecksCanonicalDecision(t *testing.T) {
	for _, change := range []string{"state", "plan", "accepted-plan", "gate", "operation"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			plan := &launch.Plan{LaunchID: "launch", ProjectID: "project", LogicalAgentID: "agent", ProviderID: "claude", Command: "claude"}
			row := store.SessionRow{ID: "created-session", LaunchID: plan.LaunchID, ProjectID: plan.ProjectID, LogicalAgentID: plan.LogicalAgentID, ProviderID: plan.ProviderID, Workspace: t.TempDir(), State: "created"}
			if err := db.CreateSession(row, plan); err != nil {
				t.Fatal(err)
			}
			canonicalRow, err := db.GetSession(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			row = *canonicalRow
			svc := &Service{Store: db}
			unlock, err := svc.lockSessionLaunch(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if unlock != nil {
					unlock()
				}
			}()
			admission, err := svc.launchArtifactAdmission(ctx, row.ID, &row, plan)
			if err != nil {
				t.Fatal(err)
			}
			if err := admission.Validate(ctx); err != nil {
				t.Fatalf("accepted launch: %v", err)
			}
			switch change {
			case "state":
				if err := db.UpdateSessionState(row.ID, "running", 123, nil); err != nil {
					t.Fatal(err)
				}
			case "plan":
				if _, err := db.DB().Exec("UPDATE launch_plans SET plan_json = ? WHERE session_id = ?", `{}`, row.ID); err != nil {
					t.Fatal(err)
				}
			case "accepted-plan":
				plan.Command = "substituted-provider"
			case "gate":
				unlock()
				unlock = nil
			case "operation":
				cancel()
			}
			if err := admission.Validate(context.Background()); err == nil {
				t.Fatal("changed launch authority accepted")
			}
		})
	}
}

func TestLaunchArtifactAdmission_RequiresHeldCanonicalSession(t *testing.T) {
	svc := &Service{}
	if _, err := svc.launchArtifactAdmission(context.Background(), "foreign", nil, &launch.Plan{}); err == nil {
		t.Fatal("missing canonical created session accepted")
	}
}
