package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// The daemon-only `mux mcp` (CW-20261001-0173) has an app.Service with no
// Store: it must never open the state database, so it reads Tether's state
// from the daemon. These tests run its tools against the real API handler
// over a real store, and against an in-process adapter on that same store:
// the two must answer alike.

// daemonOnlyService is the slice of api.LaunchService the session reads use.
type daemonOnlyService struct {
	api.LaunchService // any call outside the overrides below panics: a read the tests did not expect
	db                *store.Store
	health            api.RuntimeHealthResult
	healthOK          bool
}

func (s *daemonOnlyService) ListSessions(o store.ListSessionsOptions) ([]store.SessionRow, error) {
	return s.db.ListSessions(o)
}
func (s *daemonOnlyService) GetSession(id string) (*store.SessionRow, error) {
	return s.db.GetSession(id)
}
func (s *daemonOnlyService) AttachedClients(string) int { return 0 }
func (s *daemonOnlyService) RuntimeHealth(string) (api.RuntimeHealthResult, bool) {
	return s.health, s.healthOK
}

type daemonOnlyFixture struct {
	client     *client.Client
	db         *store.Store
	inProcess  *Adapter // reads the store directly
	daemonOnly *Adapter // no Store: reads the daemon API
	service    *daemonOnlyService
}

func newDaemonOnlyFixture(t *testing.T) *daemonOnlyFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const ts = "2026-04-23T10:00:00Z"
	if err := db.CreateSession(store.SessionRow{
		ID: "sess-1", LaunchID: "launch-1", ProjectID: "proj", LogicalAgentID: "worker",
		ProviderID: "claude-code", ProviderKind: "cli", Workspace: "/ws", State: "running", CreatedAt: ts, UpdatedAt: ts,
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"session.created", "session.state_changed", "session.state_changed"} {
		if _, _, err := db.InsertEvent(events.ScopeSession, "sess-1", kind, `{"k":"`+kind+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateClientAttachment("att-1", "sess-1", "tui", ts); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: "worker", Name: "Worker", Role: "general"}, ts); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateCheckpoint(checkpoint.Checkpoint{
		ID: "cp-1", LogicalAgentID: "worker", Status: "complete", Summary: "did the thing", CreatedAt: ts, SourceSessionID: "sess-1",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, ev := range []store.ProxyEvent{
		{SessionID: "sess-1", Server: "hadron", ToolName: "hadron_a", ArgsSchemaFP: "ab12cd34", DurationMs: 5, OK: true},
		{SessionID: "sess-1", Server: "hadron", ToolName: "hadron_b", DurationMs: 9, OK: false, Error: "boom"},
		{SessionID: "other", Server: "vanta", ToolName: "vanta_c", DurationMs: 1, OK: true},
	} {
		ev.Timestamp = now.Add(time.Duration(i) * time.Second)
		if err := db.AppendProxyEvent(ev); err != nil {
			t.Fatal(err)
		}
	}

	svc := &daemonOnlyService{db: db}
	srv := httptest.NewServer(api.NewHandler(api.Deps{
		Service: svc, EventsStore: db, Checkpoints: db, Attachments: db, ProxyEvents: db, Bus: events.NewBus(events.BusOptions{Persister: db}),
	}))
	t.Cleanup(srv.Close)
	dc := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))

	return &daemonOnlyFixture{
		client:     dc,
		db:         db,
		service:    svc,
		inProcess:  New(&app.Service{Store: db}, "tok", nil),
		daemonOnly: NewWithDaemon(&app.Service{}, dc, "tok", nil), // no Store, as app.NewCatalogOnly leaves it
	}
}

func callAnyTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	c := connectInMemory(t, a.newServer())
	res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

func TestDaemonOnly_ReadsAnswerLikeTheStoreDoes(t *testing.T) {
	f := newDaemonOnlyFixture(t)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"mux_session_events", map[string]any{"session_id": "sess-1"}},
		{"mux_session_events", map[string]any{"session_id": "sess-1", "limit": 2}},
		{"mux_session_checkpoints", map[string]any{"session_id": "sess-1"}},
		{"mux_session_attachments", map[string]any{"session_id": "sess-1"}},
		{"mux_proxy_events", map[string]any{}},
		{"mux_proxy_events", map[string]any{"session_id": "sess-1"}},
		{"mux_proxy_events", map[string]any{"server": "hadron", "errors_only": true}},
		{"mux_proxy_events", map[string]any{"tool_name": "hadron_", "limit": 1}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			want := callAnyTool(t, f.inProcess, tc.tool, tc.args)
			got := callAnyTool(t, f.daemonOnly, tc.tool, tc.args)
			if want.IsError || got.IsError {
				t.Fatalf("error: in-process %q, daemon-only %q", textOf(want), textOf(got))
			}
			if !reflect.DeepEqual(parseToolJSON(t, want), parseToolJSON(t, got)) {
				t.Errorf("answers differ for %v\n in-process: %s\n daemon-only: %s", tc.args, textOf(want), textOf(got))
			}
			if n, _ := parseToolJSON(t, got)["count"].(float64); n == 0 {
				t.Errorf("empty result for %v: the fixture should match it", tc.args)
			}
		})
	}
}

