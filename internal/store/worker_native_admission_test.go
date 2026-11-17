package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestWorkerNativeAdmissionPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	source := SessionRow{ID: "source", WorkerID: "worker", LogicalAgentID: "agent", State: "completed"}
	if err := db.CreateSession(source, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	plan := &launch.Plan{NativeResumeOnly: true, ResumeSourceSessionID: "source", WorkerSourceGeneration: "source-generation", ResumeProviderSessionID: "thread", WorkRoot: "/worker/root"}
	child := SessionRow{ID: "child", WorkerID: "worker", LogicalAgentID: "agent", State: "launching", Intent: "resume", ParentSessionID: sql.NullString{String: "source", Valid: true}}
	if err := db.CreateSession(child, plan); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "child"} {
		if got, err := db.GetWorkerNativeAdmission(id); err != nil || got != nil {
			t.Fatalf("unconfirmed %s: %+v %v", id, got, err)
		}
	}
	receipt := WorkerNativeAdmission{SourceSessionID: "source", SourceGeneration: "source-generation", ReplacementSessionID: "child", ReplacementGeneration: "child-generation", WorkerID: "worker", LogicalAgentID: "agent", NativeThreadID: "thread", Workroot: "/worker/root"}
	bad := receipt
	bad.SourceGeneration = "stale"
	if err := db.RecordWorkerNativeAdmission(bad); err == nil {
		t.Fatal("stale generation accepted")
	}
	if err := db.RecordWorkerNativeAdmission(receipt); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordWorkerNativeAdmission(receipt); err == nil {
		t.Fatal("receipt overwritten")
	}
	before, err := db.GetWorkerNativeAdmission("child")
	if err != nil || before == nil {
		t.Fatalf("receipt: %+v %v", before, err)
	}
	// Typed plan copies cannot promote the source's admission to another attempt.
	copied, err := db.GetLaunchPlan("child")
	if err != nil {
		t.Fatal(err)
	}
	child.ID = "fresh-copy"
	if err := db.CreateSession(child, copied); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetWorkerNativeAdmission(child.ID); err != nil || got != nil {
		t.Fatalf("inherited proof: %+v %v", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after, err := db.GetWorkerNativeAdmission("child")
	if err != nil || after == nil || *before != *after {
		t.Fatalf("restart lost receipt: %+v %v", after, err)
	}
	if after.Kind != "codex_same_worker_v1" || after.AdmittedAt == "" {
		t.Fatalf("missing server fields: %+v", after)
	}
	// Request-shaped/plan JSON cannot introduce the server-only member through Plan.
	var supplied launch.Plan
	if err := json.Unmarshal([]byte(`{"_worker_native_admission":{"kind":"codex_same_worker_v1"}}`), &supplied); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(SessionRow{ID: "forged", State: "created"}, &supplied); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetWorkerNativeAdmission("forged"); err != nil || got != nil {
		t.Fatalf("request supplied proof: %+v %v", got, err)
	}
}
