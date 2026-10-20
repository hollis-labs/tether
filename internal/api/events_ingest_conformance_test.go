package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/libs/mesh/eventbus"

	"github.com/hollis-labs/tether/internal/eventingest"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// Conformance: the libs mesh/eventbus client against the real POST /events
// handler, ingest service, event bus and SQLite store. It pins the contract
// between the two sides: per-item verdicts, duplicates, final rejections,
// app_mismatch, rate limiting and a hub whose ingest is not enabled yet.

type confHub struct {
	srv *httptest.Server
	db  *store.Store
	bus events.OutboxBus
}

func newConfHub(t *testing.T, mode identity.Mode, principal *identity.Principal, cfg eventingest.Config, ingestOn bool) *confHub {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "conf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus := events.NewBus(events.BusOptions{Persister: db}).(events.OutboxBus)
	t.Cleanup(bus.StartOutbox(context.Background()))
	s := &Server{EventsStore: db, EventIngestMode: mode}
	if ingestOn {
		svc, err := eventingest.New(bus, db, cfg)
		if err != nil {
			t.Fatal(err)
		}
		s.EventIngest = svc
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal != nil {
			r = r.WithContext(identity.WithPrincipal(r.Context(), *principal))
		}
		s.handleEvents(w, r)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &confHub{srv: srv, db: db, bus: bus}
}

type confClient struct {
	*eventbus.Client
	mu          sync.Mutex
	rejected    []string
	quarantined []string
	errs        []error
}

func newConfClient(t *testing.T, url string, mut func(*eventbus.Config)) *confClient {
	t.Helper()
	cc := &confClient{}
	cfg := eventbus.Config{URL: url, FlushInterval: 20 * time.Millisecond, BackoffBase: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond, Timeout: 3 * time.Second}
	cfg.OnReject = func(id, code, msg string) { cc.mu.Lock(); cc.rejected = append(cc.rejected, code); cc.mu.Unlock() }
	cfg.OnQuarantine = func(_ eventbus.Item, reason, _ string) {
		cc.mu.Lock()
		cc.quarantined = append(cc.quarantined, reason)
		cc.mu.Unlock()
	}
	cfg.OnError = func(err error) { cc.mu.Lock(); cc.errs = append(cc.errs, err); cc.mu.Unlock() }
	if mut != nil {
		mut(&cfg)
	}
	c, err := eventbus.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cc.Client = c
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return cc
}

func confEvent(t *testing.T, app, kind string) eventbus.Item {
	t.Helper()
	id := runtimeevents.NewEventID()
	b, err := json.Marshal(map[string]any{
		"schema_version": "1", "id": id, "kind": kind, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"app": app, "sequence": 1, "generation": 1,
		"source":  map[string]any{"channel": "conformance", "confidence": "exact"},
		"subject": "msg://alert/" + app + "/" + id[4:12], "payload": map[string]any{"rule_id": app + ".test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return eventbus.Item{ID: id, Priority: eventbus.PriorityHigh, Body: b}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClientDeliversBatchesAndDuplicatesAreSuccess(t *testing.T) {
	hub := newConfHub(t, identity.Off, nil, eventingest.Config{}, true)
	c := newConfClient(t, hub.srv.URL, nil)
	items := []eventbus.Item{confEvent(t, "cerberus", "alert.raised"), confEvent(t, "cerberus", "alert.raised"), confEvent(t, "cerberus", "alert.cleared")}
	for _, it := range items {
		c.Publish(it)
	}
	waitFor(t, "3 sent", func() bool { return c.Stats().Sent == 3 })
	for _, it := range items {
		if _, ok, _ := hub.db.EventByEventID(context.Background(), it.ID); !ok {
			t.Fatalf("%s not stored", it.ID)
		}
	}
	// Resending an event the hub already has is a duplicate: success, no new row.
	c.Publish(items[0])
	waitFor(t, "duplicate counted", func() bool { return c.Stats().Duplicates == 1 })
	if s := c.Stats(); s.Sent != 4 || s.Rejected != 0 || s.Quarantined != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestClientRejectionsAreFinalAndDoNotBlockTheQueue(t *testing.T) {
	hub := newConfHub(t, identity.Off, nil, eventingest.Config{}, true)
	c := newConfClient(t, hub.srv.URL, nil)
	bad := confEvent(t, "cerberus", "task.state_changed") // torque's kind, not cerberus's
	unknown := confEvent(t, "cerberus", "alert.invented")
	good := confEvent(t, "cerberus", "alert.raised")
	for _, it := range []eventbus.Item{bad, unknown, good} {
		c.Publish(it)
	}
	waitFor(t, "good sent and two rejected", func() bool { s := c.Stats(); return s.Sent == 1 && s.Rejected == 2 })
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.rejected) != 2 {
		t.Fatalf("OnReject codes: %v", c.rejected)
	}
	seen := map[string]bool{}
	for _, code := range c.rejected {
		seen[code] = true
	}
	if !seen["forbidden_kind"] || !seen["unknown_kind"] {
		t.Fatalf("codes %v", c.rejected)
	}
	if _, ok, _ := hub.db.EventByEventID(context.Background(), good.ID); !ok {
		t.Fatal("good event behind two rejections was not stored")
	}
}

func TestClientAppMismatchIsFinalForTheBatchAndLaterEventsStillFlow(t *testing.T) {
	cerberus := &identity.Principal{Verified: true, ID: "msg://service/cerberus/dev", Kind: "service", Scopes: []string{"tether.events"}}
	hub := newConfHub(t, identity.Enforce, cerberus, eventingest.Config{}, true)
	c := newConfClient(t, hub.srv.URL, func(cfg *eventbus.Config) { cfg.BatchSize = 1 })
	c.Publish(confEvent(t, "torque", "task.state_changed")) // credential is bound to cerberus
	waitFor(t, "app_mismatch quarantined", func() bool { return c.Stats().Quarantined == 1 })
	c.mu.Lock()
	if len(c.quarantined) != 1 || c.quarantined[0] != "app_mismatch" {
		t.Fatalf("quarantine reasons: %v", c.quarantined)
	}
	c.mu.Unlock()
	good := confEvent(t, "cerberus", "alert.raised")
	c.Publish(good)
	waitFor(t, "later event sent", func() bool { return c.Stats().Sent == 1 })
	if _, ok, _ := hub.db.EventByEventID(context.Background(), good.ID); !ok {
		t.Fatal("event after app_mismatch not stored")
	}
}

func TestClientWaitsForAHubWhoseIngestIsNotEnabledYet(t *testing.T) {
	hub := newConfHub(t, identity.Off, nil, eventingest.Config{}, false) // POST /events answers 405
	c := newConfClient(t, hub.srv.URL, nil)
	it := confEvent(t, "cerberus", "alert.raised")
	c.Publish(it)
	waitFor(t, "retried at least twice", func() bool { return c.Stats().Retries >= 2 })
	if s := c.Stats(); s.Sent != 0 || s.Quarantined != 0 || s.Dropped != 0 {
		t.Fatalf("event must be kept while ingest is off: %+v", s)
	}
}

func TestClientHonoursRateLimitRetryAfter(t *testing.T) {
	hub := newConfHub(t, identity.Off, nil, eventingest.Config{RatePerSecond: 5, Burst: 2}, true)
	c := newConfClient(t, hub.srv.URL, func(cfg *eventbus.Config) { cfg.BatchSize = 1 })
	var items []eventbus.Item
	for i := 0; i < 4; i++ {
		items = append(items, confEvent(t, "cerberus", "alert.raised"))
		c.Publish(items[i])
	}
	waitFor(t, "all 4 delivered despite 429s", func() bool { return c.Stats().Sent == 4 })
	if c.Stats().Retries == 0 {
		t.Error("expected at least one 429 retry with burst 2")
	}
	for _, it := range items {
		if _, ok, _ := hub.db.EventByEventID(context.Background(), it.ID); !ok {
			t.Fatalf("%s lost", it.ID)
		}
	}
}

// Client.Deliver (mesh/eventbus v0.2.0) is for an outbox relay that keeps its own
// durable record and must not move past an event until the hub settled it. Against
// the real handler: a new event is Sent, a repeat is Sent+Duplicate, a rejected
// kind is Quarantined with the hub's code, a good event after a rejected one is
// still Sent, and an app_mismatch quarantines the item without blocking the next.
func TestClientDeliverSettlesEachItemAgainstTheRealHandler(t *testing.T) {
	cerberus := &identity.Principal{Verified: true, ID: "msg://service/cerberus/dev", Kind: "service", Scopes: []string{"tether.events"}}
	hub := newConfHub(t, identity.Enforce, cerberus, eventingest.Config{}, true)
	c := newConfClient(t, hub.srv.URL, func(cfg *eventbus.Config) { cfg.SpoolDir = t.TempDir(); cfg.BatchSize = 10 })
	good := confEvent(t, "cerberus", "alert.raised")
	rejected := confEvent(t, "cerberus", "task.state_changed") // torque's kind
	mismatch := confEvent(t, "torque", "task.state_changed")   // the credential is bound to cerberus
	after := confEvent(t, "cerberus", "alert.cleared")
	res := c.Deliver(context.Background(), []eventbus.Item{good, rejected, after})
	if len(res) != 3 {
		t.Fatalf("%d results", len(res))
	}
	if res[0].Disposition != eventbus.DispositionSent || res[0].Duplicate {
		t.Errorf("new event: %+v", res[0])
	}
	if res[1].Disposition != eventbus.DispositionQuarantined || res[1].Code != "forbidden_kind" {
		t.Errorf("rejected kind: %+v", res[1])
	}
	if res[2].Disposition != eventbus.DispositionSent {
		t.Errorf("event behind a rejected one: %+v", res[2])
	}
	for _, it := range []eventbus.Item{good, after} {
		if _, ok, _ := hub.db.EventByEventID(context.Background(), it.ID); !ok {
			t.Errorf("%s not stored", it.ID)
		}
	}
	if _, ok, _ := hub.db.EventByEventID(context.Background(), rejected.ID); ok {
		t.Error("a rejected event was stored")
	}
	// A repeat of an event the hub already has is settled as Sent + Duplicate.
	res = c.Deliver(context.Background(), []eventbus.Item{good})
	if len(res) != 1 || res[0].Disposition != eventbus.DispositionSent || !res[0].Duplicate {
		t.Errorf("repeat: %+v", res)
	}
	// app_mismatch (a 403 for the whole request) is final for the item, never retried forever.
	more := confEvent(t, "cerberus", "alert.raised")
	res = c.Deliver(context.Background(), []eventbus.Item{mismatch})
	if len(res) != 1 || res[0].Disposition != eventbus.DispositionQuarantined || res[0].Reason != "app_mismatch" {
		t.Errorf("app_mismatch: %+v", res)
	}
	if res = c.Deliver(context.Background(), []eventbus.Item{more}); len(res) != 1 || res[0].Disposition != eventbus.DispositionSent {
		t.Errorf("event after an app_mismatch: %+v", res)
	}
}
