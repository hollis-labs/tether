package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/checkpoint"
)

func TestCheckpoints_CreateGetList(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ck.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Seed a logical agent so checkpoints have a legal parent.
	if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: "claude-code", Name: "Claude"}, "2026-04-19T10:00:00Z"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	ck1 := checkpoint.Checkpoint{
		ID:                 "ck-1",
		LogicalAgentID:     "claude-code",
		TaskID:             "T-v002-s03-03",
		WorkflowID:         "sprint-3",
		Status:             "in-progress",
		CompletedWork:      "migration + model written",
		PendingWork:        "CRUD tests",
		Summary:            "checkpoints storage primitive lands",
		NextRecommendation: "pick up with broker_envelopes next",
		CreatedAt:          "2026-04-19T10:05:00Z",
	}
	ck2 := checkpoint.Checkpoint{
		ID:             "ck-2",
		LogicalAgentID: "claude-code",
		Status:         "complete",
		Summary:        "sprint 3 done",
		CreatedAt:      "2026-04-19T11:00:00Z",
	}
	for _, c := range []checkpoint.Checkpoint{ck1, ck2} {
		if err := db.CreateCheckpoint(c); err != nil {
			t.Fatalf("create %q: %v", c.ID, err)
		}
	}

	got, err := db.GetCheckpoint("ck-1")
	if err != nil {
		t.Fatalf("get ck-1: %v", err)
	}
	if got.Status != "in-progress" || got.TaskID != "T-v002-s03-03" {
		t.Errorf("round-trip wrong: %+v", got)
	}
	if got.Summary != "checkpoints storage primitive lands" {
		t.Errorf("summary wrong: %q", got.Summary)
	}
	// Optional columns that weren't set should come back as empty strings.
	if got.KeyDecisions != "" || got.ReferencedArtifacts != "" {
		t.Errorf("unset nullable fields leaked: %+v", got)
	}

	// Newest-first order.
	rows, err := db.ListCheckpointsByLogicalAgent("claude-code")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len=%d, want 2", len(rows))
	}
	if rows[0].ID != "ck-2" || rows[1].ID != "ck-1" {
		t.Errorf("order wrong: %v %v", rows[0].ID, rows[1].ID)
	}
}

func TestCheckpoints_GetMissing(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ck2.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.GetCheckpoint("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestCheckpoints_ProviderHints_RoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ck4.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: "a1", Name: "Agent"}, "2026-04-21T00:00:00Z"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	c := checkpoint.Checkpoint{
		ID:                "ck-hints",
		LogicalAgentID:    "a1",
		Summary:           "with hints",
		ProviderHintsJSON: `{"session_id":"abc123"}`,
		CreatedAt:         "2026-04-21T10:00:00Z",
	}
	if err := db.CreateCheckpoint(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetCheckpoint("ck-hints")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProviderHintsJSON != `{"session_id":"abc123"}` {
		t.Errorf("ProviderHintsJSON = %q, want %q", got.ProviderHintsJSON, `{"session_id":"abc123"}`)
	}
}

func TestCheckpoints_GetLatestForAgent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ck5.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: "ag1", Name: "Agent"}, "2026-04-21T00:00:00Z"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	for _, c := range []checkpoint.Checkpoint{
		{ID: "old", LogicalAgentID: "ag1", Summary: "old", CreatedAt: "2026-04-21T09:00:00Z"},
		{ID: "new", LogicalAgentID: "ag1", Summary: "new", CreatedAt: "2026-04-21T10:00:00Z"},
	} {
		if err := db.CreateCheckpoint(c); err != nil {
			t.Fatalf("create %s: %v", c.ID, err)
		}
	}

	latest, err := db.GetLatestCheckpointForAgent("ag1")
	if err != nil {
		t.Fatalf("GetLatestCheckpointForAgent: %v", err)
	}
	if latest.ID != "new" {
		t.Errorf("latest.ID = %q, want %q", latest.ID, "new")
	}
}

func TestCheckpoints_GetLatestForAgent_None(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ck6.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, err = db.GetLatestCheckpointForAgent("no-such-agent")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestLogicalAgent_LaunchID_RoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "la.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: "la1", Name: "A"}, "2026-04-21T00:00:00Z"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := db.SetLogicalAgentLaunchID("la1", "my-launch"); err != nil {
		t.Fatalf("SetLogicalAgentLaunchID: %v", err)
	}

	row, err := db.GetLogicalAgent("la1")
	if err != nil {
		t.Fatalf("GetLogicalAgent: %v", err)
	}
	if row.LaunchID != "my-launch" {
		t.Errorf("LaunchID = %q, want %q", row.LaunchID, "my-launch")
	}
}

func TestCheckpoints_RejectsMissingRequiredFields(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ck3.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	cases := []struct {
		name string
		c    checkpoint.Checkpoint
	}{
		{"no id", checkpoint.Checkpoint{LogicalAgentID: "a", CreatedAt: "2026"}},
		{"no agent", checkpoint.Checkpoint{ID: "x", CreatedAt: "2026"}},
		{"no created_at", checkpoint.Checkpoint{ID: "x", LogicalAgentID: "a"}},
	}
	for _, tc := range cases {
		if err := db.CreateCheckpoint(tc.c); err == nil {
			t.Errorf("case %q: expected error, got nil", tc.name)
		}
	}
}
