package app

import (
	"path/filepath"
	"testing"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/store"
)

// TestSeedLogicalAgents covers the seed helper directly (no catalog
// fixture required) — Service.New's integration path is exercised by
// the end-to-end smoke. This test pins the upsert semantics from
// ADR 0003: idempotent, preserves created_at, refreshes name/role.
func TestSeedLogicalAgents(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	agents := map[string]config.Agent{
		"claude-code": {ID: "claude-code", Name: "Claude Code", Roles: []string{"backend", "storage"}},
		"codex-cli":   {ID: "codex-cli", Name: "Codex", Roles: []string{"frontend"}},
	}
	n, err := seedLogicalAgents(db, agents)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}

	got, err := db.GetLogicalAgent("claude-code")
	if err != nil {
		t.Fatalf("get claude-code: %v", err)
	}
	if got.Name != "Claude Code" {
		t.Errorf("Name = %q, want 'Claude Code'", got.Name)
	}
	if got.Role != "backend, storage" {
		t.Errorf("Role = %q, want 'backend, storage'", got.Role)
	}
	firstCreated := got.CreatedAt

	// Re-seed with an updated name and a shorter roles list — upsert must
	// overwrite name/role but preserve created_at.
	agents["claude-code"] = config.Agent{
		ID:    "claude-code",
		Name:  "Claude Code CLI",
		Roles: []string{"backend"},
	}
	if _, err := seedLogicalAgents(db, agents); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	got, err = db.GetLogicalAgent("claude-code")
	if err != nil {
		t.Fatalf("get after reseed: %v", err)
	}
	if got.Name != "Claude Code CLI" {
		t.Errorf("Name after reseed = %q", got.Name)
	}
	if got.Role != "backend" {
		t.Errorf("Role after reseed = %q", got.Role)
	}
	if got.CreatedAt != firstCreated {
		t.Errorf("CreatedAt changed: %q → %q", firstCreated, got.CreatedAt)
	}

	rows, err := db.ListLogicalAgents()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2", len(rows))
	}
}
