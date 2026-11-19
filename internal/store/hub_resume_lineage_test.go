package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestHubResumeLineageExactStoredPlans(t *testing.T) {
	for _, kind := range []string{"valid", "historical", "missing-source-plan", "bad-plan", "source-agent", "next-agent", "plan-agent", "plan-parent", "plain-relaunch", "source-worker", "next-worker", "source-team", "next-team", "custody", "detached", "unknown", "created"} {
		t.Run(kind, func(t *testing.T) {
			db := openTestStoreForSessions(t)
			source := SessionRow{ID: "old", LogicalAgentID: "agent", State: "orphaned"}
			next := SessionRow{ID: "new", LogicalAgentID: "agent", State: "running", Intent: "resume", ParentSessionID: sql.NullString{String: "old", Valid: true}}
			sp := &launch.Plan{LogicalAgentID: "agent"}
			np := &launch.Plan{LogicalAgentID: "agent", ResumeSourceSessionID: "old"}
			switch kind {
			case "historical":
				next.State = "orphaned"
			case "source-agent":
				source.LogicalAgentID = "other"
			case "next-agent":
				next.LogicalAgentID = "other"
			case "plan-agent":
				np.LogicalAgentID = "other"
			case "plan-parent":
				np.ResumeSourceSessionID = "other"
			case "plain-relaunch":
				np.ResumeSourceSessionID = ""
				np.Continuity = "relaunch"
			case "source-worker":
				source.WorkerID = "worker"
			case "next-worker":
				next.WorkerID = "worker"
			case "source-team":
				sp.TeamMember = true
			case "next-team":
				np.TeamMember = true
			case "detached":
				source.State = "detached"
			case "unknown":
				source.State = "unknown"
			case "created":
				next.State = "created"
			}
			if err := db.CreateSession(source, sp); err != nil {
				t.Fatal(err)
			}
			if err := db.CreateSession(next, np); err != nil {
				t.Fatal(err)
			}
			if kind == "missing-source-plan" {
				if _, err := db.DB().Exec(`DELETE FROM launch_plans WHERE session_id='old'`); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "bad-plan" {
				if _, err := db.DB().Exec(`UPDATE launch_plans SET plan_json='{' WHERE session_id='new'`); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "custody" {
				row := shimTestRow()
				row.SessionID = "old"
				if err := db.UpsertSessionShim(context.Background(), row); err != nil {
					t.Fatal(err)
				}
			}
			proof, err := db.GetHubResumeLineage(context.Background(), "new")
			if kind != "valid" && kind != "historical" {
				if proof != nil {
					t.Fatalf("invalid lineage: %+v %v", proof, err)
				}
				return
			}
			if err != nil || proof == nil || proof.Kind != "hub_resume_v1" || proof.SourceSessionID != "old" || proof.ReplacementSessionID != "new" || proof.LogicalAgentID != "agent" {
				t.Fatalf("proof: %+v %v", proof, err)
			}
			native, err := db.GetWorkerNativeAdmission("new")
			if err != nil || native != nil {
				t.Fatalf("native promotion: %+v %v", native, err)
			}
		})
	}
}
