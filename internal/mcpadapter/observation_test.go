package mcpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

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
	res := callObservationTool(t, a, "mux_events_history", map[string]any{
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

	res := callObservationTool(t, a, "mux_events_history", map[string]any{
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

	res := callObservationTool(t, a, "mux_events_wait", map[string]any{
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

func TestObservationTools_EventsWaitRejectsInvalidScope(t *testing.T) {
	a := NewWithDaemon(&app.Service{}, client.New("tcp:127.0.0.1:1"), "test-token", nil)
	res := callObservationTool(t, a, "mux_events_wait", map[string]any{"scope": "nope"})
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
	res := callObservationTool(t, a, "mux_events_history", map[string]any{"scope": "nope"})
	if !res.IsError {
		t.Fatal("expected invalid scope error")
	}
	body := parseToolJSON(t, res)
	if got := body["code"]; got != "invalid_request" {
		t.Fatalf("code = %v body=%v", got, body)
	}
}

func callObservationTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	s := mcpserver.NewMCPServer("test", version, mcpserver.WithToolCapabilities(true))
	a.registerObservationTools(s)

	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer c.Close()
	if _, err := c.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}
