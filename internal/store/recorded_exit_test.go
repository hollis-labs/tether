package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestRecordedExitCompareAndSwap(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(SessionRow{ID: "s", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE sessions SET pid=0,exit_code=1,ended_at='observed' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	// A launch or authoritative state report arriving after the scan must win.
	if err := db.UpdateSessionState("s", "running", 0, nil); err != nil {
		t.Fatal(err)
	}
	changed, err := db.ReconcileRecordedExit(context.Background(), *snapshot)
	if err != nil || changed {
		t.Fatalf("overwrote concurrent report: changed=%t err=%v", changed, err)
	}
	row, err := db.GetSession("s")
	if err != nil || row.State != "running" || row.ExitCode.Valid {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}
