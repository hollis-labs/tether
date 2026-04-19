package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestClientAttachments_CreateAndList(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ca.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Session must exist because of the FK.
	if err := db.CreateSession(SessionRow{
		ID: "s1", LaunchID: "l", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv",
		Workspace: "/tmp/ws", State: "running",
	}, nil); err != nil {
		// CreateSession also tries to insert launch_plans; pass a nil plan.
	}
	// Re-insert via direct sql because CreateSession needs a plan; keep the
	// test focused on the attachments table.
	if _, err := db.db.Exec(`DELETE FROM sessions WHERE id=?`, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO sessions
        (id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"s1", "l", "p", "a", "pv", "/tmp/ws", "running",
		time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}

	if err := db.CreateClientAttachment("att-1", "s1", "cli", "2026-04-18T10:00:00Z"); err != nil {
		t.Fatalf("CreateClientAttachment: %v", err)
	}
	if err := db.CreateClientAttachment("att-2", "s1", "api", "2026-04-18T10:01:00Z"); err != nil {
		t.Fatalf("CreateClientAttachment #2: %v", err)
	}

	rows, err := db.ListClientAttachments("s1")
	if err != nil {
		t.Fatalf("ListClientAttachments: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].ID != "att-1" || rows[1].ID != "att-2" {
		t.Errorf("rows order = %v %v", rows[0].ID, rows[1].ID)
	}
	if rows[0].DetachedAt.Valid {
		t.Errorf("att-1 should still be attached")
	}

	if err := db.DetachClientAttachment("att-1", "2026-04-18T10:05:00Z"); err != nil {
		t.Fatalf("DetachClientAttachment: %v", err)
	}
	rows, _ = db.ListClientAttachments("s1")
	if !rows[0].DetachedAt.Valid || rows[0].DetachedAt.String != "2026-04-18T10:05:00Z" {
		t.Errorf("att-1 detached_at not set: %+v", rows[0])
	}
}

func TestClientAttachments_SweepStale(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sweep.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Seed a session row manually (same reason as above).
	if _, err := db.db.Exec(`INSERT INTO sessions
        (id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"s1", "l", "p", "a", "pv", "/tmp/ws", "completed",
		"2026-04-18T09:00:00Z", "2026-04-18T09:00:00Z",
	); err != nil {
		t.Fatal(err)
	}

	// Two orphaned attachments + one already-detached.
	if err := db.CreateClientAttachment("live-1", "s1", "cli", "2026-04-18T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateClientAttachment("live-2", "s1", "cli", "2026-04-18T10:00:30Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateClientAttachment("already-closed", "s1", "cli", "2026-04-18T09:30:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.DetachClientAttachment("already-closed", "2026-04-18T09:45:00Z"); err != nil {
		t.Fatal(err)
	}

	n, err := db.SweepStaleAttachments("2026-04-18T11:00:00Z")
	if err != nil {
		t.Fatalf("SweepStaleAttachments: %v", err)
	}
	if n != 2 {
		t.Errorf("swept = %d, want 2", n)
	}

	rows, _ := db.ListClientAttachments("s1")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if !r.DetachedAt.Valid {
			t.Errorf("row %s still not detached after sweep", r.ID)
		}
	}

	// Second sweep is a no-op.
	n, err = db.SweepStaleAttachments("2026-04-18T11:05:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second sweep touched %d rows; want 0", n)
	}
}
