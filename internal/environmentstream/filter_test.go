package environmentstream

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
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
	operator := identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true, Scopes: []string{"*"}}
	wildcardDevice := identity.Principal{ID: "msg://device/abc", Kind: "device", Verified: true, Scopes: []string{"*"}}
	wildcardService := identity.Principal{ID: "msg://service/cerberus/prod", Kind: "service", Verified: true, Scopes: []string{"*"}}
	mintedOperator := identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true, TokenID: "jti1", Scopes: []string{"*"}}
	device := identity.Principal{ID: "msg://device/abc", Kind: "device", Scopes: []string{"read"}}
	cases := []struct {
		name       string
		visibility string
		p          identity.Principal
		has        bool
		local      bool
		want       bool
	}{
		{"environment to device", "environment", device, true, false, true},
		{"empty visibility to device", "", device, true, false, true},
		{"operator event to operator", "operator", operator, true, false, true},
		{"operator event to device", "operator", device, true, false, false},
		{"operator event to a device holding *", "operator", wildcardDevice, true, false, false},
		{"operator event to a service holding *", "operator", wildcardService, true, false, false},
		{"operator event to an operator-minted narrow token", "operator", mintedOperator, true, false, false},
		{"missing principal on the trusted local socket (mode off)", "operator", identity.Principal{}, false, true, true},
		{"missing principal anywhere else (TCP, observe, enforce)", "operator", identity.Principal{}, false, false, false},
		{"unverified operator-shaped principal", "operator", identity.Principal{ID: identity.OperatorID, Kind: "operator"}, true, true, false},
		{"non-operator event with no principal", "environment", identity.Principal{}, false, false, true},
	}
	for _, c := range cases {
		if got := Visible(c.visibility, c.p, c.has, c.local); got != c.want {
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
	// The stream repeats `synchronized` on every poll tick, so an idle repeat of
	// the marker already seen is skipped; a changed marker or any event is not.
	var lastSync int64 = -1
	next := func(r *bufio.Reader) frame {
		t.Helper()
		for {
			f := readFrame(t, r)
			if f.kind == "synchronized" && f.id == lastSync {
				continue
			}
			if f.kind == "synchronized" {
				lastSync = f.id
			}
			return f
		}
	}
	r := open("after_seq=0&kind=wanted.*")
	f := next(r)
	if f.kind != "wanted.one" || f.id != 2 {
		t.Fatalf("first frame %+v", f)
	}
	f = next(r)
	if f.kind != "synchronized" || f.id != 3 {
		t.Fatalf("filtered-out tail did not advance the cursor: %+v", f)
	}
	// Live: a non-matching event moves the marker, a matching one is delivered.
	publish("noise.three")
	f = next(r)
	if f.kind != "synchronized" || f.id != 4 {
		t.Fatalf("live noise: %+v", f)
	}
	publish("wanted.two")
	f = next(r)
	if f.kind != "wanted.two" || f.id != 5 {
		t.Fatalf("live match: %+v", f)
	}
	// A filter matching nothing still reports the head.
	r2 := open("after_seq=0&kind=nothing.*")
	lastSync = -1
	f = next(r2)
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

// The no-principal fallback needs identity mode off AND a Unix-socket listener.
func TestTrustedLocal(t *testing.T) {
	dir, err := os.MkdirTemp("", "tl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var got atomic.Bool
	var mode atomic.Value
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(TrustedLocal(mode.Load().(identity.Mode), r))
	})
	unixSrv := httptest.NewUnstartedServer(handler)
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	unixSrv.Listener = ln
	unixSrv.Start()
	t.Cleanup(unixSrv.Close)
	tcpSrv := httptest.NewServer(handler)
	t.Cleanup(tcpSrv.Close)
	unixClient := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "s"))
	}}}
	cases := []struct {
		name string
		unix bool
		mode identity.Mode
		want bool
	}{
		{"off on the socket", true, identity.Off, true},
		{"unset mode on the socket is off", true, "", true},
		{"off over TCP", false, identity.Off, false},
		{"observe on the socket", true, identity.Observe, false},
		{"permissive on the socket", true, identity.Permissive, false},
		{"enforce on the socket", true, identity.Enforce, false},
		{"observe over TCP", false, identity.Observe, false},
	}
	for _, c := range cases {
		mode.Store(c.mode)
		got.Store(!c.want)
		cl, url := http.DefaultClient, tcpSrv.URL
		if c.unix {
			cl, url = unixClient, "http://unix/"
		}
		resp, err := cl.Get(url)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_ = resp.Body.Close()
		if got.Load() != c.want {
			t.Errorf("%s: TrustedLocal = %v, want %v", c.name, got.Load(), c.want)
		}
	}
}

// End to end: an operator-visibility event reaches only the verified operator.
// Over TCP with no principal (or any other principal, "*" scope included) it is
// withheld, yet the cursor still passes it.
func TestOperatorVisibleEventsReachOnlyTheOperator(t *testing.T) {
	// class.visibility survives only the stored-event projection.
	_, _, bus := streamFixture(t)
	stored := func(seq int64, kind, visibility string) events.Event {
		e := storedExample()
		e.Seq, e.Kind = seq, kind
		e.EventID = fmt.Sprintf("evt_%032x", seq)
		e.Envelope.Class.Visibility = visibility
		return e
	}
	window := store.EnvironmentWindow{
		Events:    []events.Event{stored(1, "secret.one", "operator"), stored(2, "plain.one", "environment")},
		HighWater: 2, EarliestAvailable: 1,
	}
	empty := store.EnvironmentWindow{HighWater: 2, EarliestAvailable: 1}
	newServer := func() *Server {
		ws := []store.EnvironmentWindow{window}
		for i := 0; i < 500; i++ {
			ws = append(ws, empty)
		}
		return &Server{EnvironmentID: "env", Store: &projectionWindows{windows: ws}, Bus: bus,
			StoredIDs: &StoredIDProjection{Cutover: 0}, PollInterval: 10 * time.Millisecond}
	}
	cases := []struct {
		name      string
		principal *identity.Principal
		want      []string
	}{
		{"no principal over TCP", nil, []string{"plain.one"}},
		{"device holding *", &identity.Principal{ID: "msg://device/abc", Kind: "device", Verified: true, Scopes: []string{"*"}}, []string{"plain.one"}},
		{"service holding *", &identity.Principal{ID: "msg://service/x/prod", Kind: "service", Verified: true, Scopes: []string{"*"}}, []string{"plain.one"}},
		{"verified operator", &identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true, Scopes: []string{"*"}}, []string{"secret.one", "plain.one"}},
	}
	for _, c := range cases {
		s := newServer()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.principal != nil {
				r = r.WithContext(identity.WithPrincipal(r.Context(), *c.principal))
			}
			s.Stream(w, r, "")
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?after_seq=0", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(resp.Body)
		var got []string
		for {
			f := readFrame(t, r)
			if f.kind == "synchronized" {
				if f.id != 2 {
					t.Errorf("%s: cursor %d, want 2", c.name, f.id)
				}
				break
			}
			got = append(got, f.kind)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: delivered %v, want %v", c.name, got, c.want)
		}
		_ = resp.Body.Close()
		cancel()
		srv.Close()
	}
}
