package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/events"
)

func makeEvent(tool, server, session string, ok bool) events.ToolCallEvent {
	return events.ToolCallEvent{
		SessionID:    session,
		ToolName:     tool,
		Server:       server,
		ArgsSchemaFP: "deadbeef",
		DurationMs:   10,
		OK:           ok,
		Timestamp:    time.Now(),
	}
}

// TestToolCallEventStore_AddQuery verifies basic add and query semantics.
func TestToolCallEventStore_AddQuery(t *testing.T) {
	s := NewToolCallEventStore(100)

	s.Add(makeEvent("hadron_health", "hadron", "s1", true))
	s.Add(makeEvent("vanta_recall", "vanta", "s2", false))
	s.Add(makeEvent("hadron_runs_list", "hadron", "s1", true))

	all := s.Query(ToolCallEventFilter{})
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}
}

// TestToolCallEventStore_FilterByServer verifies server ID filtering.
func TestToolCallEventStore_FilterByServer(t *testing.T) {
	s := NewToolCallEventStore(100)
	s.Add(makeEvent("hadron_health", "hadron", "s1", true))
	s.Add(makeEvent("vanta_recall", "vanta", "s2", true))
	s.Add(makeEvent("hadron_runs", "hadron", "s3", true))

	got := s.Query(ToolCallEventFilter{ServerID: "hadron"})
	if len(got) != 2 {
		t.Errorf("expected 2 hadron events, got %d", len(got))
	}
	for _, e := range got {
		if e.Server != "hadron" {
			t.Errorf("unexpected server %q in filtered results", e.Server)
		}
	}
}

// TestToolCallEventStore_FilterByToolNamePrefix verifies prefix matching.
func TestToolCallEventStore_FilterByToolNamePrefix(t *testing.T) {
	s := NewToolCallEventStore(100)
	s.Add(makeEvent("hadron_health", "hadron", "s1", true))
	s.Add(makeEvent("hadron_runs_list", "hadron", "s1", true))
	s.Add(makeEvent("vanta_memory_recall", "vanta", "s1", true))

	got := s.Query(ToolCallEventFilter{ToolName: "hadron_"})
	if len(got) != 2 {
		t.Errorf("expected 2 hadron_ events, got %d", len(got))
	}
}

// TestToolCallEventStore_FilterErrorsOnly verifies errors_only filtering.
func TestToolCallEventStore_FilterErrorsOnly(t *testing.T) {
	s := NewToolCallEventStore(100)
	s.Add(makeEvent("tool_ok", "srv", "s1", true))
	s.Add(makeEvent("tool_err", "srv", "s1", false))

	got := s.Query(ToolCallEventFilter{ErrorsOnly: true})
	if len(got) != 1 {
		t.Fatalf("expected 1 error event, got %d", len(got))
	}
	if got[0].OK {
		t.Error("filtered result should have OK=false")
	}
}

// TestToolCallEventStore_FilterBySession verifies session ID filtering.
func TestToolCallEventStore_FilterBySession(t *testing.T) {
	s := NewToolCallEventStore(100)
	s.Add(makeEvent("tool", "srv", "session-A", true))
	s.Add(makeEvent("tool", "srv", "session-B", true))
	s.Add(makeEvent("tool", "srv", "session-A", true))

	got := s.Query(ToolCallEventFilter{SessionID: "session-A"})
	if len(got) != 2 {
		t.Errorf("expected 2 session-A events, got %d", len(got))
	}
}

// TestToolCallEventStore_Limit verifies limit is respected.
func TestToolCallEventStore_Limit(t *testing.T) {
	s := NewToolCallEventStore(100)
	for i := 0; i < 20; i++ {
		s.Add(makeEvent("tool", "srv", "s1", true))
	}
	got := s.Query(ToolCallEventFilter{Limit: 5})
	if len(got) != 5 {
		t.Errorf("expected 5 events with limit=5, got %d", len(got))
	}
}

// TestToolCallEventStore_RingBuffer verifies oldest events are evicted when full.
func TestToolCallEventStore_RingBuffer(t *testing.T) {
	const cap = 5
	s := NewToolCallEventStore(cap)

	// Add 10 events (double capacity).
	for i := 0; i < 10; i++ {
		s.Add(makeEvent("tool", "srv", "s1", i%2 == 0))
	}

	all := s.Query(ToolCallEventFilter{})
	if len(all) != cap {
		t.Errorf("expected %d events (ring cap), got %d", cap, len(all))
	}
}

// TestToolCallEventStore_SinceTimestamp verifies time-based filtering.
func TestToolCallEventStore_SinceTimestamp(t *testing.T) {
	s := NewToolCallEventStore(100)

	old := makeEvent("old_tool", "srv", "s1", true)
	old.Timestamp = time.Now().Add(-2 * time.Hour)
	s.Add(old)

	fresh := makeEvent("new_tool", "srv", "s1", true)
	fresh.Timestamp = time.Now()
	s.Add(fresh)

	cutoff := time.Now().Add(-time.Hour)
	got := s.Query(ToolCallEventFilter{SinceTimestamp: cutoff})
	if len(got) != 1 {
		t.Fatalf("expected 1 recent event, got %d", len(got))
	}
	if got[0].ToolName != "new_tool" {
		t.Errorf("unexpected tool in results: %q", got[0].ToolName)
	}
}

// TestToolCallEventStore_Subscribe verifies the bus subscriber appends events.
func TestToolCallEventStore_Subscribe(t *testing.T) {
	store := NewToolCallEventStore(100)

	// Use a fake persister to avoid SQLite dependency.
	bus := events.NewBus(events.BusOptions{Persister: &fakeEventPersister{}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store.Subscribe(ctx, bus)

	// Publish a tool_call_end event.
	ev := events.ToolCallEvent{
		ToolName:   "hadron_health",
		Server:     "hadron",
		DurationMs: 5,
		OK:         true,
		Timestamp:  time.Now(),
	}
	raw, _ := json.Marshal(ev)
	if err := bus.Publish(ctx, events.Event{
		Scope:       events.ScopeDaemon,
		Kind:        events.EventTypeToolCallEnd,
		PayloadJSON: string(raw),
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Give the goroutine time to process.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		all := store.Query(ToolCallEventFilter{})
		if len(all) == 1 {
			if all[0].ToolName != "hadron_health" {
				t.Errorf("ToolName = %q, want hadron_health", all[0].ToolName)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout: expected 1 event in store after bus subscription")
}

// ─── fake persister for store tests ──────────────────────────────────────────

type fakeEventPersister struct {
	events.Persister // embed nil interface — only InsertEvent and EventsSince are called
	seq              int64
}

func (f *fakeEventPersister) InsertEvent(scope events.Scope, sessionID, kind, payloadJSON string) (int64, time.Time, error) {
	f.seq++
	return f.seq, time.Now(), nil
}

func (f *fakeEventPersister) EventsSince(sinceSeq int64) ([]events.Event, error) {
	return nil, nil
}
