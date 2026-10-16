package sessionhealth

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

type rig struct {
	t   *testing.T
	db  *store.Store
	bus events.Bus
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tr := New()
	bus := events.NewBus(events.BusOptions{Persister: db, Observers: []events.Observer{tr}})
	tr.SetPublisher(bus)
	return &rig{t, db, bus}
}

func (r *rig) emit(session, kind, payload string) {
	r.t.Helper()
	if err := r.bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: session, Kind: kind, PayloadJSON: payload}); err != nil {
		r.t.Fatal(err)
	}
}
func (r *rig) state(session, to string) {
	r.emit(session, events.KindSessionStateChanged, `{"from":"x","to":"`+to+`"}`)
}
func (r *rig) stalled(session string) {
	r.emit(session, events.KindSessionStalled, `{"reason":"repeated_tool_denials","denied":3}`)
}
func (r *rig) lost(session string) {
	r.emit(session, events.KindProviderSessionLost, `{"requested":"a","actual":"b","reason":"r"}`)
}
func (r *rig) turn(session, kind string) {
	r.emit(session, events.KindSessionTurnOutput, `{"kind":"`+kind+`"}`)
}
func (r *rig) shim(session, state, reason string) {
	r.emit(session, "session.shim_status", `{"state":"`+state+`","reason":"`+reason+`"}`)
}

// transitions returns the session's health events in order as "from>to[reasons]".
func (r *rig) transitions(session string) []string {
	r.t.Helper()
	rows, err := r.db.DB().Query(`SELECT payload_json, payload_schema, category FROM events WHERE kind=? AND session_id=? ORDER BY id`, events.KindSessionHealthChanged, session)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var raw, schema, category string
		if err = rows.Scan(&raw, &schema, &category); err != nil {
			r.t.Fatal(err)
		}
		if schema != "session_health_changed@1" || category != "state" {
			r.t.Fatalf("class block: schema=%q category=%q", schema, category)
		}
		var p Payload
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			r.t.Fatal(err)
		}
		if p.From == p.To || p.Since.IsZero() || time.Since(p.Since) > time.Minute {
			r.t.Fatalf("payload %s", raw)
		}
		s := p.From + ">" + p.To + "["
		for i, reason := range p.Reasons {
			if i > 0 {
				s += ","
			}
			s += reason
		}
		out = append(out, s+"]")
	}
	return out
}

func same(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestStartDegradeRecoverAndEndEmitOnlyOnChange(t *testing.T) {
	r := newRig(t)
	r.state("s1", "running")
	r.state("s1", "running") // repeat: no change
	r.stalled("s1")
	r.stalled("s1")            // already degraded: no change
	r.lost("s1")               // already degraded: no change
	r.turn("s1", "failure")    // not a recovery
	r.turn("s1", "final")      // degraded -> healthy
	r.turn("s1", "final")      // healthy: no change
	r.lost("s1")               // healthy -> degraded again
	r.state("s1", "completed") // clean end: no health change
	same(t, r.transitions("s1"), []string{
		"unknown>healthy[session_running]",
		"healthy>degraded[repeated_tool_denials]",
		"degraded>healthy[turn_output_resumed]",
		"healthy>degraded[provider_session_lost]",
	})
}

func TestUnhealthyIsTerminalAndFromAnyState(t *testing.T) {
	r := newRig(t)
	r.state("a", "running")
	r.shim("a", "detached", "outcome_unknown")
	r.turn("a", "final")
	r.stalled("a")
	r.state("a", "failed")
	same(t, r.transitions("a"), []string{
		"unknown>healthy[session_running]",
		"healthy>unhealthy[shim_outcome_unknown]",
	})
	r.state("b", "running")
	r.stalled("b")
	r.state("b", "orphaned")
	same(t, r.transitions("b"), []string{
		"unknown>healthy[session_running]",
		"healthy>degraded[repeated_tool_denials]",
		"degraded>unhealthy[runtime_orphaned]",
	})
}

// The reason names what happened: a failed end and an orphaned end are
// different causes.
func TestFailedAndOrphanedEndsHaveTheirOwnReasons(t *testing.T) {
	r := newRig(t)
	r.state("f", "running")
	r.state("f", "failed")
	same(t, r.transitions("f"), []string{"unknown>healthy[session_running]", "healthy>unhealthy[runtime_failed]"})
	r.state("o", "running")
	r.state("o", "orphaned")
	same(t, r.transitions("o"), []string{"unknown>healthy[session_running]", "healthy>unhealthy[runtime_orphaned]"})
}

func TestOtherShimStatusAndKilledEndsChangeNothing(t *testing.T) {
	r := newRig(t)
	r.state("c", "running")
	r.shim("c", "detached", "output_pending")
	r.shim("c", "attached", "outcome_unknown")
	r.state("c", "killed")
	same(t, r.transitions("c"), []string{"unknown>healthy[session_running]"})
}

// A tracker that restarted under a running session did not see it start: its
// first transition starts from unknown and says so.
func TestFirstTransitionAfterRestartIsMarked(t *testing.T) {
	r := newRig(t)
	r.stalled("old") // no session_running was ever seen by this tracker
	r.turn("old", "final")
	same(t, r.transitions("old"), []string{
		"unknown>degraded[repeated_tool_denials,tracker_restart]",
		"degraded>healthy[turn_output_resumed]",
	})
	r.state("fresh", "running") // a session that starts under this tracker is not marked
	same(t, r.transitions("fresh"), []string{"unknown>healthy[session_running]"})
}

// After a restart the tracker knows nothing about a session it did not see
// start: whatever signal comes first, the transition's "from" is unknown (never
// a guessed prior state) and carries tracker_restart.
func TestAfterARestartTheFirstTransitionAlwaysStartsFromUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		fire func(r *rig, id string)
		want string
	}{
		"stalled":       {func(r *rig, id string) { r.stalled(id) }, "unknown>degraded[repeated_tool_denials,tracker_restart]"},
		"provider lost": {func(r *rig, id string) { r.lost(id) }, "unknown>degraded[provider_session_lost,tracker_restart]"},
		"failed end":    {func(r *rig, id string) { r.state(id, "failed") }, "unknown>unhealthy[runtime_failed,tracker_restart]"},
		"orphaned end":  {func(r *rig, id string) { r.state(id, "orphaned") }, "unknown>unhealthy[runtime_orphaned,tracker_restart]"},
		"shim outcome":  {func(r *rig, id string) { r.shim(id, "detached", "outcome_unknown") }, "unknown>unhealthy[shim_outcome_unknown,tracker_restart]"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			tc.fire(r, "pre-restart-session")
			same(t, r.transitions("pre-restart-session"), []string{tc.want})
		})
	}
	// A turn output with nothing known to be wrong is not a transition at all.
	r := newRig(t)
	r.turn("quiet", "final")
	same(t, r.transitions("quiet"), nil)
}

