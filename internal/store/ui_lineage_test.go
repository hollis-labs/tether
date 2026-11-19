package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func workerLineageFixture(t *testing.T, explicit bool) (*Store, string, WorkerLaunchAcknowledgement) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(SessionRow{ID: "source", LogicalAgentID: "seat", WorkerID: "old-worker", State: "completed"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	p := &launch.Plan{}
	if explicit {
		p.Continuity = "relaunch"
	}
	if err := db.CreateSession(SessionRow{ID: "child", LogicalAgentID: "seat", WorkerID: "worker", Intent: "resume", ParentSessionID: sql.NullString{String: "source", Valid: true}, State: "launching"}, p); err != nil {
		t.Fatal(err)
	}
	return db, path, WorkerLaunchAcknowledgement{SessionID: "child", WorkerID: "worker", CommandID: "launch:child", ReplyKind: "launched", ReplySessionID: "child", ReplyCommandID: "launch:child"}
}

func TestWorkerLineageRequiresExplicitProvenanceAndExactAck(t *testing.T) {
	for _, name := range []string{"success", "state-only", "wrong-session", "wrong-command", "wrong-kind", "wrong-worker", "generic-resume"} {
		t.Run(name, func(t *testing.T) {
			db, _, ack := workerLineageFixture(t, name != "generic-resume")
			switch name {
			case "wrong-session":
				ack.ReplySessionID = "another"
			case "wrong-command":
				ack.ReplyCommandID = "launch:another"
			case "wrong-kind":
				ack.ReplyKind = "state"
			case "wrong-worker":
				ack.WorkerID = "another"
			}
			if name != "state-only" {
				err := db.RecordWorkerLineageAck(context.Background(), ack)
				bad := name != "success" && name != "generic-resume"
				if (err != nil) != bad {
					t.Fatalf("ack=%+v err=%v", ack, err)
				}
			}
			if err := db.UpdateSessionState("child", "running", 7, nil); err != nil {
				t.Fatal(err)
			}
			r, err := db.GetWorkerLineage(context.Background(), "child")
			if err != nil {
				t.Fatal(err)
			}
			if name == "success" {
				if r == nil || r.Kind != "worker_lineage_v1" || r.SourceSessionID != "source" || r.ReplacementSessionID != "child" || r.LogicalAgentID != "seat" || r.SourceWorkerID != "old-worker" || r.WorkerID != "worker" || r.CommandID != "launch:child" || r.AcknowledgedAt == "" {
					t.Fatalf("receipt=%+v", r)
				}
			} else if r != nil {
				t.Fatalf("unproven attempt minted receipt: %+v", r)
			}
			native, err := db.GetWorkerNativeAdmission("child")
			if err != nil || native != nil {
				t.Fatalf("UI proof became native: %+v %v", native, err)
			}
		})
	}
}

func TestWorkerLineagePersistsAndReplayDoesNotUpgradeEvidence(t *testing.T) {
	db, path, ack := workerLineageFixture(t, true)
	if err := db.RecordWorkerLineageAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionState("child", "running", 7, nil); err != nil {
		t.Fatal(err)
	}
	first, err := db.GetWorkerLineage(context.Background(), "child")
	if err != nil || first == nil {
		t.Fatalf("first=%+v %v", first, err)
	}
	if err := db.RecordWorkerLineageAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	second, err := db.GetWorkerLineage(context.Background(), "child")
	if err != nil || *first != *second {
		t.Fatalf("replay changed receipt: %+v %+v %v", first, second, err)
	}
	if _, err := db.DB().Exec(`UPDATE worker_lineage_receipts SET command_id='forged'`); err == nil {
		t.Fatal("receipt was mutable")
	}
	if _, err := db.DB().Exec(`UPDATE session_relaunch_provenance SET source_session_id='forged'`); err == nil {
		t.Fatal("provenance was mutable")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	third, err := reopened.GetWorkerLineage(context.Background(), "child")
	if err != nil || third == nil || *first != *third {
		t.Fatalf("restart changed receipt: %+v %+v %v", first, third, err)
	}
}

func TestWorkerLineageRejectsConflictingRowsAndCopiedReceipt(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE sessions SET logical_agent_id='other' WHERE id='source'`,
		`UPDATE sessions SET worker_id='other' WHERE id='child'`,
		`UPDATE sessions SET parent_session_id='other' WHERE id='child'`,
		`UPDATE sessions SET intent='fresh' WHERE id='child'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			db, _, ack := workerLineageFixture(t, true)
			if err := db.RecordWorkerLineageAck(context.Background(), ack); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if r, err := db.GetWorkerLineage(context.Background(), "child"); err == nil || r != nil {
				t.Fatalf("conflict accepted: %+v %v", r, err)
			}
		})
	}
	db, _, ack := workerLineageFixture(t, true)
	if err := db.RecordWorkerLineageAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(SessionRow{ID: "copy", LogicalAgentID: "seat", WorkerID: "worker", Intent: "resume", ParentSessionID: sql.NullString{String: "source", Valid: true}, State: "running"}, &launch.Plan{Continuity: "relaunch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`INSERT INTO worker_lineage_receipts SELECT 'copy',source_session_id,logical_agent_id,source_worker_id,worker_id,command_id,acknowledged_at FROM worker_lineage_receipts WHERE session_id='child'`); err != nil {
		t.Fatal(err)
	}
	if r, err := db.GetWorkerLineage(context.Background(), "copy"); err == nil || r != nil {
		t.Fatalf("copied receipt accepted: %+v %v", r, err)
	}
}

