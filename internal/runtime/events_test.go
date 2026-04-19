package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/events"
)

type fakeEventPublisher struct {
	mu       sync.Mutex
	received []events.Event
}

func (f *fakeEventPublisher) Publish(_ context.Context, e events.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, e)
	return nil
}

func (f *fakeEventPublisher) snapshot() []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]events.Event, len(f.received))
	copy(out, f.received)
	return out
}

func TestManager_Start_EmitsLaunchingThenRunning(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	pub := &fakeEventPublisher{}
	m := NewManager(sink).WithEventPublisher(pub)

	if err := m.Start(context.Background(), newRequestFor("s1", rt)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	evs := pub.snapshot()
	if len(evs) != 2 {
		t.Fatalf("events len = %d, want 2 (launching, running)", len(evs))
	}

	for i, want := range []string{"launching", "running"} {
		if evs[i].Scope != events.ScopeSession {
			t.Errorf("ev[%d].Scope = %q, want session", i, evs[i].Scope)
		}
		if evs[i].SessionID != "s1" {
			t.Errorf("ev[%d].SessionID = %q, want s1", i, evs[i].SessionID)
		}
		if evs[i].LogicalAgentID != "a1" {
			t.Errorf("ev[%d].LogicalAgentID = %q, want a1", i, evs[i].LogicalAgentID)
		}
		if evs[i].Kind != events.KindSessionStateChanged {
			t.Errorf("ev[%d].Kind = %q, want %q", i, evs[i].Kind, events.KindSessionStateChanged)
		}
		var payload struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := json.Unmarshal([]byte(evs[i].PayloadJSON), &payload); err != nil {
			t.Fatalf("ev[%d].PayloadJSON (%q) not valid JSON: %v", i, evs[i].PayloadJSON, err)
		}
		if payload.To != want {
			t.Errorf("ev[%d].payload.to = %q, want %q", i, payload.To, want)
		}
	}
	// Specific from/to pairs:
	// launching comes from created; running comes from launching.
	gotFrom := []string{}
	for _, e := range evs {
		var p struct {
			From string `json:"from"`
		}
		_ = json.Unmarshal([]byte(e.PayloadJSON), &p)
		gotFrom = append(gotFrom, p.From)
	}
	if strings.Join(gotFrom, ",") != "created,launching" {
		t.Errorf("from sequence = %v, want [created launching]", gotFrom)
	}
}

func TestManager_Terminate_EmitsCompletedEvent(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	pub := &fakeEventPublisher{}
	m := NewManager(sink).WithEventPublisher(pub)

	if err := m.Start(context.Background(), newRequestFor("s1", rt)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rt.get("/tmp/ws/s1/session.log").complete(0)

	// Wait for terminal event to arrive.
	deadline := time.Now().Add(2 * time.Second)
	var terminal *events.Event
	for time.Now().Before(deadline) {
		evs := pub.snapshot()
		for i := range evs {
			var p struct {
				To string `json:"to"`
			}
			_ = json.Unmarshal([]byte(evs[i].PayloadJSON), &p)
			if p.To == "completed" {
				terminal = &evs[i]
				break
			}
		}
		if terminal != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if terminal == nil {
		t.Fatalf("no 'completed' event published; got: %v", pub.snapshot())
	}

	if terminal.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", terminal.SessionID)
	}
	if terminal.LogicalAgentID != "a1" {
		t.Errorf("LogicalAgentID = %q, want a1", terminal.LogicalAgentID)
	}
	var p struct {
		From     string `json:"from"`
		To       string `json:"to"`
		ExitCode *int   `json:"exit_code,omitempty"`
	}
	if err := json.Unmarshal([]byte(terminal.PayloadJSON), &p); err != nil {
		t.Fatalf("PayloadJSON: %v", err)
	}
	if p.From != "running" {
		t.Errorf("from = %q, want running", p.From)
	}
	if p.ExitCode == nil || *p.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", p.ExitCode)
	}
}

func TestManager_Stop_EmitsKilledEvent(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	pub := &fakeEventPublisher{}
	m := NewManager(sink).WithEventPublisher(pub)

	if err := m.Start(context.Background(), newRequestFor("s1", rt)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Stop(context.Background(), "s1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var terminal *events.Event
	for time.Now().Before(deadline) {
		for _, e := range pub.snapshot() {
			var p struct {
				To string `json:"to"`
			}
			_ = json.Unmarshal([]byte(e.PayloadJSON), &p)
			if p.To == "killed" {
				ee := e
				terminal = &ee
				break
			}
		}
		if terminal != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if terminal == nil {
		t.Fatalf("no 'killed' event; got: %v", pub.snapshot())
	}
}

func TestManager_NilPublisher_NoCrash(t *testing.T) {
	sink := &fakeSink{}
	rt := &fakeRuntime{}
	m := NewManager(sink) // no publisher

	if err := m.Start(context.Background(), newRequestFor("s1", rt)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rt.get("/tmp/ws/s1/session.log").complete(0)
	// Wait briefly to let watch goroutine run; no assertion on events.
	time.Sleep(50 * time.Millisecond)
}