func TestDaemonOnly_SessionReads(t *testing.T) {
	f := newDaemonOnlyFixture(t)

	list := parseToolJSON(t, callAnyTool(t, f.daemonOnly, "mux_session_list", map[string]any{}))
	sessions, _ := list["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("mux_session_list = %v", list)
	}
	if s, _ := sessions[0].(map[string]any); s["id"] != "sess-1" || s["state"] != "running" || s["logical_agent_id"] != "worker" {
		t.Errorf("session = %v", s)
	}

	get := parseToolJSON(t, callAnyTool(t, f.daemonOnly, "mux_session_get", map[string]any{"session_id": "sess-1"}))
	if s, _ := get["session"].(map[string]any); s["id"] != "sess-1" || s["workspace"] != "/ws" {
		t.Errorf("mux_session_get = %v", get)
	}

	res := callAnyTool(t, f.daemonOnly, "mux_session_get", map[string]any{"session_id": "missing"})
	if !res.IsError || !strings.Contains(textOf(res), "not_found") {
		t.Errorf("mux_session_get of an unknown session = %q (error %v), want not_found", textOf(res), res.IsError)
	}
}

// The planted server's mux_session_health used to ask its own, empty
// session manager and so always answered "not running". Daemon-only it asks
// the daemon, which owns the live sessions.
func TestDaemonOnly_SessionHealthComesFromTheDaemon(t *testing.T) {
	f := newDaemonOnlyFixture(t)

	res := callAnyTool(t, f.daemonOnly, "mux_session_health", map[string]any{"session_id": "sess-1"})
	if !res.IsError || !strings.Contains(textOf(res), "conflict") {
		t.Fatalf("health of a session the daemon is not running = %q (error %v), want conflict", textOf(res), res.IsError)
	}

	f.service.healthOK = true
	f.service.health.ProviderID, f.service.health.ProviderKind = "claude-code", "cli"
	f.service.health.Health.Alive = true
	f.service.health.Health.PID = 4242
	body := parseToolJSON(t, callAnyTool(t, f.daemonOnly, "mux_session_health", map[string]any{"session_id": "sess-1"}))
	if body["alive"] != true || body["provider_id"] != "claude-code" || body["pid"] != float64(4242) {
		t.Errorf("health = %v", body)
	}
}

// mux_logical_agent_list reads GET /logical-agents, whose summary carries
// less than the table row the in-process tool returns.
func TestDaemonOnly_LogicalAgentList(t *testing.T) {
	f := newDaemonOnlyFixture(t)
	body := parseToolJSON(t, callAnyTool(t, f.daemonOnly, "mux_logical_agent_list", map[string]any{}))
	agents, _ := body["logical_agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("mux_logical_agent_list = %v", body)
	}
	if a, _ := agents[0].(map[string]any); a["id"] != "worker" || a["name"] != "Worker" {
		t.Errorf("agent = %v", a)
	}
}

func TestDaemonOnly_ADaemonThatIsDownIsADaemonUnavailableError(t *testing.T) {
	dir := t.TempDir()
	a := NewWithDaemon(&app.Service{}, client.New("unix:"+filepath.Join(dir, "no-such.sock")), "tok", nil)
	for _, tool := range []string{"mux_session_get", "mux_session_events", "mux_session_checkpoints", "mux_session_attachments", "mux_session_health"} {
		res := callAnyTool(t, a, tool, map[string]any{"session_id": "sess-1"})
		if !res.IsError || !strings.Contains(textOf(res), "daemon_unavailable") {
			t.Errorf("%s with the daemon down = %q (error %v), want daemon_unavailable", tool, textOf(res), res.IsError)
		}
	}
	for _, tool := range []string{"mux_session_list", "mux_proxy_events", "mux_logical_agent_list"} {
		res := callAnyTool(t, a, tool, map[string]any{})
		if !res.IsError || !strings.Contains(textOf(res), "daemon_unavailable") {
			t.Errorf("%s with the daemon down = %q (error %v), want daemon_unavailable", tool, textOf(res), res.IsError)
		}
	}
}

