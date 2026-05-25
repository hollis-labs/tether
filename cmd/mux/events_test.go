package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func TestEventsHistoryCmdJSON(t *testing.T) {
	resetEventsFlags()
	t.Cleanup(resetEventsFlags)

	srv := httptest.NewServer(api.NewHandler(api.Deps{
		EventsStore: &fakeEventsStore{
			all: []events.Event{
				{Seq: 4, At: time.Unix(1700000004, 0).UTC(), Scope: events.ScopeBroker, SessionID: "sess-1", Kind: "broker.envelope_sent", PayloadJSON: `{"id":"m1"}`},
				{Seq: 3, At: time.Unix(1700000003, 0).UTC(), Scope: events.ScopeSession, SessionID: "sess-1", Kind: "session.state_changed", PayloadJSON: `{"to":"running"}`},
			},
		},
	}))
	defer srv.Close()

	catalogDir := writeEventsTestCatalog(t, strings.TrimPrefix(srv.URL, "http://"))
	oldCatalog := catalogPath
	catalogPath = catalogDir
	t.Cleanup(func() { catalogPath = oldCatalog })

	eventsJSONFlag = true
	eventsScopeArg = []string{"session", "broker"}
	eventsKindArg = []string{"session.state_changed", "broker.envelope_sent"}
	eventsSession = "sess-1"
	eventsSinceSeq = 2
	eventsLimit = 2

	out := captureStdout(t, func() {
		if err := eventsHistoryCmd.RunE(eventsHistoryCmd, nil); err != nil {
			t.Fatalf("events history: %v", err)
		}
	})

	var body struct {
		Events []api.EventDTO `json:"events"`
		Count  int            `json:"count"`
		Next   int64          `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("decode events history json: %v\n%s", err, out)
	}
	if body.Count != 2 || len(body.Events) != 2 {
		t.Fatalf("body = %+v", body)
	}
	if body.Next != 3 {
		t.Fatalf("next_cursor = %d body=%+v", body.Next, body)
	}
	if body.Events[0].Kind != "broker.envelope_sent" || body.Events[1].Kind != "session.state_changed" {
		t.Fatalf("events = %+v", body.Events)
	}
}

func TestEventsWatchCmdPretty(t *testing.T) {
	resetEventsFlags()
	t.Cleanup(resetEventsFlags)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/stream" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query()["scope"]; len(got) != 1 || got[0] != "daemon" {
			t.Fatalf("scope query = %v", got)
		}
		if got := r.URL.Query()["kind"]; len(got) != 1 || got[0] != "daemon.started" {
			t.Fatalf("kind query = %v", got)
		}
		if got := r.URL.Query().Get("session_id"); got != "sess-1" {
			t.Fatalf("session_id = %q", got)
		}
		if got := r.URL.Query().Get("since_seq"); got != "4" {
			t.Fatalf("since_seq = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"id: 7\n" +
				"event: daemon.started\n" +
				`data: {"scope":"daemon","session_id":"sess-1","payload_json":"{\"version\":\"0.2.0\"}"}` + "\n\n",
		))
	}))
	defer srv.Close()

	catalogDir := writeEventsTestCatalog(t, strings.TrimPrefix(srv.URL, "http://"))
	oldCatalog := catalogPath
	catalogPath = catalogDir
	t.Cleanup(func() { catalogPath = oldCatalog })

	eventsScopeArg = []string{"daemon"}
	eventsKindArg = []string{"daemon.started"}
	eventsSession = "sess-1"
	eventsSinceSeq = 4

	out := captureStdout(t, func() {
		if err := eventsWatchCmd.RunE(eventsWatchCmd, nil); err != nil {
			t.Fatalf("events watch: %v", err)
		}
	})
	for _, want := range []string{"7", "daemon", "sess-1", "daemon.started", `"version":"0.2.0"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("watch output missing %q: %s", want, out)
		}
	}
}

type fakeEventsStore struct {
	all []events.Event
}

func (f *fakeEventsStore) ListEventsBySession(string, int, int64) ([]events.Event, error) {
	return nil, nil
}

func (f *fakeEventsStore) QueryEvents(filter store.EventFilter) ([]events.Event, error) {
	var out []events.Event
	for _, ev := range f.all {
		if filter.SessionID != "" && ev.SessionID != filter.SessionID {
			continue
		}
		if filter.SinceSeq > 0 && ev.Seq <= filter.SinceSeq {
			continue
		}
		if len(filter.Scopes) > 0 {
			matched := false
			for _, scope := range filter.Scopes {
				if ev.Scope == string(scope) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if len(filter.Kinds) > 0 {
			matched := false
			for _, kind := range filter.Kinds {
				if ev.Kind == kind {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

func writeEventsTestCatalog(t *testing.T, hostport string) string {
	t.Helper()
	dir := t.TempDir()
	doc := "version: 0.1.0\n" +
		"catalog:\n" +
		"  roots:\n" +
		"    projects: projects\n" +
		"    agents: agents\n" +
		"    providers: providers\n" +
		"    launches: launches\n" +
		"    boot: boot\n" +
		"  defaults:\n" +
		"    state_db: ~/.tether/state/tether.db\n" +
		"daemon:\n" +
		"  listen_addr: tcp:" + hostport + "\n" +
		"  shutdown_timeout: 10s\n"
	if err := os.WriteFile(filepath.Join(dir, "global.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile(global.yaml): %v", err)
	}
	return dir
}

func resetEventsFlags() {
	eventsJSONFlag = false
	eventsScopeArg = nil
	eventsKindArg = nil
	eventsSession = ""
	eventsSinceSeq = 0
	eventsCursor = 0
	eventsLimit = 100
}
