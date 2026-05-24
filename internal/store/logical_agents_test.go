package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/agent"
)

func TestUpsertLogicalAgent_InsertAndUpdate(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "la.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	first := "2026-04-19T10:00:00Z"
	if err := db.UpsertLogicalAgent(agent.LogicalAgent{
		ID:   "claude-code",
		Role: "backend, storage",
		Name: "Claude Code",
	}, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	got, err := db.GetLogicalAgent("claude-code")
	if err != nil {
		t.Fatalf("get after insert: %v", err)
	}
	if got.Name != "Claude Code" {
		t.Errorf("Name = %q, want Claude Code", got.Name)
	}
	if got.Role != "backend, storage" {
		t.Errorf("Role = %q, want 'backend, storage'", got.Role)
	}
	if got.CreatedAt != first || got.UpdatedAt != first {
		t.Errorf("timestamps = %s / %s, want both = %s", got.CreatedAt, got.UpdatedAt, first)
	}

	// Second upsert must preserve created_at but refresh updated_at and
	// overwrite role + name per the ADR-specified contract. Use explicit
	// distinct timestamps to avoid RFC3339's second-granularity eating the
	// delta when tests run sub-second fast.
	second := "2026-04-19T10:00:05Z"
	if err := db.UpsertLogicalAgent(agent.LogicalAgent{
		ID:   "claude-code",
		Role: "backend",
		Name: "Claude Code CLI",
	}, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err = db.GetLogicalAgent("claude-code")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "Claude Code CLI" {
		t.Errorf("Name after update = %q, want Claude Code CLI", got.Name)
	}
	if got.Role != "backend" {
		t.Errorf("Role after update = %q, want backend", got.Role)
	}
	if got.CreatedAt != first {
		t.Errorf("CreatedAt = %q, want preserved = %q", got.CreatedAt, first)
	}
	if got.UpdatedAt != second {
		t.Errorf("UpdatedAt = %q, want %q", got.UpdatedAt, second)
	}
}

func TestUpsertLogicalAgent_RejectsEmptyID(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	err = db.UpsertLogicalAgent(agent.LogicalAgent{ID: "", Name: "oops"}, "2026-04-19T00:00:00Z")
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
	}
}

func TestGetLogicalAgent_NotFound(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "nf.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, err = db.GetLogicalAgent("missing")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestListLogicalAgents_OrdersByID(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "list.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := "2026-04-19T10:00:00Z"
	for _, id := range []string{"zebra", "alpha", "mike"} {
		if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: id, Name: id}, now); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	rows, err := db.ListLogicalAgents()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len = %d, want 3", len(rows))
	}
	wantOrder := []string{"alpha", "mike", "zebra"}
	for i, r := range rows {
		if r.ID != wantOrder[i] {
			t.Errorf("row %d ID = %q, want %q", i, r.ID, wantOrder[i])
		}
	}
}

func openClaudeSessionTestStore(t *testing.T, id string) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "cs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: id, Name: id}, "2026-04-19T10:00:00Z"); err != nil {
		t.Fatalf("seed logical_agent: %v", err)
	}
	return db
}

func TestClaudeSessionID_NullByDefault(t *testing.T) {
	db := openClaudeSessionTestStore(t, "a1")
	got, err := db.GetClaudeSessionID("a1")
	if err != nil {
		t.Fatalf("GetClaudeSessionID: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string for NULL column, got %q", got)
	}
}

func TestClaudeSessionID_SetThenGetRoundTrips(t *testing.T) {
	db := openClaudeSessionTestStore(t, "a1")
	if err := db.SetClaudeSessionID("a1", "sid-abc-123"); err != nil {
		t.Fatalf("SetClaudeSessionID: %v", err)
	}
	got, err := db.GetClaudeSessionID("a1")
	if err != nil {
		t.Fatalf("GetClaudeSessionID: %v", err)
	}
	if got != "sid-abc-123" {
		t.Fatalf("round-trip mismatch: got %q", got)
	}
}

func TestClaudeSessionID_SetOverwritesPriorValue(t *testing.T) {
	db := openClaudeSessionTestStore(t, "a1")
	if err := db.SetClaudeSessionID("a1", "first"); err != nil {
		t.Fatalf("set first: %v", err)
	}
	if err := db.SetClaudeSessionID("a1", "second"); err != nil {
		t.Fatalf("set second: %v", err)
	}
	got, _ := db.GetClaudeSessionID("a1")
	if got != "second" {
		t.Fatalf("expected latest value, got %q", got)
	}
}

