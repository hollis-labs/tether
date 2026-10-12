package app

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// An exit after an operator stop request ends as killed (the local stop
// vocabulary), not failed; an unrequested nonzero exit stays failed.
func TestWorkerExitAfterStopRequestIsKilled(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.DB().Exec(`INSERT INTO workers(id,label,device_id,platform,caps_json,last_seen,created_at) VALUES('w1','agent-os','d1','linux','{}','','')`); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db, Workers: workerhub.New(db.DB()), stops: &stopRequests{}}
	for id, tc := range map[string]struct {
		stop bool
		code int
		want string
	}{
		"stopped-143": {true, 143, "killed"},
		"stopped-0":   {true, 0, "killed"},
		"crashed-1":   {false, 1, "failed"},
		"finished-0":  {false, 0, "completed"},
	} {
		plan := &launch.Plan{ProviderID: "p", LogicalAgentID: "a"}
		if err := db.CreateSession(store.SessionRow{ID: id, WorkerID: "w1", LogicalAgentID: "a", ProviderID: "p", ProviderKind: "cli", Workspace: t.TempDir(), State: "running"}, plan); err != nil {
			t.Fatal(err)
		}
		if tc.stop {
			svc.stops.mark(id)
		}
		code := tc.code
		if err := svc.ingestWorkerFrame("w1", workerwire.Frame{T: "state", Session: id, State: "exited", ExitCode: &code}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		got, err := db.GetSession(id)
		if err != nil || got.State != tc.want {
			t.Fatalf("%s: state %q err %v, want %q", id, got.State, err, tc.want)
		}
		if svc.stops.requested(id) {
			t.Fatalf("%s: stop mark not cleared", id)
		}
	}
}
