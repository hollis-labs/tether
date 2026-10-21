package environmentstream

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func workEvent(session, kind string, at time.Time, payload string) Event {
	return Event{Event: mesh.Event{SessionID: session, Kind: kind, Time: at, Payload: json.RawMessage(payload)}}
}

func workStateChange(session, to string, at time.Time) Event {
	raw, _ := json.Marshal(map[string]string{"from": "x", "to": to, "at": at.Format(time.RFC3339Nano)})
	return workEvent(session, events.KindSessionWorkStateChanged, at, string(raw))
}

var t0 = time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)

// CW-20261021-0003: the snapshot carries the recorded work-state; a session that
// never had one is unknown (known=false), never idle; an ended session has none.
func TestSnapshotWorkState(t *testing.T) {
	base := store.EnvironmentSnapshot{
		HighWater: 10, EarliestAvailable: 1,
		Sessions: []store.EnvironmentSessionRow{
			{ID: "idle", State: "running"}, {ID: "never", State: "running"}, {ID: "ended", State: "completed"}, {ID: "starting", State: "created"},
		},
		WorkStates: map[string]store.EnvironmentWorkState{
			"idle": {Value: "idle", At: t0}, "ended": {Value: "processing", At: t0},
		},
	}
	got := map[string]*WorkState{}
	for _, s := range snapshot("env", base).Sessions {
		got[s.ID] = s.WorkState
	}
	if w := got["idle"]; w == nil || w.Value != WorkIdle || !w.Known || w.Since == nil || !w.Since.Equal(t0) {
		t.Fatalf("idle: %+v", w)
	}
	if w := got["never"]; w == nil || w.Value != WorkUnknown || w.Known || w.Since != nil {
		t.Fatalf("never observed must be unknown, not idle: %+v", w)
	}
	if got["ended"] != nil {
		t.Fatalf("an ended session has no work-state: %+v", got["ended"])
	}
	if w := got["starting"]; w == nil || w.Known {
		t.Fatalf("starting: %+v", w)
	}
}

func TestReduceWorkStateIsIdempotentAndOrdered(t *testing.T) {
	s := Session{ID: "a"}
	setState(&s, "running")
	refreshWork(&s)
	if s.WorkState == nil || s.WorkState.Known {
		t.Fatalf("start: %+v", s.WorkState)
	}
	Reduce(&s, workStateChange("a", "processing", t0.Add(time.Second)))
	Reduce(&s, workStateChange("a", "processing", t0.Add(time.Second))) // replay
	if s.WorkState.Value != WorkProcessing || !s.WorkState.Known {
		t.Fatalf("processing: %+v", s.WorkState)
	}
	Reduce(&s, workStateChange("a", "idle", t0.Add(3*time.Second)))
	Reduce(&s, workStateChange("a", "processing", t0.Add(2*time.Second))) // older: ignored
	if s.WorkState.Value != WorkIdle || !s.WorkState.Since.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("an older event overrode a newer one: %+v", s.WorkState)
	}
	Reduce(&s, workEvent("a", events.KindSessionWorkStateChanged, t0.Add(9*time.Second), `{"to":"garbage","at":"2026-10-21T00:00:09Z"}`))
	Reduce(&s, workEvent("a", events.KindSessionWorkStateChanged, t0.Add(9*time.Second), `not json`))
	if s.WorkState.Value != WorkIdle {
		t.Fatalf("an unreadable event changed the work-state: %+v", s.WorkState)
	}
	Reduce(&s, workStateChange("other", "processing", t0.Add(20*time.Second))) // another session
	if s.WorkState.Value != WorkIdle {
		t.Fatalf("another session's event applied: %+v", s.WorkState)
	}
}

// awaiting_* is derived from the open requests, so it cannot disagree with the
// pending flags, and the recorded value returns when they close.
func TestWorkStateFollowsPendingRequests(t *testing.T) {
	s := Session{ID: "a"}
	setState(&s, "running")
	Reduce(&s, workStateChange("a", "processing", t0))
	request := func(id, kind string, open bool, seq uint64) Event {
		raw, _ := json.Marshal(store.EnvironmentRequest{TurnID: "turn", RequestID: id, Kind: kind, Open: open, SourceSequence: seq})
		return workEvent("a", store.EnvironmentRequestKind, t0.Add(time.Duration(seq)*time.Second), string(raw))
	}
	Reduce(&s, request("r1", "approval", true, 1))
	if s.WorkState.Value != WorkAwaitingApproval || !s.PendingApproval {
		t.Fatalf("approval: %+v pending=%v", s.WorkState, s.PendingApproval)
	}
	Reduce(&s, request("r2", "question", true, 2))
	if s.WorkState.Value != WorkAwaitingApproval { // approval wins while both are open
		t.Fatalf("both open: %+v", s.WorkState)
	}
	Reduce(&s, request("r1", "approval", false, 3))
	if s.WorkState.Value != WorkAwaitingInput || !s.PendingQuestion {
		t.Fatalf("question only: %+v", s.WorkState)
	}
	Reduce(&s, request("r2", "question", false, 4))
	if s.WorkState.Value != WorkProcessing || s.PendingApproval || s.PendingQuestion {
		t.Fatalf("closed: %+v", s.WorkState)
	}
	// Lifecycle end removes the work-state.
	Reduce(&s, workEvent("a", events.KindSessionStateChanged, t0.Add(10*time.Second), `{"from":"running","to":"completed"}`))
	if s.WorkState != nil {
		t.Fatalf("ended session kept a work-state: %+v", s.WorkState)
	}
}
