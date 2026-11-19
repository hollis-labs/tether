package app

import (
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// lastPayload is the newest event of kind for session id, decoded.
func lastPayload(t *testing.T, svc *Service, id, kind string) map[string]any {
	t.Helper()
	rows, err := svc.Store.QueryEvents(store.EventFilter{SessionID: id, Kinds: []string{kind}, Limit: 1})
	if err != nil || len(rows) == 0 {
		t.Fatalf("no %s for %s: %v", kind, id, err)
	}
	var p map[string]any
	if err = json.Unmarshal([]byte(rows[0].PayloadJSON), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// CW-20261114-0168: a worker session's end says who or what ended it. The
// incident's 26 kills arrived as bare exit 143s with nothing naming the hub.
func TestWorkerSessionEndRecordsAReason(t *testing.T) {
	svc := stopHarness(t)
	svc.Workers = workerhub.New(svc.Store.DB())
	exit := func(id string, code int) {
		t.Helper()
		if err := svc.ingestWorkerFrame("w1", workerwire.Frame{T: "state", Session: id, State: "exited", ExitCode: &code}); err != nil {
			t.Fatal(err)
		}
	}

	// A kill a welcome ordered: published as session.kill_ordered, then named on the end.
	ghostRow(t, svc, "s-ordered", "w1")
	svc.recordWorkerKillOrder("w1", "s-ordered", "hub_state_killed")
	if p := lastPayload(t, svc, "s-ordered", KindSessionKillOrdered); p["reason"] != "hub_state_killed" || p["worker_id"] != "w1" {
		t.Fatalf("kill_ordered = %v", p)
	}
	exit("s-ordered", 143)
	if p := lastPayload(t, svc, "s-ordered", events.KindSessionStateChanged); p["reason"] != "hub_kill:hub_state_killed" || p["to"] != "failed" {
		t.Fatalf("ordered end = %v", p)
	}

	// A requested stop: its reason, and the session ends killed.
	ghostRow(t, svc, "s-stopped", "w1")
	svc.stops.markWithReason("s-stopped", "operator stop")
	exit("s-stopped", 143)
	if p := lastPayload(t, svc, "s-stopped", events.KindSessionStateChanged); p["reason"] != "operator stop" || p["to"] != "killed" {
		t.Fatalf("stopped end = %v", p)
	}

	// The agent exiting on its own.
	ghostRow(t, svc, "s-exit", "w1")
	exit("s-exit", 0)
	if p := lastPayload(t, svc, "s-exit", events.KindSessionStateChanged); p["reason"] != EndReasonWorkerExit || p["to"] != "completed" {
		t.Fatalf("plain end = %v", p)
	}
}

// A session closed before it started publishes its end too.
func TestClosedCreatedSessionRecordsAReason(t *testing.T) {
	svc := stopHarness(t)
	ghostRow(t, svc, "s-created", "")
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET state='created' WHERE id='s-created'`); err != nil {
		t.Fatal(err)
	}
	if closed, err := svc.closeCreatedSession("s-created"); err != nil || !closed {
		t.Fatalf("close: %v %v", closed, err)
	}
	if p := lastPayload(t, svc, "s-created", events.KindSessionStateChanged); p["reason"] != EndReasonNeverStarted || p["to"] != "killed" {
		t.Fatalf("created end = %v", p)
	}
}
