package eventstamp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
)

type fakeSource struct {
	mu    sync.Mutex
	rows  map[string]SessionRow
	calls map[string]int
	err   error
}

func (f *fakeSource) Session(_ context.Context, id string) (SessionRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[id]++
	if f.err != nil {
		return SessionRow{}, false, f.err
	}
	row, ok := f.rows[id]
	return row, ok, nil
}

var workerRow = SessionRow{WorkerID: "worker-1", ProjectID: "PRJ-9", WorkstreamID: "ws-9", TaskID: "CW-9", LaunchID: "launch-9", LogicalAgentID: "la-9"}

func rowsFor(src *fakeSource) (*Rows, *time.Time) {
	r := NewRows(src, time.Minute)
	clock := time.Unix(1_000, 0)
	r.now = func() time.Time { return clock }
	return r, &clock
}

func TestRowsStampsASessionsEventsFromItsRow(t *testing.T) {
	src := &fakeSource{rows: map[string]SessionRow{"s9": workerRow}}
	r, _ := rowsFor(src)
	e := events.Event{Scope: events.ScopeSession, SessionID: "s9", Kind: "session.state_changed"}
	if err := r.Stamp(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
	want := events.ScopeDims{WorkerID: "worker-1", ProjectID: "PRJ-9", WorkstreamID: "ws-9", TaskID: "CW-9", LaunchID: "launch-9", LogicalAgentID: "la-9"}
	if e.Envelope.Dims != want {
		t.Fatalf("dims %+v, want %+v", e.Envelope.Dims, want)
	}
}

func TestRowsLeavesProducerValuesAndUnmanagedEventsAlone(t *testing.T) {
	src := &fakeSource{rows: map[string]SessionRow{"s9": workerRow, "hub": {ProjectID: "PRJ-9"}}}
	r, _ := rowsFor(src)
	e := events.Event{Scope: events.ScopeSession, SessionID: "s9"}
	e.Envelope.Dims.ProjectID = "producer"
	_ = r.Stamp(context.Background(), &e)
	if e.Envelope.Dims.ProjectID != "producer" || e.Envelope.Dims.WorkerID != "worker-1" {
		t.Fatalf("dims %+v", e.Envelope.Dims)
	}
	// A hub-local session has a blank worker: the dimension stays empty.
	e = events.Event{Scope: events.ScopeSession, SessionID: "hub"}
	_ = r.Stamp(context.Background(), &e)
	if e.Envelope.Dims.WorkerID != "" {
		t.Fatalf("worker invented for a hub-local session: %q", e.Envelope.Dims.WorkerID)
	}
	// No session, no lookup.
	before := len(src.calls)
	e = events.Event{Scope: events.ScopeDaemon, Kind: "daemon.started"}
	_ = r.Stamp(context.Background(), &e)
	if len(src.calls) != before || e.Envelope.Dims != (events.ScopeDims{}) {
		t.Fatal("a daemon event triggered a session lookup or got session dims")
	}
	// An unknown session stamps nothing and is not an error.
	e = events.Event{Scope: events.ScopeSession, SessionID: "ghost"}
	if err := r.Stamp(context.Background(), &e); err != nil || e.Envelope.Dims != (events.ScopeDims{}) {
		t.Fatalf("unknown session: %v %+v", err, e.Envelope.Dims)
	}
}

func TestRowsTrustsNoClaimedSession(t *testing.T) {
	src := &fakeSource{rows: map[string]SessionRow{"s9": workerRow}}
	r, _ := rowsFor(src)
	unresolved := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, PrincipalKind: "operator"})
	e := events.Event{Scope: events.ScopeSession, SessionID: "s9"}
	_ = r.Stamp(unresolved, &e)
	if e.Envelope.Dims != (events.ScopeDims{}) || src.calls["s9"] != 0 {
		t.Fatalf("stamped (or looked up) a session an unresolved caller only named: %+v", e.Envelope.Dims)
	}
	// A resolved caller, and a daemon-internal publish with no snapshot, both stamp.
	resolvedCtx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, SessionResolved: true, SessionID: "s1"})
	for name, ctx := range map[string]context.Context{"resolved caller": resolvedCtx, "daemon internal": context.Background()} {
		e = events.Event{Scope: events.ScopeSession, SessionID: "s9"}
		_ = r.Stamp(ctx, &e)
		if e.Envelope.Dims.WorkerID != "worker-1" {
			t.Fatalf("%s: not stamped: %+v", name, e.Envelope.Dims)
		}
	}
}

func TestRowsCachesAndExpires(t *testing.T) {
	src := &fakeSource{rows: map[string]SessionRow{"s9": workerRow}}
	r, clock := rowsFor(src)
	stamp := func() {
		e := events.Event{Scope: events.ScopeSession, SessionID: "s9"}
		if err := r.Stamp(context.Background(), &e); err != nil {
			t.Fatal(err)
		}
	}
	for range 50 {
		stamp()
	}
	if src.calls["s9"] != 1 {
		t.Fatalf("%d lookups for 50 events, want 1", src.calls["s9"])
	}
	*clock = clock.Add(2 * time.Minute)
	stamp()
	if src.calls["s9"] != 2 {
		t.Fatalf("%d lookups after the TTL, want 2", src.calls["s9"])
	}
}

func TestRowsErrorIsReportedNotCached(t *testing.T) {
	src := &fakeSource{err: errors.New("db busy")}
	r, _ := rowsFor(src)
	e := events.Event{Scope: events.ScopeSession, SessionID: "s9"}
	if err := r.Stamp(context.Background(), &e); err == nil {
		t.Fatal("lookup failure not reported")
	}
	src.err = nil
	src.rows = map[string]SessionRow{"s9": workerRow}
	if err := r.Stamp(context.Background(), &e); err != nil || e.Envelope.Dims.WorkerID != "worker-1" {
		t.Fatalf("a failure was cached: %v %+v", err, e.Envelope.Dims)
	}
}

func TestRowsIsNotTxSafe(t *testing.T) {
	var s events.Stage = NewRows(&fakeSource{}, 0)
	if safe, ok := s.(events.TxSafe); ok && safe.TxSafe() {
		t.Fatal("a stage that reads the database must not run inside a caller's transaction")
	}
}
