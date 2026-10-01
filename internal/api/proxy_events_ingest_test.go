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

	"github.com/hollis-labs/tether/internal/events"
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

// A daemon-only `mux mcp` cannot write the event log, so it asks the daemon
// to: POST /proxy/events with publish puts the call on the daemon's bus
// (CW-20261001-0173). The daemon stamps the time itself.
func TestIngestProxyEvent_PublishPutsTheCallOnTheDaemonsBus(t *testing.T) {
	store := &obsProxyEventStore{}
	bus := &recordingBus{}
	h := NewHandler(Deps{ProxyEvents: store, Bus: bus})

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

func TestIngestProxyEvent_WithoutPublishBehavesAsBefore(t *testing.T) {
	store := &obsProxyEventStore{}
	bus := &recordingBus{}
	h := NewHandler(Deps{ProxyEvents: store, Bus: bus})

	rr := postProxyEvent(t, h, ProxyEventIngestRequest{ToolName: "t", Server: "s", OK: true, Timestamp: "2026-04-23T10:00:00Z"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if len(bus.published) != 0 {
		t.Errorf("published without publish: %+v", bus.published)
	}
	if len(store.events) != 1 || !store.events[0].Timestamp.Equal(time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("rows = %+v, want one row with the caller's timestamp", store.events)
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

func TestIngestProxyEvent_TruncatesALongErrorOnARuneBoundary(t *testing.T) {
	store := &obsProxyEventStore{}
	// A 3-byte rune straddling the limit: the cut must not split it.
	errText := strings.Repeat("a", maxProxyEventErrorBytes-1) + "€€€"
	rr := postProxyEvent(t, NewHandler(Deps{ProxyEvents: store}), ProxyEventIngestRequest{ToolName: "t", Error: errText})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	got := store.events[0].Error
	if len(got) > maxProxyEventErrorBytes {
		t.Errorf("error is %d bytes, want <= %d", len(got), maxProxyEventErrorBytes)
	}
	if got != strings.Repeat("a", maxProxyEventErrorBytes-1) {
		t.Errorf("error ends %q: a rune was split or the cut is off", got[len(got)-4:])
	}
}

func TestIngestProxyEvent_PublishWithoutABusIsNotFound(t *testing.T) {
	store := &obsProxyEventStore{}
	rr := postProxyEvent(t, NewHandler(Deps{ProxyEvents: store}), ProxyEventIngestRequest{ToolName: "t", Publish: true})
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
	if len(store.events) != 0 {
		t.Errorf("the record was written though the publish it asked for cannot happen: %+v", store.events)
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
