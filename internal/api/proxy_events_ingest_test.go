package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// recordingBus is an events.Bus that remembers what was published.
type recordingBus struct {
	fakeBus
	pubMu     sync.Mutex
	published []events.Event
}

func (b *recordingBus) Publish(_ context.Context, e events.Event) error {
	b.pubMu.Lock()
	defer b.pubMu.Unlock()
	b.published = append(b.published, e)
	return nil
}

func postProxyEvent(t *testing.T, h http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/proxy/events", strings.NewReader(string(raw)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// sessionsFor is a LaunchService that knows the given sessions.
func sessionsFor(ids ...string) *fakeLaunchService {
	rows := map[string]*store.SessionRow{}
	for _, id := range ids {
		rows[id] = &store.SessionRow{ID: id}
	}
	return &fakeLaunchService{getRes: rows}
}

// A daemon-only `tether mcp` cannot write the event log, so it asks the daemon
// to: POST /proxy/events with publish puts the call on the daemon's bus
// (CW-20261001-0173). The daemon stamps the time itself.
func TestIngestProxyEvent_PublishPutsTheCallOnTheDaemonsBus(t *testing.T) {
	store := &obsProxyEventStore{}
	bus := &recordingBus{}
	h := NewHandler(Deps{ProxyEvents: store, Bus: bus, Service: sessionsFor("s1")})

	claimed := "2001-01-01T00:00:00Z" // an agent-asserted time the daemon must not trust
	for _, phase := range []string{ProxyEventPhaseStart, ProxyEventPhaseEnd} {
		rr := postProxyEvent(t, h, ProxyEventIngestRequest{
			SessionID: "s1", Server: "hadron", ToolName: "hadron_x", ArgsSchemaFP: "ab12cd34",
			DurationMs: 7, OK: phase == ProxyEventPhaseEnd, Timestamp: claimed, Phase: phase, Publish: true,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d, body %s", phase, rr.Code, rr.Body.String())
		}
	}

	if len(bus.published) != 2 {
		t.Fatalf("published %d events, want 2 (start, end)", len(bus.published))
	}
	wantKinds := []string{events.EventTypeToolCallStart, events.EventTypeToolCallEnd}
	for i, e := range bus.published {
		if e.Kind != wantKinds[i] || e.Scope != events.ScopeSession || e.SessionID != "s1" {
			t.Errorf("event %d = %+v, want %s on the session scope of s1", i, e, wantKinds[i])
		}
		var tce events.ToolCallEvent
		if err := json.Unmarshal([]byte(e.PayloadJSON), &tce); err != nil {
			t.Fatalf("event %d payload: %v", i, err)
		}
		if tce.ToolName != "hadron_x" || tce.Server != "hadron" || tce.ArgsSchemaFP != "ab12cd34" {
			t.Errorf("event %d payload = %+v", i, tce)
		}
		if tce.Timestamp.Year() < 2026 || time.Since(tce.Timestamp) > time.Minute {
			t.Errorf("event %d timestamp = %v: the daemon must stamp the time itself, not take the caller's", i, tce.Timestamp)
		}
	}
	// A start record carries no outcome.
	var start events.ToolCallEvent
	_ = json.Unmarshal([]byte(bus.published[0].PayloadJSON), &start)
	if start.OK || start.DurationMs != 0 || start.Error != "" {
		t.Errorf("start event carries an outcome: %+v", start)
	}
	// Only the end is a row in proxy_events, and its time is the daemon's.
	if len(store.events) != 1 || store.events[0].ToolName != "hadron_x" || !store.events[0].OK {
		t.Fatalf("proxy_events rows = %+v, want the one end record", store.events)
	}
	if time.Since(store.events[0].Timestamp) > time.Minute {
		t.Errorf("proxy_events row timestamp = %v: the daemon must stamp it, not take the caller's", store.events[0].Timestamp)
	}
}

// The time of a record is the daemon's whether or not it is published: the
// caller's clock is not recorded, so a record cannot be back-dated.
func TestIngestProxyEvent_AlwaysStampsTheTimeItself(t *testing.T) {
	for _, publish := range []bool{false, true} {
		store := &obsProxyEventStore{}
		h := NewHandler(Deps{ProxyEvents: store, Bus: &recordingBus{}})
		rr := postProxyEvent(t, h, ProxyEventIngestRequest{ToolName: "t", Server: "s", OK: true, Timestamp: "2001-01-01T00:00:00Z", Publish: publish})
		if rr.Code != http.StatusCreated {
			t.Fatalf("publish=%v: status = %d, body %s", publish, rr.Code, rr.Body.String())
		}
		if len(store.events) != 1 || time.Since(store.events[0].Timestamp) > time.Minute {
			t.Errorf("publish=%v: rows = %+v, want one row stamped now, not 2001", publish, store.events)
		}
	}
}

func TestIngestProxyEvent_WithoutPublishWritesOnlyTheRow(t *testing.T) {
	store := &obsProxyEventStore{}
	bus := &recordingBus{}
	h := NewHandler(Deps{ProxyEvents: store, Bus: bus})

	rr := postProxyEvent(t, h, ProxyEventIngestRequest{ToolName: "t", Server: "s", OK: true, SessionID: "any-session"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if len(bus.published) != 0 {
		t.Errorf("published without publish: %+v", bus.published)
	}
	// The operator's own `tether mcp` forwards without publish, and does not
	// need the session to exist (boot-exec has none).
	if len(store.events) != 1 || store.events[0].SessionID != "any-session" {
		t.Errorf("rows = %+v, want the one record", store.events)
	}
}

// A published record lands in the event log and fans out to subscribers, so
// its session must exist: forged events for a session that never ran are
// refused, and nothing is written.
func TestIngestProxyEvent_PublishNeedsAnExistingSession(t *testing.T) {
	store := &obsProxyEventStore{}
	bus := &recordingBus{}
	h := NewHandler(Deps{ProxyEvents: store, Bus: bus, Service: sessionsFor("real")})

	rr := postProxyEvent(t, h, ProxyEventIngestRequest{ToolName: "t", SessionID: "forged", Publish: true})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown session: status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if len(store.events) != 0 || len(bus.published) != 0 {
		t.Errorf("a record for an unknown session was written: rows %+v, published %+v", store.events, bus.published)
	}

	// A call the proxy could not attribute has no session id and goes on the
	// daemon scope, as it always has (CW-20260912-0074).
	rr = postProxyEvent(t, h, ProxyEventIngestRequest{ToolName: "t", Publish: true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("no session id: status = %d, want 201 (body %s)", rr.Code, rr.Body.String())
	}
	if len(bus.published) != 1 || bus.published[0].Scope != events.ScopeDaemon || bus.published[0].SessionID != "" {
		t.Errorf("published = %+v, want one event on the daemon scope", bus.published)
	}

	// With no way to look sessions up, a named session cannot be vouched for.
	rr = postProxyEvent(t, NewHandler(Deps{ProxyEvents: &obsProxyEventStore{}, Bus: &recordingBus{}}), ProxyEventIngestRequest{ToolName: "t", SessionID: "real", Publish: true})
	if rr.Code != http.StatusNotFound {
		t.Errorf("no session lookup: status = %d, want 404", rr.Code)
	}
}

func TestIngestProxyEvent_RefusesAMalformedRecord(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	for name, req := range map[string]ProxyEventIngestRequest{
		"unknown phase":               {ToolName: "t", Phase: "middle", Publish: true},
		"start without publish":       {ToolName: "t", Phase: ProxyEventPhaseStart},
		"no tool name":                {Server: "s", Publish: true},
		"tool name too long":          {ToolName: long(maxProxyEventIDBytes + 1)},
		"server too long":             {ToolName: "t", Server: long(maxProxyEventIDBytes + 1)},
		"session id too long":         {ToolName: "t", SessionID: long(maxProxyEventIDBytes + 1)},
		"schema fingerprint too long": {ToolName: "t", ArgsSchemaFP: long(maxProxyEventFPBytes + 1)},
		"negative duration":           {ToolName: "t", DurationMs: -1},
	} {
		t.Run(name, func(t *testing.T) {
			store := &obsProxyEventStore{}
			bus := &recordingBus{}
			rr := postProxyEvent(t, NewHandler(Deps{ProxyEvents: store, Bus: bus}), req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
			}
			if len(store.events) != 0 || len(bus.published) != 0 {
				t.Errorf("a refused record was written: rows %+v, published %+v", store.events, bus.published)
			}
		})
	}
}

func TestIngestProxyEvent_RefusesAnOversizedBody(t *testing.T) {
	store := &obsProxyEventStore{}
	rr := postProxyEvent(t, NewHandler(Deps{ProxyEvents: store}), map[string]string{
		"tool_name": "t", "error": strings.Repeat("x", maxProxyEventBodyBytes+1),
	})
	if rr.Code != http.StatusBadRequest || len(store.events) != 0 {
		t.Errorf("status = %d, rows = %d, want 400 and none", rr.Code, len(store.events))
	}
}

// The daemon cuts an over-long error itself, so a record is kept; a caller
// should have cut it first (TruncateProxyEventError), since a body over the
// limit is refused whole.
func TestIngestProxyEvent_TruncatesALongError(t *testing.T) {
	store := &obsProxyEventStore{}
	errText := strings.Repeat("a", MaxProxyEventErrorBytes-1) + "€€€"
	rr := postProxyEvent(t, NewHandler(Deps{ProxyEvents: store}), ProxyEventIngestRequest{ToolName: "t", Error: errText})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	got := store.events[0].Error
	if len(got) > MaxProxyEventErrorBytes || !strings.HasSuffix(got, "[truncated]") || !utf8.ValidString(got) {
		t.Errorf("error = %d bytes, valid=%v, ends %q; want within the cap, valid UTF-8, with the truncated marker", len(got), utf8.ValidString(got), got[len(got)-14:])
	}
}

func TestTruncateProxyEventError(t *testing.T) {
	if got := TruncateProxyEventError("short"); got != "short" {
		t.Errorf("a short error changed: %q", got)
	}
	atCap := strings.Repeat("x", MaxProxyEventErrorBytes)
	if got := TruncateProxyEventError(atCap); got != atCap {
		t.Errorf("an error exactly at the cap changed (%d bytes)", len(got))
	}
	for name, in := range map[string]string{
		"ascii":             strings.Repeat("x", 100<<10),
		"multibyte at cut":  strings.Repeat("€", 100<<10),
		"one byte past cap": strings.Repeat("x", MaxProxyEventErrorBytes+1),
	} {
		got := TruncateProxyEventError(in)
		if len(got) > MaxProxyEventErrorBytes {
			t.Errorf("%s: %d bytes, want <= %d", name, len(got), MaxProxyEventErrorBytes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: result is not valid UTF-8: a rune was split", name)
		}
		if !strings.HasSuffix(got, "…[truncated]") {
			t.Errorf("%s: result does not end with the truncated marker", name)
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 10, "abc"},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"a€b", 2, "a"}, // € is bytes 1..3; a cut at 2 backs off to 1
		{"a€b", 4, "a€"},
		{"€", 1, ""},
	} {
		if got := truncateUTF8(tc.in, tc.n); got != tc.want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
