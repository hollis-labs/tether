package store

import (
	"github.com/hollis-labs/tether/internal/events"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestProxyTelemetrySurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	details := events.ToolCallDetails{Profile: "readers", DiscoveryMode: "search", ArgsBytes: 77, ResultBytes: 99, Truncated: true, ErrorClass: events.ToolErrorTimeout, TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", QueueMs: 4, ForwardMs: 80}
	if err := db.AppendProxyEvent(ProxyEvent{ToolCallDetails: details, Server: "alpha", ToolName: "read", Timestamp: time.Now()}); err != nil {
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
	rows, err := db.QueryProxyEvents(ProxyEventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !reflect.DeepEqual(rows[0].ToolCallDetails, details) {
		t.Fatalf("durable projection: %+v", rows)
	}
}