func TestClaudeSessionID_SetEmptyIsNoOp(t *testing.T) {
	db := openClaudeSessionTestStore(t, "a1")
	if err := db.SetClaudeSessionID("a1", "persisted"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.SetClaudeSessionID("a1", ""); err != nil {
		t.Fatalf("set empty should not error, got %v", err)
	}
	got, _ := db.GetClaudeSessionID("a1")
	if got != "persisted" {
		t.Fatalf("empty set clobbered prior value: %q", got)
	}
}

func TestClaudeSessionID_MissingRowErrors(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "missing.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.SetClaudeSessionID("ghost", "sid"); err == nil {
		t.Fatal("expected error setting on missing logical_agents row")
	}

	_, err = db.GetClaudeSessionID("ghost")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestClaudeSessionID_EmptyAgentIDRejected(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.SetClaudeSessionID("", "sid"); err == nil {
		t.Fatal("expected error for empty logical agent id")
	}
}

func TestLogicalAgentPolicy_DefaultsToManual(t *testing.T) {
	db := openClaudeSessionTestStore(t, "policy-agent")
	got, err := db.GetLogicalAgentPolicy("policy-agent")
	if err != nil {
		t.Fatalf("GetLogicalAgentPolicy: %v", err)
	}
	if got.CheckpointPolicy != agent.CheckpointPolicyManual {
		t.Fatalf("CheckpointPolicy = %q, want %q", got.CheckpointPolicy, agent.CheckpointPolicyManual)
	}
	if got.CheckpointStatus != "" {
		t.Fatalf("CheckpointStatus = %q, want empty", got.CheckpointStatus)
	}
}

func TestLogicalAgentPolicy_UpdateRoundTrip(t *testing.T) {
	db := openClaudeSessionTestStore(t, "policy-agent")
	now := "2026-04-19T10:10:00Z"
	err := db.UpdateLogicalAgentPolicy(agent.LogicalAgentPolicy{
		LogicalAgentID:   "policy-agent",
		CheckpointPolicy: agent.CheckpointPolicyOnStop,
		CheckpointStatus: "auto-stop",
	}, now)
	if err != nil {
		t.Fatalf("UpdateLogicalAgentPolicy: %v", err)
	}
	got, err := db.GetLogicalAgentPolicy("policy-agent")
	if err != nil {
		t.Fatalf("GetLogicalAgentPolicy: %v", err)
	}
	if got.CheckpointPolicy != agent.CheckpointPolicyOnStop {
		t.Fatalf("CheckpointPolicy = %q, want %q", got.CheckpointPolicy, agent.CheckpointPolicyOnStop)
	}
	if got.CheckpointStatus != "auto-stop" {
		t.Fatalf("CheckpointStatus = %q, want auto-stop", got.CheckpointStatus)
	}
	if got.UpdatedAt != now {
		t.Fatalf("UpdatedAt = %q, want %q", got.UpdatedAt, now)
	}
}

func TestLogicalAgentPolicy_UpdateClearsStatusForManual(t *testing.T) {
	db := openClaudeSessionTestStore(t, "policy-agent")
	if err := db.UpdateLogicalAgentPolicy(agent.LogicalAgentPolicy{
		LogicalAgentID:   "policy-agent",
		CheckpointPolicy: agent.CheckpointPolicyOnStop,
		CheckpointStatus: "auto-stop",
	}, "2026-04-19T10:10:00Z"); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	if err := db.UpdateLogicalAgentPolicy(agent.LogicalAgentPolicy{
		LogicalAgentID:   "policy-agent",
		CheckpointPolicy: agent.CheckpointPolicyManual,
		CheckpointStatus: "should-clear",
	}, "2026-04-19T10:11:00Z"); err != nil {
		t.Fatalf("clear policy: %v", err)
	}
	got, err := db.GetLogicalAgentPolicy("policy-agent")
	if err != nil {
		t.Fatalf("GetLogicalAgentPolicy: %v", err)
	}
	if got.CheckpointPolicy != agent.CheckpointPolicyManual {
		t.Fatalf("CheckpointPolicy = %q, want manual", got.CheckpointPolicy)
	}
	if got.CheckpointStatus != "" {
		t.Fatalf("CheckpointStatus = %q, want empty", got.CheckpointStatus)
	}
}
