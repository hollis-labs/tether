package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func usageFixture(t *testing.T) (*store.Store, events.Bus) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.CreateSession(store.SessionRow{ID: "s1", LaunchID: "l", ProjectID: "p", LogicalAgentID: "la", ProviderID: "claude", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	return db, events.NewBus(events.BusOptions{Persister: db})
}

func usageEvents(t *testing.T, db *store.Store) []sessionUsagePayload {
	t.Helper()
	rows, err := db.DB().Query(`SELECT payload_json FROM events WHERE kind=? ORDER BY id`, events.KindSessionUsage)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []sessionUsagePayload
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var p sessionUsagePayload
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestUsageIsReportedPerTurnWithCumulativeTotalsAndOncePerSessionEnd(t *testing.T) {
	db, bus := usageFixture(t)
	turn := func(id string, in, out int64, reported bool) {
		t.Helper()
		if err := db.AddSessionTurnUsage("s1", store.TurnUsage{Reported: reported, InputTokens: in, OutputTokens: out}); err != nil {
			t.Fatal(err)
		}
		publishSessionUsage(bus, db, "s1", "la", id, false)
	}
	turn("t1", 100, 20, true)
	turn("t2", 50, 10, true)
	turn("t3", 0, 0, false) // a turn that reported nothing: totals unchanged, turns still count
	got := usageEvents(t, db)
	want := []sessionUsagePayload{
		{InputTokens: 100, OutputTokens: 20, Turns: 1, Source: "provider", TurnID: "t1"},
		{InputTokens: 150, OutputTokens: 30, Turns: 2, Source: "provider", TurnID: "t2"},
		{InputTokens: 150, OutputTokens: 30, Turns: 3, Source: "provider", TurnID: "t3"},
	}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("usage events %+v, want %+v", got, want)
	}
	// Only a terminal transition closes the stream, once, with final=true.
	publishFinalUsageIfTerminal(bus, db, "s1", "la", "running")
	if n := len(usageEvents(t, db)); n != 3 {
		t.Fatalf("a non-terminal state produced a usage report (%d events)", n)
	}
	publishFinalUsageIfTerminal(bus, db, "s1", "la", "completed")
	got = usageEvents(t, db)
	if len(got) != 4 || !got[3].Final || got[3].InputTokens != 150 || got[3].TurnID != "" {
		t.Fatalf("final report: %+v", got)
	}
	var schema, category sql.NullString
	if err := db.DB().QueryRow(`SELECT payload_schema, category FROM events WHERE kind=? LIMIT 1`, events.KindSessionUsage).Scan(&schema, &category); err != nil {
		t.Fatal(err)
	}
	if schema.String != "session_usage@1" || category.String != "activity" {
		t.Fatalf("class block: schema=%q category=%q", schema.String, category.String)
	}
}

func TestAnUnmeteredSessionReportsNoUsageAtAll(t *testing.T) {
	db, bus := usageFixture(t)
	// A runtime that never reports tokens: turns are counted, tokens are not.
	if err := db.AddSessionTurnUsage("s1", store.TurnUsage{Reported: false}); err != nil {
		t.Fatal(err)
	}
	publishSessionUsage(bus, db, "s1", "la", "t1", false)
	publishFinalUsageIfTerminal(bus, db, "s1", "la", "completed")
	publishSessionUsage(bus, db, "ghost", "", "t1", false) // no meter row at all
	if got := usageEvents(t, db); len(got) != 0 {
		t.Fatalf("zero totals were claimed as a measurement: %+v", got)
	}
}

func TestTerminalLifecycleEventClosesTheUsageStream(t *testing.T) {
	db, bus := usageFixture(t)
	if err := db.AddSessionTurnUsage("s1", store.TurnUsage{Reported: true, InputTokens: 7, OutputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	sink := &eventSinkAdapter{db: db, bus: bus}
	sink.Emit(context.Background(), agentsessions.LifecycleEvent{SessionID: "s1", From: agentsessions.StateRunning, To: agentsessions.StateDone})
	got := usageEvents(t, db)
	if len(got) != 1 || !got[0].Final || got[0].InputTokens != 7 || got[0].Turns != 1 {
		t.Fatalf("final usage after the terminal state change: %+v", got)
	}
	// The state change is published first, so a consumer sees the end before its totals.
	var first string
	if err := db.DB().QueryRow(`SELECT kind FROM events ORDER BY id LIMIT 1`).Scan(&first); err != nil || first != events.KindSessionStateChanged {
		t.Fatalf("first event %q %v", first, err)
	}
}

// Two terminal changes for one session (a stop, then the provider's exit) close
// the stream once.
func TestFinalUsageIsReportedAtMostOncePerSession(t *testing.T) {
	db, bus := usageFixture(t)
	if err := db.AddSessionTurnUsage("s1", store.TurnUsage{Reported: true, InputTokens: 5, OutputTokens: 2}); err != nil {
		t.Fatal(err)
	}
	publishFinalUsageIfTerminal(bus, db, "s1", "la", "killed")
	publishFinalUsageIfTerminal(bus, db, "s1", "la", "failed")
	publishFinalUsageIfTerminal(bus, db, "s1", "la", "orphaned")
	got := usageEvents(t, db)
	if len(got) != 1 || !got[0].Final {
		t.Fatalf("final reports: %+v", got)
	}
}
