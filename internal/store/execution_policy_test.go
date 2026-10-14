package store

import (
	"testing"

	"github.com/hollis-labs/tether/internal/execpolicy"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestExecutionPolicyRoundTripAndUsage(t *testing.T) {
	db := openTestStoreForSessions(t)
	p, err := db.GetExecutionPolicy()
	if err != nil || p.Paused || p.MaxSessions != 0 {
		t.Fatalf("default = %+v, %v", p, err)
	}
	want := execpolicy.Policy{Paused: true, Reason: "r", MaxSessions: 3, ProjectCaps: map[string]int{"p": 1}}
	if err := db.SetExecutionPolicy(want); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetExecutionPolicy()
	if err != nil || !got.Paused || got.MaxSessions != 3 || got.ProjectCaps["p"] != 1 || got.Reason != "r" {
		t.Fatalf("got %+v, %v", got, err)
	}
	for i, st := range []string{"created", "running", "orphaned", "completed"} {
		row := SessionRow{ID: "s" + string(rune('a'+i)), LaunchID: "l", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp/ws", State: st}
		if err := db.CreateSession(row, &launch.Plan{LaunchID: "l"}); err != nil {
			t.Fatal(err)
		}
	}
	u, err := db.ExecutionUsage()
	if err != nil || u.Total != 2 || u.ByProject["p"] != 2 {
		t.Fatalf("usage = %+v, %v", u, err)
	}
}
