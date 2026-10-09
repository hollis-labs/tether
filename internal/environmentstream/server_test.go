package environmentstream

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func streamFixture(t *testing.T) (*Server, *store.Store, events.Bus) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus := events.NewBus(events.BusOptions{Persister: db})
	s := New("environment-a", db, bus)
	s.PollInterval = 10 * time.Millisecond
	return s, db, bus
}

type blockedWriter struct {
	header  http.Header
	mu      sync.Mutex
	body    strings.Builder
	first   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Header() http.Header { return w.header }
func (w *blockedWriter) WriteHeader(int)     {}
func (w *blockedWriter) Flush()              {}
func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.first); <-w.release })
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func TestSlowClientBacklogIsExplicitGap(t *testing.T) {
	s, _, bus := streamFixture(t)
	s.MaxCatchUp = 2
	publish := func() {
		t.Helper()
		if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeDaemon, Kind: "tick", PayloadJSON: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	w := &blockedWriter{header: make(http.Header), first: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/?after_seq=0", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.Stream(w, r, "") }()
	select {
	case <-w.first:
	case <-ctx.Done():
		t.Fatal("stream did not start")
	}
	for range 4 {
		publish()
	}
	close(w.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("slow backlog did not close")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !strings.Contains(w.body.String(), `"reason":"too_large"`) {
		t.Fatalf("silent backlog skip: %s", w.body.String())
	}
}

type closingBus struct {
	events.Bus
	live chan events.Event
}

func (b *closingBus) SubscribeLive(context.Context, events.Filter) (<-chan events.Event, func(), error) {
	return b.live, func() {}, nil
}

func TestSubscriptionRestartGapAndDurableReconnect(t *testing.T) {
	s, db, bus := streamFixture(t)
	if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeDaemon, Kind: "tick", PayloadJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	closed := make(chan events.Event)
	close(closed)
	s.Bus = &closingBus{Bus: bus, live: closed}
	w := httptest.NewRecorder()
	s.Stream(w, httptest.NewRequest(http.MethodGet, "/?after_seq=1", nil), "")
	if !strings.Contains(w.Body.String(), `"reason":"restart"`) {
		t.Fatalf("missing restart gap %s", w.Body.String())
	}
	// Rebuild the bus with the same durable store, as a daemon restart does.
	s.Bus = events.NewBus(events.BusOptions{Persister: db})
	if err := s.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeDaemon, Kind: "next", PayloadJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w2 := &cancelOnSyncWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	s.Stream(w2, httptest.NewRequest(http.MethodGet, "/?after_seq=1", nil).WithContext(ctx), "")
	if !strings.Contains(w2.Body.String(), "event: next") || !strings.Contains(w2.Body.String(), "id: 2") {
		t.Fatalf("durable reconnect %s", w2.Body.String())
	}
}

type cancelOnSyncWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *cancelOnSyncWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if strings.Contains(string(p), "event: synchronized") {
		w.cancel()
	}
	return n, err
}

func TestPendingReducerConcurrentRequestsAndSnapshot(t *testing.T) {
	s := Session{ID: "s"}
	setState(&s, "running")
	applyRequest(&s, store.EnvironmentRequest{TurnID: "t", RequestID: "q1", Kind: "question", Open: true, SourceSequence: 1})
	applyRequest(&s, store.EnvironmentRequest{TurnID: "t", RequestID: "q2", Kind: "question", Open: true, SourceSequence: 2})
	applyRequest(&s, store.EnvironmentRequest{TurnID: "t", RequestID: "a", Kind: "approval", Open: true, SourceSequence: 3})
	applyRequest(&s, store.EnvironmentRequest{TurnID: "other", RequestID: "q1", Open: false, SourceSequence: 4})
	applyRequest(&s, store.EnvironmentRequest{TurnID: "t", RequestID: "q1", Open: false, SourceSequence: 5})
	if !s.PendingQuestion || !s.PendingApproval || !s.PendingKnown {
		t.Fatalf("concurrent requests %+v", s)
	}
	setState(&s, "detached")
	if !s.PendingApproval || s.InstanceStatus != mesh.InstanceWaiting {
		t.Fatal("detach cleared pending")
	}
	applyRequest(&s, store.EnvironmentRequest{TurnID: "t", RequestID: "q2", Open: false, SourceSequence: 6})
	applyRequest(&s, store.EnvironmentRequest{TurnID: "t", RequestID: "q2", Kind: "question", Open: true, SourceSequence: 2})
	if s.PendingQuestion || !s.PendingApproval {
		t.Fatalf("stale request reopened %+v", s)
	}
	setState(&s, "failed")
	if s.PendingQuestion || s.PendingApproval || !s.PendingKnown {
		t.Fatal("actual terminal did not clear")
	}
}

func TestMeshEventFlatRoundTripUnknownKind(t *testing.T) {
	e, err := Map("environment-a", events.Event{Seq: 42, At: time.Now().UTC(), Scope: "custom", SessionID: "s", Kind: "unknown.future.kind", PayloadJSON: `{"nested":{"seq":8,"environment_id":"payload-only"}}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["seq"]) != "42" || string(fields["environment_id"]) != `"environment-a"` || fields["Event"] != nil {
		t.Fatalf("flattened wire %s", raw)
	}
	var recovered Event
	if err = json.Unmarshal(raw, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Seq != 42 || recovered.SourceSequence != 42 || recovered.Cursor != "42" || string(recovered.Payload) != string(e.Payload) {
		t.Fatalf("roundtrip %+v", recovered)
	}
	var published mesh.Event
	if err = json.Unmarshal(raw, &published); err != nil {
		t.Fatal(err)
	}
	if err = published.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err = Map("e", events.Event{Seq: 1, At: time.Now(), Kind: "bad", PayloadJSON: "not JSON"}); err == nil {
		t.Fatal("invalid persisted payload accepted")
	}
}

type frame struct {
	kind string
	id   int64
	data string
}

func readFrame(t *testing.T, r *bufio.Reader) frame {
	t.Helper()
	var f frame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			return f
		}
		if strings.HasPrefix(line, "event: ") {
			f.kind = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "id: ") {
			f.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		}
		if strings.HasPrefix(line, "data: ") {
			f.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestSnapshotCutReplayLiveDedupeAndFilteredGlobalCursor(t *testing.T) {
	s, db, bus := streamFixture(t)
	if err := db.CreateSession(store.SessionRow{ID: "a", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	publish := func(id string) {
		t.Helper()
		if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: id, Kind: "custom.event", PayloadJSON: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	publish("a")
	base, err := db.EnvironmentSnapshot(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	publish("b")
	publish("a")
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Stream(w, r, "a") }))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"?after_seq="+strconv.FormatInt(base.HighWater, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	f := readFrame(t, r)
	if f.kind != "custom.event" || f.id != 3 {
		t.Fatalf("filtered replay %+v", f)
	}
	f = readFrame(t, r)
	if f.kind != "synchronized" || f.id != 3 {
		t.Fatalf("sync %+v", f)
	}
	publish("b")
	publish("a")
	seen := 0
	for seen < 1 {
		f = readFrame(t, r)
		if f.kind == "custom.event" {
			seen++
			if f.id != 5 {
				t.Fatalf("live duplicated/renumbered %+v", f)
			}
		}
	}
	// Reconnecting from a filtered global cursor replays exactly its suffix.
	resp.Body.Close()
	cancel()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	req, err = http.NewRequestWithContext(ctx2, http.MethodGet, httpServer.URL+"?after_seq=5", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := httpServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	f = readFrame(t, bufio.NewReader(resp2.Body))
	if f.kind != "synchronized" || f.id != 5 {
		t.Fatalf("reconnect duplicated %+v", f)
	}
}

func TestStreamGapsRequireSnapshot(t *testing.T) {
	for _, reason := range []string{"purged", "ahead", "too_large"} {
		t.Run(reason, func(t *testing.T) {
			s, db, bus := streamFixture(t)
			for range 3 {
				if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeDaemon, Kind: "daemon.tick", PayloadJSON: `{}`}); err != nil {
					t.Fatal(err)
				}
			}
			after := "0"
			switch reason {
			case "purged":
				if _, err := db.DB().Exec(`DELETE FROM events WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			case "ahead":
				after = "4"
			case "too_large":
				s.MaxCatchUp = 2
			}
			w := httptest.NewRecorder()
			s.Stream(w, httptest.NewRequest(http.MethodGet, "/?after_seq="+after, nil), "")
			if !strings.Contains(w.Body.String(), `"reason":"`+reason+`"`) || !strings.Contains(w.Body.String(), `"snapshot_required":true`) || strings.Contains(w.Body.String(), "event: synchronized") {
				t.Fatalf("gap response %s", w.Body.String())
			}
		})
	}
}

func TestReducerDetachedOrphanedRemainNonterminal(t *testing.T) {
	s := Session{ID: "s"}
	for _, state := range []string{"running", "detached", "orphaned"} {
		e, err := Map("e", events.Event{Seq: 1, At: time.Now(), Kind: events.KindSessionStateChanged, SessionID: "s", PayloadJSON: `{"to":"` + state + `"}`})
		if err != nil {
			t.Fatal(err)
		}
		Reduce(&s, e)
		if s.InstanceStatus != mesh.InstanceRunning || s.SessionState == mesh.SessionEnded {
			t.Fatalf("implicit terminal %+v", s)
		}
	}
	setState(&s, "failed")
	if s.InstanceDetail.Stopped != mesh.StopFailed || s.SessionState != mesh.SessionEnded {
		t.Fatalf("terminal %+v", s)
	}
}
