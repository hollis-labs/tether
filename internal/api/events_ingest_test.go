package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"

	"github.com/hollis-labs/tether/internal/eventingest"
	"github.com/hollis-labs/tether/internal/identity"
)

type fakeIngest struct {
	caller  eventingest.Caller
	items   int
	results []ingest.Result
	err     error
}

func (f *fakeIngest) Ingest(_ context.Context, c eventingest.Caller, items []json.RawMessage) ([]ingest.Result, error) {
	f.caller, f.items = c, len(items)
	return f.results, f.err
}

func postEvents(t *testing.T, s *Server, p *identity.Principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	if p != nil {
		r = r.WithContext(identity.WithPrincipal(r.Context(), *p))
	}
	w := httptest.NewRecorder()
	s.handleEvents(w, r)
	return w
}

const oneEvent = `{"schema_version":"1","id":"evt_1","kind":"alert.raised"}`

func svcPrincipal(app string, scopes ...string) *identity.Principal {
	return &identity.Principal{Verified: true, ID: "msg://service/" + app + "/dev", Kind: "service", Scopes: scopes}
}

func TestIngestModes(t *testing.T) {
	ok := []ingest.Result{{ID: "evt_1", Status: ingest.StatusAccepted, Cursor: "7"}}
	cases := []struct {
		name string
		mode identity.Mode
		p    *identity.Principal
		want int
	}{
		{"enforce, anonymous", identity.Enforce, nil, 401},
		{"enforce, no scope", identity.Enforce, svcPrincipal("cerberus", "tether.read"), 403},
		{"enforce, scope", identity.Enforce, svcPrincipal("cerberus", "tether.events"), 200},
		{"enforce, agent token", identity.Enforce, &identity.Principal{Verified: true, ID: "msg://agent/x", Kind: "session", Scopes: []string{"*"}}, 403},
		{"enforce, operator", identity.Enforce, &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}, 200},
		{"enforce, worker device", identity.Enforce, &identity.Principal{Verified: true, ID: "msg://device/w1", Kind: "device", Scopes: []string{"worker"}}, 200},
		{"enforce, read-only device", identity.Enforce, &identity.Principal{Verified: true, ID: "msg://device/r1", Kind: "device", Scopes: []string{"read"}}, 403},
		{"observe, no scope", identity.Observe, svcPrincipal("cerberus", "tether.read"), 200},
		{"observe, anonymous", identity.Observe, nil, 200},
		{"off, anonymous", identity.Off, nil, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeIngest{results: ok}
			s := &Server{EventIngest: f, EventIngestMode: c.mode}
			w := postEvents(t, s, c.p, oneEvent)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d: %s", w.Code, c.want, w.Body)
			}
			if c.want == 200 {
				var resp ingest.Response
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Accepted != 1 || resp.Results[0].Cursor != "7" {
					t.Fatalf("%v %s", err, w.Body)
				}
			} else if f.items != 0 {
				t.Fatal("service was called for a refused caller")
			}
		})
	}
}

func TestIngestBindsServiceToAppAndLeavesDevicesUnbound(t *testing.T) {
	f := &fakeIngest{}
	s := &Server{EventIngest: f, EventIngestMode: identity.Enforce}
	postEvents(t, s, svcPrincipal("cerberus", "tether.events"), oneEvent)
	if f.caller.App != "cerberus" || f.caller.Actor.URN != "msg://service/cerberus/dev" || f.caller.Actor.Verified != "token" || f.caller.Actor.Kind != "service" {
		t.Fatalf("%+v", f.caller)
	}
	postEvents(t, s, &identity.Principal{Verified: true, ID: "msg://device/w1", Kind: "device", Scopes: []string{"worker"}}, oneEvent)
	if f.caller.App != "" || f.caller.Actor.Kind != "device" || f.caller.Actor.URN != "msg://device/w1" || f.caller.Actor.Verified != "token" {
		t.Fatalf("%+v", f.caller)
	}
	postEvents(t, s, &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}, oneEvent)
	if f.caller.App != "" || f.caller.Actor.Role != "operator" {
		t.Fatalf("%+v", f.caller)
	}
	w := postEvents(t, s, &identity.Principal{Verified: true, ID: "msg://user/someone", Kind: "service", Scopes: []string{"tether.events"}}, oneEvent)
	if w.Code != 403 {
		t.Fatalf("service id without an app: %d", w.Code)
	}
}

func TestIngestRequestErrors(t *testing.T) {
	big := `{"id":"` + strings.Repeat("a", ingest.MaxRequestBytes) + `"}`
	many := `{"events":[` + strings.Repeat(`{},`, ingest.MaxBatchEvents) + `{}]}`
	cases := []struct {
		name string
		body string
		err  error
		want int
	}{
		{"not json", "nope", nil, 400},
		{"empty batch", `{"events":[]}`, nil, 400},
		{"too many", many, nil, 413},
		{"too big", big, nil, 413},
		{"app mismatch", oneEvent, eventingest.ErrAppMismatch, 403},
		{"rate limited", oneEvent, &eventingest.RateLimited{RetryAfter: 2500 * time.Millisecond}, 429},
		{"log down", oneEvent, eventingest.ErrUnavailable, 503},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{EventIngest: &fakeIngest{err: c.err}, EventIngestMode: identity.Off}
			w := postEvents(t, s, nil, c.body)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d: %s", w.Code, c.want, w.Body)
			}
			if c.want == 429 && w.Header().Get("Retry-After") != "3" {
				t.Errorf("Retry-After = %q", w.Header().Get("Retry-After"))
			}
			if c.want == 503 && w.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After on 503")
			}
		})
	}
}

func TestPostEventsWithoutIngestIsStillRefused(t *testing.T) {
	s := &Server{}
	w := postEvents(t, s, nil, oneEvent)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", w.Code)
	}
}
