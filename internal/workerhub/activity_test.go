package workerhub

import (
	"encoding/json"
	"testing"
	"time"

	re "github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestActivityFencesConnectionAndClaims(t *testing.T) {
	h := New(nil)
	c := &connection{}
	h.connections["worker-one"] = c
	now := time.Now()
	b := &workerwire.Activity{ID: "batch", ObservedAt: now, Events: []re.Event{{SchemaVersion: re.SchemaVersion, ID: "event", Kind: re.KindSessionHeartbeat, Time: now, SessionID: "claimed-other-worker", Sequence: 1, Process: re.Process{PID: 42, Provider: "codex", ProviderSessionID: "thread"}, Source: re.Source{Channel: "plugin-hook"}, Actor: &re.Actor{URN: "msg://user/fake", Kind: "user"}, Payload: json.RawMessage(`{"state":"idle"}`)}}}
	if err := h.receiveActivity("worker-one", c, b, now); err != nil {
		t.Fatal(err)
	}
	e := c.activity.Events[0]
	if e.SessionID != "worker-one:codex:thread" || e.Actor != nil || e.ID != "worker-one:event" {
		t.Fatalf("untrusted identity survived: %+v", e)
	}
	b.Events[0].Process.Provider = "bad"
	if err := h.receiveActivity("worker-one", c, b, now); err == nil {
		t.Fatal("accepted invalid batch")
	}
	if c.activity.Events[0].Process.Provider != "codex" {
		t.Fatal("bad batch mutated last successful observation")
	}
	b.Events[0].Process.Provider = "codex"
	h.connections["worker-one"] = &connection{}
	if err := h.receiveActivity("worker-one", c, b, now); err == nil {
		t.Fatal("replaced connection updated activity")
	}
}
