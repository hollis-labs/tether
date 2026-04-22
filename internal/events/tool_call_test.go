package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestToolCallEvent_BusRoundTrip publishes a ToolCallEvent as JSON in Event.PayloadJSON
// and asserts the subscriber receives it with all fields intact.
func TestToolCallEvent_BusRoundTrip(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	ctx := context.Background()

	ch, cancel, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	want := ToolCallEvent{
		SessionID:    "sess-abc",
		ToolName:     "hadron_health",
		Server:       "hadron",
		ArgsSchemaFP: "deadbeef",
		DurationMs:   42,
		OK:           true,
		Error:        "",
		Timestamp:    time.Now().UTC().Truncate(time.Second),
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	if err := bus.Publish(ctx, Event{
		Scope:       ScopeSession,
		SessionID:   want.SessionID,
		Kind:        EventTypeToolCallEnd,
		PayloadJSON: string(raw),
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if got.Kind != EventTypeToolCallEnd {
			t.Errorf("Kind = %q, want %q", got.Kind, EventTypeToolCallEnd)
		}
		if got.SessionID != want.SessionID {
			t.Errorf("SessionID = %q, want %q", got.SessionID, want.SessionID)
		}
		// Unmarshal payload and verify all fields.
		var tce ToolCallEvent
		if err := json.Unmarshal([]byte(got.PayloadJSON), &tce); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if tce.ToolName != want.ToolName {
			t.Errorf("ToolName = %q, want %q", tce.ToolName, want.ToolName)
		}
		if tce.Server != want.Server {
			t.Errorf("Server = %q, want %q", tce.Server, want.Server)
		}
		if tce.ArgsSchemaFP != want.ArgsSchemaFP {
			t.Errorf("ArgsSchemaFP = %q, want %q", tce.ArgsSchemaFP, want.ArgsSchemaFP)
		}
		if tce.DurationMs != want.DurationMs {
			t.Errorf("DurationMs = %d, want %d", tce.DurationMs, want.DurationMs)
		}
		if tce.OK != want.OK {
			t.Errorf("OK = %v, want %v", tce.OK, want.OK)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for ToolCallEvent")
	}
}

// TestToolCallEvent_StartEvent verifies tool_call_start events also round-trip.
func TestToolCallEvent_StartEvent(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	ctx := context.Background()

	ch, cancel, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	startEv := ToolCallEvent{
		ToolName:     "vanta_memory_recall",
		Server:       "vanta",
		ArgsSchemaFP: "00000000",
		Timestamp:    time.Now().UTC(),
	}
	raw, _ := json.Marshal(startEv)

	if err := bus.Publish(ctx, Event{
		Scope:       ScopeDaemon,
		Kind:        EventTypeToolCallStart,
		PayloadJSON: string(raw),
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if got.Kind != EventTypeToolCallStart {
			t.Errorf("Kind = %q, want %q", got.Kind, EventTypeToolCallStart)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}