func TestWorkerLineageCannotBeSuppliedInPlanJSON(t *testing.T) {
	db, _, ack := workerLineageFixture(t, false)
	var p launch.Plan
	if err := json.Unmarshal([]byte(`{"continuity":"relaunch","_worker_lineage":{"kind":"worker_lineage_v1"}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Continuity != "" {
		t.Fatal("JSON set server-only create provenance")
	}
	if err := db.RecordWorkerLineageAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if r, err := db.GetWorkerLineage(context.Background(), "child"); err != nil || r != nil {
		t.Fatalf("generic lineage minted proof: %+v %v", r, err)
	}
}

func TestWorkerLineageAckEligibilityIsSeparateFromReadSettlement(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		db, _, ack := workerLineageFixture(t, true)
		if err := db.UpdateSessionState("source", "running", 4, nil); err != nil {
			t.Fatal(err)
		}
		stamp := ""
		if acknowledged {
			// Do not add a new launch rejection for an unsettled parent. This is
			// historical exact acknowledgement evidence, not current UI eligibility.
			if err := db.RecordWorkerLineageAck(context.Background(), ack); err != nil {
				t.Fatal(err)
			}
			if err := db.DB().QueryRow(`SELECT acknowledged_at FROM worker_lineage_receipts WHERE session_id='child'`).Scan(&stamp); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.UpdateSessionState("child", "running", 7, nil); err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"running", "detached", "orphaned"} {
			if err := db.UpdateSessionState("source", state, 4, nil); err != nil {
				t.Fatal(err)
			}
			if r, err := db.GetWorkerLineage(context.Background(), "child"); err != nil || r != nil {
				t.Fatalf("unsettled source projected: %+v %v", r, err)
			}
		}
		if err := db.UpdateSessionState("source", "completed", 0, nil); err != nil {
			t.Fatal(err)
		}
		r, err := db.GetWorkerLineage(context.Background(), "child")
		if err != nil {
			t.Fatal(err)
		}
		if acknowledged {
			if r == nil || r.AcknowledgedAt != stamp {
				t.Fatalf("settlement lost earned ack: %+v", r)
			}
		} else if r != nil {
			t.Fatalf("settlement invented ack: %+v", r)
		}
	}
}
