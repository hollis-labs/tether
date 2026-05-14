package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// fakeBus is a minimal events.Bus that lets tests drive the
// subscriber channel by hand. Filter is captured so assertions can
// verify the handler built it correctly.
type fakeBus struct {
	mu      sync.Mutex
	filter  events.Filter
	subCh   chan events.Event
	cancels int
}

func (f *fakeBus) Publish(_ context.Context, _ events.Event) error { return nil }

func (f *fakeBus) Subscribe(_ context.Context, filter events.Filter) (<-chan events.Event, func(), error) {
	f.mu.Lock()
	f.filter = filter
	if f.subCh == nil {
		f.subCh = make(chan events.Event, 16)
	}
	f.mu.Unlock()
	cancel := func() {
		f.mu.Lock()
		f.cancels++
		f.mu.Unlock()
	}
	return f.subCh, cancel, nil
}

// fakeEventsStore returns preconfigured rows from ListEventsBySession.
type fakeEventsStore struct {
	byID  map[string][]events.Event
	err   error
	calls []string
}

func (f *fakeEventsStore) ListEventsBySession(id string, _ int, _ int64) ([]events.Event, error) {
	f.calls = append(f.calls, id)
	if f.err != nil {
		return nil, f.err
	}
	return f.byID[id], nil
}

func newEventsTestHandler(bus events.Bus, store EventsStore, svc LaunchService) http.Handler {
	return NewHandler(Deps{Service: svc, Bus: bus, EventsStore: store})
}

func TestHandleEventsStream_SSEFraming(t *testing.T) {
	bus := &fakeBus{}

	srv := httptest.NewServer(newEventsTestHandler(bus, nil, nil))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Let the handler register its subscription, then push one event.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bus.mu.Lock()
		ready := bus.subCh != nil
		bus.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	bus.subCh <- events.Event{
		Seq:         42,
		Scope:       events.ScopeSession,
		SessionID:   "s1",
		Kind:        "session.state_changed",
		PayloadJSON: `{"from":"launching","to":"running"}`,
	}

	// Read the framed block.
	buf := make([]byte, 512)
	_ = resp.Body.(io.Reader)
	n, err := resp.Body.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	chunk := string(buf[:n])
	if !strings.Contains(chunk, "id: 42") {
		t.Errorf("missing id: field: %q", chunk)
	}
	if !strings.Contains(chunk, "event: session.state_changed") {
		t.Errorf("missing event: field: %q", chunk)
	}
	if !strings.Contains(chunk, `"session_id":"s1"`) {
		t.Errorf("missing session_id in data: %q", chunk)
	}

	cancel() // triggers handler return + cancel of subscription
	// Give the handler a moment to unwind.
	time.Sleep(50 * time.Millisecond)
}

func TestHandleEventsStream_BadSinceSeq(t *testing.T) {
	bus := &fakeBus{}
	req := httptest.NewRequest(http.MethodGet, "/events/stream?since_seq=bad", nil)
	rr := httptest.NewRecorder()
	newEventsTestHandler(bus, nil, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleEventsStream_BadScope(t *testing.T) {
	bus := &fakeBus{}
	req := httptest.NewRequest(http.MethodGet, "/events/stream?scope=nope", nil)
	rr := httptest.NewRecorder()
	newEventsTestHandler(bus, nil, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleEventsStream_FiltersForwarded(t *testing.T) {
	bus := &fakeBus{}
	srv := httptest.NewServer(newEventsTestHandler(bus, nil, nil))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/events/stream?since_seq=10&scope=session&scope=daemon&session_id=s1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Give subscription time to register.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bus.mu.Lock()
		ready := bus.subCh != nil
		bus.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	bus.mu.Lock()
	f := bus.filter
	bus.mu.Unlock()

	if f.SinceSeq != 10 {
		t.Errorf("SinceSeq = %d, want 10", f.SinceSeq)
	}
	if f.SessionID != "s1" {
		t.Errorf("SessionID = %q", f.SessionID)
	}
	if len(f.Scopes) != 2 {
		t.Errorf("scopes = %v", f.Scopes)
	}
	cancel()
}

func TestHandleSessionEventsList(t *testing.T) {
	store := &fakeEventsStore{
		byID: map[string][]events.Event{
			"s1": {
				{Seq: 3, At: time.Unix(1700000003, 0).UTC(), Scope: events.ScopeSession, SessionID: "s1", Kind: "k3"},
				{Seq: 2, At: time.Unix(1700000002, 0).UTC(), Scope: events.ScopeSession, SessionID: "s1", Kind: "k2"},
				{Seq: 1, At: time.Unix(1700000001, 0).UTC(), Scope: events.ScopeSession, SessionID: "s1", Kind: "k1"},
			},
		},
	}
	svc := &fakeLaunchService{}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/events?limit=3", nil)
	rr := httptest.NewRecorder()
	newEventsTestHandler(nil, store, svc).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var res EventListResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("len = %d", len(res.Events))
	}
	// Page equals limit → next_cursor = last row's seq.
	if res.NextCursor != 1 {
		t.Errorf("NextCursor = %d, want 1", res.NextCursor)
	}
}

func TestHandleSessionEventsList_BadLimit(t *testing.T) {
	store := &fakeEventsStore{}
	svc := &fakeLaunchService{}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/events?limit=bad", nil)
	rr := httptest.NewRecorder()
	newEventsTestHandler(nil, store, svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestSessionEvents_NotRegisteredWithoutStore(t *testing.T) {
	svc := &fakeLaunchService{}
	h := NewHandler(Deps{Service: svc}) // no EventsStore
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/events", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestEventsStream_NotRegisteredWithoutBus(t *testing.T) {
	h := NewHandler(Deps{Service: &fakeLaunchService{}}) // no Bus
	req := httptest.NewRequest(http.MethodGet, "/events/stream", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}
