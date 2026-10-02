package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/telemetry"
)

func TestDurableToolMetricsHistogramsRetainLegacyAndDeniedCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	appendCall := func(kind string, call events.ToolCallEvent) {
		t.Helper()
		raw, _ := json.Marshal(call)
		if _, _, err := db.InsertEvent(events.ScopeDaemon, "", kind, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	appendCall(events.EventTypeToolCallStart, events.ToolCallEvent{ToolName: "read", Server: "alpha"})
	appendCall(events.EventTypeToolCallEnd, events.ToolCallEvent{ToolName: "read", Server: "alpha", OK: true, DurationMs: 25, ToolCallDetails: events.ToolCallDetails{ArgsBytes: 9, ResultBytes: 12, GatewayMs: 1, ForwardMs: 24}})
	appendCall(events.EventTypeToolCallEnd, events.ToolCallEvent{ToolName: "read", Server: "alpha", OK: true, DurationMs: 100, ToolCallDetails: events.ToolCallDetails{ArgsBytes: 5, ResultBytes: 7, GatewayMs: 5, ForwardMs: 95}})
	appendCall(events.EventTypeToolCallEnd, events.ToolCallEvent{ToolName: "read", Server: "alpha", DurationMs: 3, ToolCallDetails: events.ToolCallDetails{ErrorClass: events.ToolErrorDenied, GatewayMs: 3}})
	// A pre-v2 row has duration but lacks size/gateway/forward metadata.
	if _, _, err := db.InsertEvent(events.ScopeDaemon, "", events.EventTypeToolCallEnd, `{"tool_name":"read","server":"legacy","ok":true,"duration_ms":50}`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	out, err := (telemetry.QueryService{Source: db}).ReadMetrics(context.Background(), telemetry.MetricsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Groups) != 3 {
		t.Fatalf("groups %+v", out.Groups)
	}
	for _, group := range out.Groups {
		if group.Upstream == "legacy" {
			if group.Calls != 1 || group.MetadataSamples != 0 || group.Duration.Count != 1 || group.Gateway.Count != 0 || group.Gateway.Buckets[0].Count != 0 {
				t.Fatal(group)
			}
			continue
		}
		if group.Outcome == "ok" {
			if group.Calls != 2 || group.ArgsBytes != 14 || group.ResultBytes != 19 || group.Duration.SumMs != 125 || group.Duration.Buckets[1].Count != 1 || group.Duration.Buckets[2].Count != 2 {
				t.Fatal(group)
			}
		}
		if group.Outcome == "denied" && group.Calls != 1 {
			t.Fatal(group)
		}
	}
	out, err = (telemetry.QueryService{Source: db}).ReadMetrics(context.Background(), telemetry.MetricsRequest{Upstream: "alpha", Tool: "read", Since: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	if err != nil || len(out.Groups) != 0 {
		t.Fatal(out, err)
	}
	if _, err := (telemetry.QueryService{Source: db}).ReadMetrics(context.Background(), telemetry.MetricsRequest{Since: "invalid"}); err == nil {
		t.Fatal("invalid timestamp accepted")
	}
}

func TestToolMetricsFractionalTimeBoundaries(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "time.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stamp := range []string{"2026-10-02T00:00:00Z", "2026-10-02T00:00:00.1Z", "2026-10-02T00:00:00.12Z", "2026-10-02T00:00:00.123Z"} {
		id, _, err := db.InsertEvent(events.ScopeDaemon, "", events.EventTypeToolCallEnd, `{"tool_name":"read","server":"alpha","ok":true,"duration_ms":1}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec("UPDATE events SET at=? WHERE id=?", stamp, id); err != nil {
			t.Fatal(err)
		}
	}
	service := telemetry.QueryService{Source: db}
	out, err := service.ReadMetrics(context.Background(), telemetry.MetricsRequest{Since: "2026-10-02T00:00:00.12Z", Until: "2026-10-02T00:00:01Z"})
	if err != nil || len(out.Groups) != 1 || out.Groups[0].Calls != 2 {
		t.Fatal(out, err)
	}
	out, err = service.ReadMetrics(context.Background(), telemetry.MetricsRequest{Until: "2026-10-02T00:00:00.12Z"})
	if err != nil || len(out.Groups) != 1 || out.Groups[0].Calls != 2 {
		t.Fatal(out, err)
	}
}

func TestToolMetricsPrioritizeFrequentToolsOverJunkGroups(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "volume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i := range 1100 {
		raw, err := json.Marshal(events.ToolCallEvent{ToolName: fmt.Sprintf("a_junk_%d", i), ToolCallDetails: events.ToolCallDetails{ErrorClass: events.ToolErrorDenied}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.InsertEvent(events.ScopeDaemon, "", events.EventTypeToolCallEnd, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, _, err := db.InsertEvent(events.ScopeDaemon, "", events.EventTypeToolCallEnd, `{"tool_name":"tether_docs_list","ok":true}`); err != nil {
			t.Fatal(err)
		}
	}
	out, err := (telemetry.QueryService{Source: db}).ReadMetrics(context.Background(), telemetry.MetricsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || len(out.Groups) == 0 || out.Groups[0].Tool != "tether_docs_list" || out.Groups[0].Calls != 3 {
		t.Fatal("frequent real tool displaced by junk groups or truncation missing")
	}
}
