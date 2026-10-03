package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func shimTestRow() SessionShimRow {
	return SessionShimRow{SessionID: "recovery", ShimKey: "key", HostBackend: "detached", SocketPath: "/private/control.sock", DescriptorPath: "/private/launch.json", Runtime: "claude-stream", RuntimeGeneration: 1, BootGeneration: "boot", JournalID: "journal", ControllerEpoch: 20, InjectCounter: 30, LastCommittedCursor: "journal:100"}
}
func TestSessionShimCascadeWithForeignKeysEnabled(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "cascade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = db.CreateSession(SessionRow{ID: "recovery", State: "created"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err = db.UpsertSessionShim(context.Background(), shimTestRow()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.db.Exec(`DELETE FROM sessions WHERE id='recovery'`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SessionShim(context.Background(), "recovery"); !errors.Is(err, ErrSessionShimNotFound) {
		t.Fatalf("cascade: %v", err)
	}
}
func TestSessionShimMigrationPreservesExistingPrincipalPolicy(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = db.CreateSession(SessionRow{ID: "recovery", State: "orphaned"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	_, err = db.db.Exec(`INSERT INTO principals(principal_id,kind,display,token_hash,scopes_json,session_id,addresses_json,created_at) VALUES ('p','session','test',?,'[]','recovery','[]','2026-10-03T00:00:00Z')`, strings.Repeat("0", 64))
	if err != nil {
		t.Fatalf("additive migration changed principal policy: %v", err)
	}
}
func TestSessionShimCountersNeverRegress(t *testing.T) {
	for _, field := range []string{"counter", "epoch", "cursor"} {
		t.Run(field, func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "monotonic.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if err = db.CreateSession(SessionRow{ID: "recovery", State: "created"}, &launch.Plan{}); err != nil {
				t.Fatal(err)
			}
			row := shimTestRow()
			if err = db.UpsertSessionShim(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "counter":
				row.InjectCounter--
			case "epoch":
				row.ControllerEpoch--
			case "cursor":
				row.LastCommittedCursor = "journal:99"
			}
			if err = db.UpsertSessionShim(context.Background(), row); !errors.Is(err, ErrSessionShimConflict) {
				t.Fatalf("backwards update: %v", err)
			}
			got, err := db.SessionShim(context.Background(), row.SessionID)
			if err != nil || got.InjectCounter != 30 || got.ControllerEpoch != 20 || got.LastCommittedCursor != "journal:100" {
				t.Fatal("failed update changed counters")
			}
		})
	}
}
