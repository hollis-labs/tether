package mcpadapter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func TestObservationTools_EventsHistory(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "obs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, _, err := db.InsertEvent(events.ScopeDaemon, "", "daemon.started", `{"pid":1}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InsertEvent(events.ScopeSession, "sess-1", "session.state_changed", `{"to":"running"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InsertEvent(events.ScopeBroker, "sess-1", "broker.envelope_sent", `{"id":"m1"}`); err != nil {
		t.Fatal(err)
	}

	a := New(&app.Service{Store: db}, "", nil)
	res := callObservationTool(t, a, "tether_events_history", map[string]any{
		"scope":      "session,broker",
		"kind":       "session.state_changed,broker.envelope_sent",
		"session_id": "sess-1",
		"since_seq":  1,
		"limit":      2,
	})
	if res.IsError {
		t.Fatalf("events history error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if got, _ := body["count"].(float64); int(got) != 2 {
		t.Fatalf("events history body = %v", body)
	}
	if got, _ := body["next_cursor"].(float64); int(got) != 2 {
		t.Fatalf("next_cursor body = %v", body)
	}
	evs, _ := body["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("events = %v", body)
	}
	first, _ := evs[0].(map[string]any)
	second, _ := evs[1].(map[string]any)
	if first["kind"] != "broker.envelope_sent" || second["kind"] != "session.state_changed" {
		t.Fatalf("events order/filter = %v", evs)
	}
}

