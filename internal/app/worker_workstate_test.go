package app

import (
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type workStateEvent struct {
	Seq      int64
	From, To string
}

func workStateEvents(t *testing.T, s *Service, session string) (out []workStateEvent, kinds []string) {
	t.Helper()
	rows, err := s.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.SessionID != session {
			continue
		}
		switch e.Kind {
		case events.KindSessionWorkStateChanged:
			var p struct{ From, To string }
			if err := json.Unmarshal([]byte(e.PayloadJSON), &p); err != nil {
				t.Fatal(err)
			}
			out = append(out, workStateEvent{Seq: e.Seq, From: p.From, To: p.To})
			kinds = append(kinds, e.Kind)
		case events.KindSessionStateChanged:
			kinds = append(kinds, e.Kind)
		}
	}
	return out, kinds
}

// CW-20261021-0003: a worker's idle frame is recorded as a work-state change
// before it is normalized to the persisted lifecycle state "running", only when
// the value changes, and it survives an in-memory reset.
func TestWorkerStateFramesRecordWorkStateBeforeNormalization(t *testing.T) {
	s := permissionWorkerHarness(t)
	frame := func(state string) {
		t.Helper()
		if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "state", Session: "s1", State: state}); err != nil {
			t.Fatal(err)
		}
	}
	frame("running") // a turn begins
	frame("running") // repeated: no new work-state event
	frame("idle")    // turn complete
	frame("idle")    // repeated
	frame("running")
	got, kinds := workStateEvents(t, s, "s1")
	want := []workStateEvent{{From: "unknown", To: "processing"}, {From: "processing", To: "idle"}, {From: "idle", To: "processing"}}
	if len(got) != len(want) {
		t.Fatalf("work-state events %+v, want %d changes", got, len(want))
	}
	for i := range want {
		if got[i].From != want[i].From || got[i].To != want[i].To {
			t.Fatalf("event %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	// Within a frame the work-state event precedes the lifecycle event.
	if kinds[0] != events.KindSessionWorkStateChanged || kinds[1] != events.KindSessionStateChanged {
		t.Fatalf("the first frame must record work-state before the lifecycle event: %v", kinds)
	}

	// A restart loses the in-memory value. The last recorded event says processing,
	// so an unchanged frame announces nothing and the next change names it as from.
	s.workerIngestMu.Lock()
	s.workStates = nil
	s.workerIngestMu.Unlock()
	frame("running") // unchanged vs the recorded processing: no event
	if after, _ := workStateEvents(t, s, "s1"); len(after) != len(want) {
		t.Fatalf("a restart re-announced an unchanged work-state: %+v", after)
	}
	frame("idle")
	if after, _ := workStateEvents(t, s, "s1"); len(after) != len(want)+1 || after[len(after)-1].From != "processing" || after[len(after)-1].To != "idle" {
		t.Fatalf("after restart the next change must name the recorded value as from: %+v", after)
	}
}

// Exit is a lifecycle fact, not a work-state, and clears the in-memory value.
func TestWorkerExitRecordsNoWorkState(t *testing.T) {
	s := permissionWorkerHarness(t)
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "state", Session: "s1", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "state", Session: "s1", State: "exited"}); err != nil {
		t.Fatal(err)
	}
	got, _ := workStateEvents(t, s, "s1")
	if len(got) != 1 || got[0].To != "processing" {
		t.Fatalf("exit recorded a work-state: %+v", got)
	}
	s.workerIngestMu.Lock()
	_, kept := s.workStates["s1"]
	s.workerIngestMu.Unlock()
	if kept {
		t.Fatal("an ended session kept its in-memory work-state")
	}
}
