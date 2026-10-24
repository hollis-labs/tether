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

func TestDurableToolMetricsDiscoveryModeDimensionFilterAndCompact(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	add := func(tool, mode string, ok bool, ms, result int64) {
		t.Helper()
		call := events.ToolCallEvent{ToolName: tool, Server: "alpha", OK: ok, DurationMs: ms,
			ToolCallDetails: events.ToolCallDetails{DiscoveryMode: mode, ResultBytes: result, ArgsBytes: 10, GatewayMs: 1, ForwardMs: ms}}
		raw, _ := json.Marshal(call)
		if _, _, err := db.InsertEvent(events.ScopeDaemon, "", events.EventTypeToolCallEnd, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		add("read", "search", true, 4, 1000)
	}
	add("read", "search", true, 400, 3000) // slow outlier: 9 search calls
	add("read", "flat", true, 4, 500)
	add("read", "flat", true, 4, 500)
	// A row that predates the discovery_mode field.
	if _, _, err := db.InsertEvent(events.ScopeDaemon, "", events.EventTypeToolCallEnd, `{"tool_name":"read","server":"alpha","ok":true,"duration_ms":4}`); err != nil {
		t.Fatal(err)
	}
	svc := telemetry.QueryService{Source: db}
	ctx := context.Background()

	// Default behavior is unchanged: modes are not a dimension.
	out, err := svc.ReadMetrics(ctx, telemetry.MetricsRequest{})
	if err != nil || len(out.Groups) != 1 || out.Groups[0].Calls != 12 || out.Groups[0].Mode != "" {
		t.Fatalf("default grouping changed: %+v %v", out.Groups, err)
	}

	// group_by=mode splits them, ordered by call count; legacy rows keep mode "".
	out, err = svc.ReadMetrics(ctx, telemetry.MetricsRequest{GroupBy: "mode"})
	if err != nil || len(out.Groups) != 3 {
		t.Fatalf("by mode: %+v %v", out.Groups, err)
	}
	got := map[string]int64{}
	for _, g := range out.Groups {
		got[g.Mode] = g.Calls
	}
	if got["search"] != 9 || got["flat"] != 2 || got[""] != 1 {
		t.Fatalf("mode counts %v", got)
	}

	// mode filter returns only that mode.
	out, err = svc.ReadMetrics(ctx, telemetry.MetricsRequest{Mode: "search"})
	if err != nil || len(out.Groups) != 1 || out.Groups[0].Calls != 9 {
		t.Fatalf("filter: %+v %v", out.Groups, err)
	}

	// compact: no histogram arrays, averages and percentile bounds instead.
	out, err = svc.ReadMetrics(ctx, telemetry.MetricsRequest{Mode: "search", Compact: true})
	if err != nil || len(out.Compact) != 1 || len(out.Groups) != 0 {
		t.Fatalf("compact: %+v %v", out, err)
	}
	c := out.Compact[0]
	if c.Calls != 9 || c.AvgResultBytes != (8*1000+3000)/9 || c.P50LeMs == nil || *c.P50LeMs != 5 || c.P95LeMs == nil || *c.P95LeMs != 500 {
		t.Fatalf("compact summary: %+v p50=%v p95=%v", c, c.P50LeMs, c.P95LeMs)
	}

	// limit truncates with an explicit flag.
	out, err = svc.ReadMetrics(ctx, telemetry.MetricsRequest{GroupBy: "mode", Limit: 1})
	if err != nil || len(out.Groups) != 1 || !out.Truncated || out.Groups[0].Mode != "search" {
		t.Fatalf("limit: %+v %v", out, err)
	}

	// invalid selectors are refused, not ignored.
	for _, bad := range []telemetry.MetricsRequest{{Mode: "both"}, {GroupBy: "tool"}, {Limit: -1}, {Limit: telemetry.MaxMetricGroups + 1}} {
		if _, err := svc.ReadMetrics(ctx, bad); err == nil {
			t.Fatalf("accepted invalid request %+v", bad)
		}
	}
}
