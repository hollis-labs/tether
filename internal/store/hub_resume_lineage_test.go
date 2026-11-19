package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestHubResumeLineageExactStoredPlans(t *testing.T) {
	for _, kind := range []string{"valid", "historical", "missing-source-plan", "bad-plan", "source-agent", "next-agent", "plan-agent", "plan-parent", "plain-relaunch", "generic-resume", "explicit-plan-mismatch", "source-worker", "next-worker", "source-team", "next-team", "custody", "detached", "unknown", "created"} {
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
			case "explicit-plan-mismatch":
				np.Continuity = "relaunch"
				np.ResumeSourceSessionID = "other"
			case "generic-resume":
				np.ResumeSourceSessionID = ""
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
			if kind != "valid" && kind != "historical" && kind != "plain-relaunch" {
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

func TestHubExplicitProvenanceReloadAndCanonicalCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(SessionRow{ID: "a", LogicalAgentID: "seat", State: "orphaned"}, &launch.Plan{LogicalAgentID: "seat"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(SessionRow{ID: "b", LogicalAgentID: "seat", State: "running", Intent: "resume", ParentSessionID: sql.NullString{String: "a", Valid: true}}, &launch.Plan{LogicalAgentID: "seat", Continuity: "relaunch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE hub_relaunch_provenance SET source_session_id='x'`); err == nil {
		t.Fatal("mutable provenance")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err := db.GetHubResumeLineage(context.Background(), "b")
	if err != nil || p == nil {
		t.Fatalf("reloaded: %+v %v", p, err)
	}
	for _, statement := range []string{
		`UPDATE sessions SET logical_agent_id='forged'`,
		`UPDATE sessions SET worker_id='forged' WHERE id='a'`,
		`UPDATE sessions SET parent_session_id='b' WHERE id='b'`,
		`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.logical_agent_id','forged') WHERE session_id='a'`,
		`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.resume_source_session_id','forged') WHERE session_id='b'`,
	} {
		// Undo each mutation after checking that it cannot reuse the immutable proof.
		if _, err := db.DB().Exec("SAVEPOINT corrupt"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec(statement); err != nil {
			t.Fatal(err)
		}
		p, e := db.GetHubResumeLineage(context.Background(), "b")
		if p != nil {
			t.Fatalf("corruption accepted: %s %+v %v", statement, p, e)
		}
		if _, err := db.DB().Exec("ROLLBACK TO corrupt"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec("RELEASE corrupt"); err != nil {
			t.Fatal(err)
		}
	}
}