// A daemon-only adapter has no bus to write the event log with: every tool
// call goes to the daemon as a start and an end record, in order, flagged
// publish so the daemon writes the events row itself.
func TestDaemonToolCallPublisher_PostsStartThenEndWithPublish(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []api.ProxyEventIngestRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/proxy/events" {
			http.NotFound(w, r)
			return
		}
		var req api.ProxyEventIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, req)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewDaemonToolCallPublisher(ctx, client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://")))

	mk := func(kind string, ev events.ToolCallEvent) events.Event {
		raw, _ := json.Marshal(ev)
		return events.Event{Scope: events.ScopeSession, SessionID: ev.SessionID, Kind: kind, PayloadJSON: string(raw)}
	}
	now := time.Now().UTC()
	call := events.ToolCallEvent{SessionID: "s1", ToolName: "hadron_x", Server: "hadron", ArgsSchemaFP: "ab12cd34", Timestamp: now}
	end := call
	end.DurationMs, end.OK = 12, true
	for _, e := range []events.Event{
		mk(events.EventTypeToolCallStart, call),
		mk(events.EventTypeToolCallEnd, end),
		{Scope: events.ScopeDaemon, Kind: "daemon.started", PayloadJSON: `{}`}, // not a tool call: ignored
	} {
		if err := p.Publish(ctx, e); err != nil {
			t.Fatalf("Publish(%s): %v", e.Kind, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a stray third post would arrive by now

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("daemon got %d records, want 2: %+v", len(bodies), bodies)
	}
	if bodies[0].Phase != api.ProxyEventPhaseStart || bodies[1].Phase != api.ProxyEventPhaseEnd {
		t.Errorf("phases = %q, %q, want start then end", bodies[0].Phase, bodies[1].Phase)
	}
	for i, b := range bodies {
		if !b.Publish || b.SessionID != "s1" || b.ToolName != "hadron_x" || b.Server != "hadron" || b.ArgsSchemaFP != "ab12cd34" {
			t.Errorf("record %d = %+v", i, b)
		}
	}
	if !bodies[1].OK || bodies[1].DurationMs != 12 {
		t.Errorf("end record lost the outcome: %+v", bodies[1])
	}
}

// A slow or dead daemon must not hold up a tool call: Publish returns at
// once and drops an event when the queue is full.
func TestDaemonToolCallPublisher_NeverBlocksATool(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewDaemonToolCallPublisher(ctx, client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://")))
	raw, _ := json.Marshal(events.ToolCallEvent{ToolName: "t", Timestamp: time.Now()})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < daemonToolCallQueue*3; i++ {
			_ = p.Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: events.EventTypeToolCallEnd, PayloadJSON: string(raw)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a daemon that does not answer")
	}
}

// A failing call's upstream error can be far larger than the daemon's body
// limit, which refuses a body over it whole. The publisher cuts the error
// before sending, so the end record still arrives; it used to be dropped,
// leaving a start with no proxy_events row, on failing calls specifically.
func TestDaemonToolCallPublisher_AHugeErrorStillRecordsTheCall(t *testing.T) {
	f := newDaemonOnlyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewDaemonToolCallPublisher(ctx, f.client)

	huge := strings.Repeat("upstream said no. ", 100<<10/18) // about 100 KB
	if len(huge) < 90<<10 {
		t.Fatalf("test error is only %d bytes", len(huge))
	}
	mk := func(kind string, ev events.ToolCallEvent) events.Event {
		raw, _ := json.Marshal(ev)
		return events.Event{Scope: events.ScopeSession, SessionID: ev.SessionID, Kind: kind, PayloadJSON: string(raw)}
	}
	call := events.ToolCallEvent{SessionID: "sess-1", ToolName: "huge_error_tool", Server: "hadron", ArgsSchemaFP: "ab12cd34", Timestamp: time.Now().UTC()}
	end := call
	end.OK, end.DurationMs, end.Error = false, 5, huge
	for _, e := range []events.Event{mk(events.EventTypeToolCallStart, call), mk(events.EventTypeToolCallEnd, end)} {
		if err := p.Publish(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	var rows []store.ProxyEvent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ = f.db.QueryProxyEvents(store.ProxyEventFilter{ToolName: "huge_error_tool"})
		if len(rows) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("proxy_events rows for the failing call = %d, want 1: the end record was dropped", len(rows))
	}
	if rows[0].OK || len(rows[0].Error) > api.MaxProxyEventErrorBytes || !strings.HasSuffix(rows[0].Error, "[truncated]") {
		t.Errorf("row = ok %v, error %d bytes ending %q; want a failure with a truncated error within the cap", rows[0].OK, len(rows[0].Error), rows[0].Error[max(0, len(rows[0].Error)-14):])
	}
	evs, err := f.db.ListEventsBySession("sess-1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range evs {
		kinds[e.Kind]++
	}
	if kinds[events.EventTypeToolCallStart] != 1 || kinds[events.EventTypeToolCallEnd] != 1 {
		t.Errorf("event kinds for sess-1 = %v, want one tool_call_start and one tool_call_end: an orphan start", kinds)
	}
}

// The daemon refuses a published record for a session that does not exist.
func TestDaemonToolCallPublisher_ADaemonRefusesAForgedSession(t *testing.T) {
	f := newDaemonOnlyFixture(t)
	err := f.client.IngestProxyEvent(context.Background(), api.ProxyEventIngestRequest{
		SessionID: "no-such-session", ToolName: "t", Phase: api.ProxyEventPhaseEnd, Publish: true,
	})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a record for an unknown session: err = %v, want a 400", err)
	}
	evs, _ := f.db.ListEventsBySession("no-such-session", 0, 0)
	if len(evs) != 0 {
		t.Errorf("events for a session that never ran: %+v", evs)
	}
}
