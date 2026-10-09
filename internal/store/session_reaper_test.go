package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestReaperOrphanPreservesCompletedSession(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(SessionRow{ID: "late", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionState("late", "completed", 0, new(int)); err != nil {
		t.Fatal(err)
	}
	changed, err := db.RecordReaperOrphan(context.Background(), "late", "process_missing")
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	row, _ := db.GetSession("late")
	if row.State != "completed" {
		t.Fatalf("state=%s", row.State)
	}
}

func TestReaperOrphanOutcomeAndActivity(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(SessionRow{ID: "lost", State: "running", PID: sql.NullInt64{Int64: 22, Valid: true}}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	_, at, err := db.InsertEvent(events.ScopeSession, "lost", "session.turn_output", "{}")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = db.InsertEvent(events.ScopeSession, "lost", "session.reaper", "{}")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.ReaperSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].LastActivity.Equal(at) {
		t.Fatalf("activity=%+v want=%s", rows, at)
	}
	changed, err := db.RecordReaperOrphan(context.Background(), "lost", "process_missing")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	changed, err = db.RecordReaperOrphan(context.Background(), "lost", "process_missing")
	if err != nil || changed {
		t.Fatalf("duplicate changed=%v err=%v", changed, err)
	}
	row, _ := db.GetSession("lost")
	if row.State != "orphaned" || row.PID.Valid || row.EndedAt.Valid {
		t.Fatalf("orphan=%+v", row)
	}
	evs, err := db.QueryEvents(EventFilter{SessionID: "lost", Kinds: []string{"session.reaper"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("expected one outcome plus initial probe, got %d", len(evs))
	}
}

func TestReaperLeaseExpiry(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	expired, err := db.SessionLeaseExpired(context.Background(), "one", now)
	if err != nil || expired {
		t.Fatalf("no lease expired=%v err=%v", expired, err)
	}
	// Binding table deliberately has no session foreign key: registry storage
	// and lifecycle store share the daemon DB but keep independent identities.
	for _, b := range []struct {
		id, expiry string
		generation int
	}{
		{"old", now.Add(-time.Hour).Format(time.RFC3339Nano), 1},
		{"new", now.Add(time.Hour).Format(time.RFC3339Nano), 2},
	} {
		_, err = db.DB().Exec(`INSERT INTO runtime_bindings(id,target_urn,session_id,host_id,attempt_id,generation,capabilities_json,visibility,leased_at,lease_expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'private-local',?,?,?,?)`, b.id, "actor", "one", "local", b.id, b.generation, "[]", now.Format(time.RFC3339Nano), b.expiry, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
	}
	expired, err = db.SessionLeaseExpired(context.Background(), "one", now)
	if err != nil || expired {
		t.Fatalf("live replacement expired=%v err=%v", expired, err)
	}
	expired, err = db.SessionLeaseExpired(context.Background(), "one", now.Add(2*time.Hour))
	if err != nil || !expired {
		t.Fatalf("stale latest expired=%v err=%v", expired, err)
	}
}

func TestReaperActivitySnapshotSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(SessionRow{ID: "restart", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Add(-time.Minute)
	if err := db.RecordSessionActivity(context.Background(), "restart", observed); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.ReaperSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].LastActivity.Equal(observed) {
		t.Fatalf("restart activity=%+v want=%s", rows, observed)
	}
}

func TestReaperOrphanDoesNotClobberConcurrentLaunch(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(SessionRow{ID: "launching", State: "launching"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.GetSession("launching")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionState("launching", "running", 42, nil); err != nil {
		t.Fatal(err)
	}
	changed, err := db.RecordReaperOrphan(context.Background(), "launching", "process_missing", *snapshot)
	if err != nil || changed {
		t.Fatalf("clobbered concurrent launch: changed=%v err=%v", changed, err)
	}
	row, err := db.GetSession("launching")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "running" || row.PID.Int64 != 42 {
		t.Fatalf("live launch=%+v", row)
	}
}
