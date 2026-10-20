package environmentstream

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
)

func meshEvent(kind, app, subject, session string, class *mesh.EventClass, scope *mesh.EventScope) Event {
	return Event{Event: mesh.Event{Kind: kind, App: app, Subject: mesh.URN(subject), SessionID: session, Class: class, Scope: scope}}
}

func TestFilterMatch(t *testing.T) {
	ev := meshEvent("task.state_changed", "torque", "msg://task/torque/CW-1", "",
		&mesh.EventClass{Category: "state", Severity: "warning"},
		&mesh.EventScope{TaskID: "CW-1", ProjectID: "p1"})
	sess := meshEvent("session.idle", "tether", "urn:session:s1", "s1", nil, nil)
	cases := []struct {
		name  string
		query string
		ev    Event
		want  bool
	}{
		{"empty matches all", "", ev, true},
		{"kind exact", "kind=task.state_changed", ev, true},
		{"kind exact miss", "kind=task.state_change", ev, false},
		{"kind prefix", "kind=task.*", ev, true},
		{"kind prefix miss", "kind=run.*", ev, false},
		{"kind OR", "kind=run.*&kind=task.state_changed", ev, true},
		{"category", "category=state", ev, true},
		{"category miss", "category=alert", ev, false},
		{"category on classless event", "category=state", sess, false},
		{"subject prefix", "subject=msg://task/torque/", ev, true},
		{"subject prefix miss", "subject=msg://run/", ev, false},
		{"app", "app=torque", ev, true},
		{"app miss", "app=cerberus", ev, false},
		{"severity at min", "severity_min=warning", ev, true},
		{"severity below min", "severity_min=error", ev, false},
		{"severity unset counts as info", "severity_min=info", sess, true},
		{"severity unset below notice", "severity_min=notice", sess, false},
		{"scope task", "scope.task_id=CW-1", ev, true},
		{"scope task miss", "scope.task_id=CW-2", ev, false},
		{"scope task OR", "scope.task_id=CW-2&scope.task_id=CW-1", ev, true},
		{"scope on event without scope", "scope.task_id=CW-1", sess, false},
		{"scope session_id from envelope", "scope.session_id=s1", sess, true},
		{"AND across params", "kind=task.*&app=torque&scope.project_id=p1", ev, true},
		{"AND across params miss", "kind=task.*&app=torque&scope.project_id=p2", ev, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := url.ParseQuery(c.query)
			if err != nil {
				t.Fatal(err)
			}
			f, err := ParseFilter(q)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.Match(c.ev); got != c.want {
				t.Fatalf("Match = %v, want %v", got, c.want)
			}
			if (c.query == "") != f.Empty() {
				t.Fatalf("Empty() = %v for %q", f.Empty(), c.query)
			}
		})
	}
}

func TestParseFilterRejectsTypos(t *testing.T) {
	for _, q := range []string{"scope.taskid=x", "scope.task_id=", "severity_min=loud", "severity_min=info&severity_min=error", "kind=", "app="} {
		v, _ := url.ParseQuery(q)
		if _, err := ParseFilter(v); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
}

func TestVisible(t *testing.T) {
	operator := identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	device := identity.Principal{ID: "msg://device/abc", Kind: "device", Scopes: []string{"read"}}
	cases := []struct {
		name       string
		visibility string
		p          identity.Principal
		has        bool
		want       bool
	}{
		{"environment to device", "environment", device, true, true},
		{"empty visibility to device", "", device, true, true},
		{"operator event to operator", "operator", operator, true, true},
		{"operator event to device", "operator", device, true, false},
		{"operator event with no principal (auth off)", "operator", identity.Principal{}, false, true},
	}
	for _, c := range cases {
		if got := Visible(c.visibility, c.p, c.has); got != c.want {
			t.Errorf("%s: Visible = %v, want %v", c.name, got, c.want)
		}
	}
}

// A filter decides what is written, never what is consumed: events that do not
// match still advance the stream, so `synchronized` carries the head and a
// reconnect from it replays nothing.
func TestFilteredStreamStillAdvancesCursor(t *testing.T) {
	s, _, bus := streamFixture(t)
	publish := func(kind string) {
		t.Helper()
		if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeDaemon, Kind: kind, PayloadJSON: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	publish("noise.one")
	publish("wanted.one")
	publish("noise.two")
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Stream(w, r, "") }))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	open := func(query string) *bufio.Reader {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := httpServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return bufio.NewReader(resp.Body)
	}
	r := open("after_seq=0&kind=wanted.*")
	f := readFrame(t, r)
	if f.kind != "wanted.one" || f.id != 2 {
		t.Fatalf("first frame %+v", f)
	}
	f = readFrame(t, r)
	if f.kind != "synchronized" || f.id != 3 {
		t.Fatalf("filtered-out tail did not advance the cursor: %+v", f)
	}
	// Live: a non-matching event moves the marker, a matching one is delivered.
	publish("noise.three")
	f = readFrame(t, r)
	if f.kind != "synchronized" || f.id != 4 {
		t.Fatalf("live noise: %+v", f)
	}
	publish("wanted.two")
	f = readFrame(t, r)
	if f.kind != "wanted.two" || f.id != 5 {
		t.Fatalf("live match: %+v", f)
	}
	// A filter matching nothing still reports the head.
	r2 := open("after_seq=0&kind=nothing.*")
	f = readFrame(t, r2)
	if f.kind != "synchronized" || f.id < 5 {
		t.Fatalf("empty selection: %+v", f)
	}
}

func TestStreamRejectsInvalidFilter(t *testing.T) {
	s, _, _ := streamFixture(t)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Stream(w, r, "") }))
	defer httpServer.Close()
	resp, err := http.Get(httpServer.URL + "?after_seq=0&scope.nope=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}
