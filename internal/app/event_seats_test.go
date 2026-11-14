package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestCatalogSeatsResolveCellThroughExtendsAndURNs(t *testing.T) {
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-base":   {ID: "photon-base", Cell: "photon"},
		"photon-tether": {ID: "photon-tether", Extends: "photon-base", URN: "msg://agent/agent-mux/agt_lpy0gym4eh"},
		"archie":        {ID: "archie", Cell: "command"},
		"loner":         {ID: "loner", Extends: "missing"},
		"loop-a":        {ID: "loop-a", Extends: "loop-b"},
		"loop-b":        {ID: "loop-b", Extends: "loop-a"},
	}}
	seats := &catalogSeats{}
	seats.follow(func() *config.Catalog { return cat })
	for id, want := range map[string]string{"photon-tether": "photon", "archie": "command", "loner": "", "loop-a": "", "nobody": ""} {
		if got := seats.Cell(id); got != want {
			t.Errorf("Cell(%s) = %q, want %q", id, got, want)
		}
	}
	for urn, want := range map[string]string{
		"msg://agent/agent-mux/agt_lpy0gym4eh": "photon-tether",
		"msg://agent/agent-mux/archie":         "archie",
		"msg://agent/agent-mux/unknown":        "",
	} {
		if got := seats.AgentForURN(urn); got != want {
			t.Errorf("AgentForURN(%s) = %q, want %q", urn, got, want)
		}
	}
	// A reload is a new catalog pointer: the index follows it.
	cat = &config.Catalog{Agents: map[string]config.Agent{"archie": {ID: "archie", Cell: "photon"}}}
	if got := seats.Cell("archie"); got != "photon" {
		t.Fatalf("after reload Cell(archie) = %q", got)
	}
}

func TestValidCell(t *testing.T) {
	for v, want := range map[string]bool{"": true, "photon": true, "cell-2_b": true, "Photon": false, "2cell": false, "a b": false, "-x": false} {
		if launchprofile.ValidCell(v) != want {
			t.Errorf("ValidCell(%q) = %v", v, !want)
		}
	}
}

type recordingPublisher struct {
	mu  sync.Mutex
	got []events.Event
}

func (r *recordingPublisher) Publish(_ context.Context, e events.Event) error {
	r.mu.Lock()
	r.got = append(r.got, e)
	r.mu.Unlock()
	return nil
}

func requestEvent(t *testing.T, session string, req store.EnvironmentRequest) events.Event {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return events.Event{Kind: store.EnvironmentRequestKind, SessionID: session, LogicalAgentID: "photon-tether", PayloadJSON: string(raw)}
}

func TestSeatWaitTrackerEmitsOnFirstOpenAndLastClose(t *testing.T) {
	tr := newSeatWaitTracker()
	pub := &recordingPublisher{}
	tr.SetPublisher(pub)
	ctx := context.Background()
	steps := []events.Event{
		requestEvent(t, "s1", store.EnvironmentRequest{TurnID: "t1", RequestID: "r1", Kind: "approval", Open: true}),
		requestEvent(t, "s1", store.EnvironmentRequest{TurnID: "t1", RequestID: "r1", Kind: "approval", Open: true}), // repeat
		requestEvent(t, "s1", store.EnvironmentRequest{TurnID: "t1", RequestID: "r2", Kind: "question", Open: true}),
		requestEvent(t, "s1", store.EnvironmentRequest{TurnID: "t1", RequestID: "r1"}),
		requestEvent(t, "s1", store.EnvironmentRequest{TurnID: "t1", RequestID: "r2"}),
		requestEvent(t, "s1", store.EnvironmentRequest{TurnID: "t1", RequestID: "r9"}), // never seen open
		requestEvent(t, "s1", store.EnvironmentRequest{TurnID: "t2", RequestID: "r3", Kind: "approval", Open: true}),
		requestEvent(t, "s1", store.EnvironmentRequest{TurnID: "t2", Terminal: true, SourceSequence: 1}),
		requestEvent(t, "s2", store.EnvironmentRequest{TurnID: "t3", RequestID: "r4", Kind: "question", Open: true}),
		{Kind: events.KindSessionStateChanged, SessionID: "s2", PayloadJSON: `{"from":"running","to":"killed"}`},
	}
	for _, e := range steps {
		tr.Observe(ctx, e)
	}
	want := []struct{ kind, session, payload string }{
		{KindSeatWaiting, "s1", `{"waiting_for":"approval","request_id":"r1"}`},
		{KindSeatUnblocked, "s1", `{"waiting_for":"question","request_id":"r2","resolution":"resolved"}`},
		{KindSeatWaiting, "s1", `{"waiting_for":"approval","request_id":"r3"}`},
		{KindSeatUnblocked, "s1", `{"waiting_for":"approval","request_id":"r3","resolution":"turn_ended"}`},
		{KindSeatWaiting, "s2", `{"waiting_for":"question","request_id":"r4"}`},
		{KindSeatUnblocked, "s2", `{"waiting_for":"question","request_id":"r4","resolution":"session_ended"}`},
	}
	if len(pub.got) != len(want) {
		t.Fatalf("published %d events, want %d: %+v", len(pub.got), len(want), pub.got)
	}
	for i, w := range want {
		g := pub.got[i]
		if g.Kind != w.kind || g.SessionID != w.session || g.PayloadJSON != w.payload || g.LogicalAgentID == "" && w.session == "s1" {
			t.Errorf("event %d = %s %s %s (agent %q), want %s %s %s", i, g.Kind, g.SessionID, g.PayloadJSON, g.LogicalAgentID, w.kind, w.session, w.payload)
		}
	}
}

func TestWorkerCompactBoundaryPublishesSessionCompacted(t *testing.T) {
	s := permissionWorkerHarness(t)
	line := []byte(`{"type":"system","subtype":"compact_boundary","session_id":"p1","compact_metadata":{"trigger":"auto","pre_tokens":150000}}` + "\n")
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: line}); err != nil {
		t.Fatal(err)
	}
	var payload string
	err := s.Store.DB().QueryRow(`SELECT payload_json FROM events WHERE kind=? AND session_id='s1'`, KindSessionCompacted).Scan(&payload)
	if err != nil {
		t.Fatalf("no session.compacted event: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(payload), &got); err != nil || got["source"] != "compact" || got["trigger"] != "auto" || got["pre_tokens"] != float64(150000) {
		t.Fatalf("payload = %s", payload)
	}
}

func TestAdmitEventRetentionClassesMakesSessionOutputDebug(t *testing.T) {
	s := permissionWorkerHarness(t)
	if err := s.AdmitEventRetentionClasses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: []byte("raw bytes\n")}); err != nil {
		t.Fatal(err)
	}
	var class string
	if err := s.Store.DB().QueryRow(`SELECT COALESCE(retention_class,'') FROM events WHERE kind='session.output' AND session_id='s1'`).Scan(&class); err != nil {
		t.Fatal(err)
	}
	if class != events.RetentionDebug {
		t.Fatalf("session.output class = %q, want R-debug", class)
	}
}