func TestObservationTools_EventsHistoryViaDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if got := q["scope"]; len(got) != 2 || got[0] != "session" || got[1] != "broker" {
			t.Fatalf("scope query = %v", got)
		}
		if got := q["kind"]; len(got) != 2 || got[0] != "session.state_changed" || got[1] != "broker.envelope_sent" {
			t.Fatalf("kind query = %v", got)
		}
		if got := q.Get("session_id"); got != "sess-1" {
			t.Fatalf("session_id = %q", got)
		}
		if got := q.Get("since_seq"); got != "1" {
			t.Fatalf("since_seq = %q", got)
		}
		if got := q.Get("limit"); got != "10" {
			t.Fatalf("limit = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[{"seq":3,"at":"2026-05-25T18:42:00Z","scope":"broker","session_id":"sess-1","kind":"broker.envelope_sent","payload_json":"{\"id\":\"m1\"}"}]}`))
	}))
	defer srv.Close()

	hostport := srv.URL[len("http://"):]
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+hostport), "test-token", nil)

	res := callObservationTool(t, a, "tether_events_history", map[string]any{
		"scope":      "session,broker",
		"kind":       "session.state_changed,broker.envelope_sent",
		"session_id": "sess-1",
		"since_seq":  1,
		"limit":      10,
	})
	if res.IsError {
		t.Fatalf("events history error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if got, _ := body["count"].(float64); int(got) != 1 {
		t.Fatalf("events history body = %v", body)
	}
	if got, _ := body["next_cursor"].(float64); int(got) != 0 {
		t.Fatalf("next_cursor body = %v", body)
	}
	evs, _ := body["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events = %v", body)
	}
	ev, _ := evs[0].(map[string]any)
	if ev["kind"] != "broker.envelope_sent" || ev["scope"] != "broker" || ev["session_id"] != "sess-1" {
		t.Fatalf("event = %v", ev)
	}
}

func TestObservationTools_EventsWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/stream" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if got := q["scope"]; len(got) != 2 || got[0] != "daemon" || got[1] != "session" {
			t.Fatalf("scope query = %v", got)
		}
		if got := q["kind"]; len(got) != 2 || got[0] != "ai.budget_rejected" || got[1] != "session.state_changed" {
			t.Fatalf("kind query = %v", got)
		}
		if got := q.Get("session_id"); got != "sess-1" {
			t.Fatalf("session_id = %q", got)
		}
		if got := q.Get("since_seq"); got != "10" {
			t.Fatalf("since_seq = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"id: 12\n" +
				"event: session.state_changed\n" +
				`data: {"scope":"session","session_id":"sess-1","payload_json":"{\"state\":\"running\"}"}` + "\n\n",
		))
	}))
	defer srv.Close()

	hostport := srv.URL[len("http://"):]
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+hostport), "test-token", nil)

	res := callObservationTool(t, a, "tether_events_wait", map[string]any{
		"scope":      "daemon,session",
		"kind":       "ai.budget_rejected,session.state_changed",
		"session_id": "sess-1",
		"since_seq":  10,
		"wait_ms":    100,
		"max_events": 1,
	})
	if res.IsError {
		t.Fatalf("events wait error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if got, _ := body["count"].(float64); int(got) != 1 {
		t.Fatalf("events wait body = %v", body)
	}
	if got, _ := body["next_since_seq"].(float64); int(got) != 12 {
		t.Fatalf("next_since_seq = %v body=%v", got, body)
	}
	evs, _ := body["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events = %v", body)
	}
	ev, _ := evs[0].(map[string]any)
	if ev["kind"] != "session.state_changed" || ev["scope"] != "session" || ev["session_id"] != "sess-1" {
		t.Fatalf("event = %v", ev)
	}
}

func TestObservationTools_EventsWaitEnvironmentCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/environment/events" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Tether-Protocol") != "1" {
			t.Errorf("Tether-Protocol = %q", r.Header.Get("Tether-Protocol"))
		}
		q := r.URL.Query()
		if got := q.Get("after_seq"); got != "10" {
			t.Errorf("after_seq = %q", got)
		}
		if got := q["kind"]; len(got) != 2 || got[0] != "task.*" || got[1] != "run.state_changed" {
			t.Errorf("kind = %v", got)
		}
		if got := q.Get("category"); got != "state" {
			t.Errorf("category = %q", got)
		}
		if got := q.Get("severity_min"); got != "warning" {
			t.Errorf("severity_min = %q", got)
		}
		if got := q["scope.task_id"]; len(got) != 1 || got[0] != "CW-1" {
			t.Errorf("scope.task_id = %v", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"id: 12\nevent: task.state_changed\n" +
				`data: {"kind":"task.state_changed","seq":12,"environment_id":"e"}` + "\n\n" +
				"id: 15\nevent: synchronized\n" +
				`data: {"environment_id":"e","seq":15}` + "\n\n",
		))
	}))
	defer srv.Close()
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+srv.URL[len("http://"):]), "test-token", nil)
	res := callObservationTool(t, a, "tether_events_wait", map[string]any{
		"after_seq": 10, "kind": "task.*,run.state_changed", "category": "state",
		"severity_min": "warning", "task_id": "CW-1", "wait_ms": 200, "max_events": 5,
	})
	if res.IsError {
		t.Fatalf("events wait error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if got, _ := body["count"].(float64); int(got) != 1 {
		t.Fatalf("body = %v", body)
	}
	// The synchronized marker moves the cursor past the filtered-out range.
	if got, _ := body["next_cursor"].(float64); int(got) != 15 {
		t.Fatalf("next_cursor = %v body=%v", got, body)
	}
	evs, _ := body["events"].([]any)
	ev, _ := evs[0].(map[string]any)
	if ev["kind"] != "task.state_changed" || ev["environment_id"] != "e" {
		t.Fatalf("event = %v", ev)
	}
}

func TestObservationTools_EventsWaitEnvironmentFilterNeedsCursor(t *testing.T) {
	a := NewWithDaemon(&app.Service{}, client.New("tcp:127.0.0.1:1"), "test-token", nil)
	res := callObservationTool(t, a, "tether_events_wait", map[string]any{"category": "state"})
	if !res.IsError {
		t.Fatal("expected an error without after_seq")
	}
	if got := parseToolJSON(t, res)["code"]; got != "invalid_request" {
		t.Fatalf("code = %v", got)
	}
}

func TestObservationTools_EventsWaitRejectsInvalidScope(t *testing.T) {
	a := NewWithDaemon(&app.Service{}, client.New("tcp:127.0.0.1:1"), "test-token", nil)
	res := callObservationTool(t, a, "tether_events_wait", map[string]any{"scope": "nope"})
	if !res.IsError {
		t.Fatal("expected invalid scope error")
	}
	body := parseToolJSON(t, res)
	if got := body["code"]; got != "invalid_request" {
		t.Fatalf("code = %v body=%v", got, body)
	}
}

func TestObservationTools_EventsHistoryRejectsInvalidScope(t *testing.T) {
	a := New(&app.Service{}, "", nil)
	res := callObservationTool(t, a, "tether_events_history", map[string]any{"scope": "nope"})
	if !res.IsError {
		t.Fatal("expected invalid scope error")
	}
	body := parseToolJSON(t, res)
	if got := body["code"]; got != "invalid_request" {
		t.Fatalf("code = %v body=%v", got, body)
	}
}

func callObservationTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	s := gomcp.NewServer("test", "test")
	a.registerObservationTools(s)

	c := connectInMemory(t, s)

	res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

// A stream that ends right after its frames (EOF) must still hand over every
// buffered frame and the final cursor: the wait may not end on the closed error
// channel ahead of frames the reader has not taken yet.
func TestObservationTools_EventsWaitEnvironmentDrainsFramesBeforeEOF(t *testing.T) {
	const n = 40
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "id: %d\nevent: task.state_changed\ndata: {\"kind\":\"task.state_changed\",\"seq\":%d}\n\n", i, i)
		}
		fmt.Fprintf(&b, "id: %d\nevent: synchronized\ndata: {\"seq\":%d}\n\n", n+5, n+5)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+srv.URL[len("http://"):]), "test-token", nil)
	for i := 0; i < 25; i++ {
		res := callObservationTool(t, a, "tether_events_wait", map[string]any{"after_seq": 0, "kind": "task.*", "wait_ms": 500, "max_events": 100})
		if res.IsError {
			t.Fatalf("run %d: %s", i, textOf(res))
		}
		body := parseToolJSON(t, res)
		if c, _ := body["count"].(float64); int(c) != n {
			t.Fatalf("run %d: count = %v, want %d (events lost at EOF)", i, c, n)
		}
		if c, _ := body["next_cursor"].(float64); int(c) != n+5 {
			t.Fatalf("run %d: next_cursor = %v, want %d", i, c, n+5)
		}
	}
}