func TestEventsWithoutASessionAndOurOwnEventsAreIgnored(t *testing.T) {
	r := newRig(t)
	r.emit("", events.KindSessionStalled, `{}`)
	r.state("d", "running")
	r.emit("d", events.KindSessionStateChanged, `not json`)
	r.emit("d", events.KindSessionTurnOutput, `not json`)
	same(t, r.transitions("d"), []string{"unknown>healthy[session_running]"})
	var n int
	if err := r.db.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE kind=?`, events.KindSessionHealthChanged).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d health events (%v), want 1: our own emission must not loop", n, err)
	}
}

func TestSessionsAreTrackedIndependentlyAndForgottenAtTheEnd(t *testing.T) {
	r := newRig(t)
	r.state("x", "running")
	r.state("y", "running")
	r.stalled("x")
	same(t, r.transitions("y"), []string{"unknown>healthy[session_running]"})
	r.state("x", "completed")
	// x was forgotten at its end: a later signal starts from unknown again.
	r.stalled("x")
	got := r.transitions("x")
	if got[len(got)-1] != "unknown>degraded[repeated_tool_denials,tracker_restart]" {
		t.Fatalf("after the end, state was kept: %v", got)
	}
}

func TestTransitionTableHasNoSelfTransitionAndOnlyClosedReasons(t *testing.T) {
	for _, from := range []string{Unknown, Healthy, Degraded, Unhealthy} {
		for sig := sigRunning; sig <= sigShimOutcomeUnknown; sig++ {
			to, reason := next(from, sig)
			if to == "" {
				continue
			}
			if to == from {
				t.Errorf("%s with signal %d is a self-transition (the schema rejects from==to)", from, sig)
			}
			if from == Unhealthy {
				t.Errorf("a transition leaves unhealthy: %s", to)
			}
			for _, c := range reason {
				if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
					t.Errorf("reason %q is not a closed snake_case code", reason)
				}
			}
		}
	}
}

// Concurrent signals for one session are applied and published in one order,
// so the from/to chain read back by sequence is contiguous.
func TestConcurrentSignalsKeepAContiguousChainBySequence(t *testing.T) {
	r := newRig(t)
	r.state("c", "running")
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				r.stalled("c")
			} else {
				r.turn("c", "final")
			}
		}()
	}
	wg.Wait()
	rows, err := r.db.DB().Query(`SELECT payload_json FROM events WHERE kind=? AND session_id='c' ORDER BY id`, events.KindSessionHealthChanged)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	prev := ""
	n := 0
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var p Payload
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if prev != "" && p.From != prev {
			t.Fatalf("transition %d starts from %q but the previous one ended at %q", n, p.From, prev)
		}
		prev = p.To
		n++
	}
	if n < 2 {
		t.Fatalf("only %d transitions; the test did not exercise ordering", n)
	}
}

// slowFirst delays the second publish so a later signal, applied right after,
// would overtake it if emissions were not ordered.
type slowFirst struct {
	events.Publisher
	mu sync.Mutex
	n  int
}

func (s *slowFirst) Publish(ctx context.Context, e events.Event) error {
	s.mu.Lock()
	s.n++
	first := s.n == 2
	s.mu.Unlock()
	if first {
		time.Sleep(150 * time.Millisecond)
	}
	return s.Publisher.Publish(ctx, e)
}

func TestASlowPublishCannotBeOvertakenBySeq(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tr := New()
	bus := events.NewBus(events.BusOptions{Persister: db, Observers: []events.Observer{tr}})
	tr.SetPublisher(&slowFirst{Publisher: bus})
	pub := func(kind, payload string) {
		_ = bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: "o", Kind: kind, PayloadJSON: payload})
	}
	pub(events.KindSessionStateChanged, `{"to":"running"}`) // healthy (slow publish)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pub(events.KindSessionStalled, `{}`) }()
	time.Sleep(30 * time.Millisecond)
	go func() { defer wg.Done(); pub(events.KindSessionTurnOutput, `{"kind":"final"}`) }()
	wg.Wait()
	rows, err := db.DB().Query(`SELECT payload_json FROM events WHERE kind=? ORDER BY id`, events.KindSessionHealthChanged)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	prev := ""
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var p Payload
		_ = json.Unmarshal([]byte(raw), &p)
		if prev != "" && p.From != prev {
			t.Fatalf("by seq the chain breaks: %q then from %q", prev, p.From)
		}
		prev = p.To
	}
	if prev != "healthy" {
		t.Fatalf("final state %q, want healthy after stall then recovery", prev)
	}
}
