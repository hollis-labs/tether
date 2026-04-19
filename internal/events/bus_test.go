package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakePersister struct {
	mu     sync.Mutex
	seq    int64
	events []Event
}

func (f *fakePersister) InsertEvent(scope Scope, sessionID, kind, payloadJSON string) (int64, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	at := time.Now().UTC()
	f.events = append(f.events, Event{
		Seq:         f.seq,
		At:          at,
		Scope:       scope,
		SessionID:   sessionID,
		Kind:        kind,
		PayloadJSON: payloadJSON,
	})
	return f.seq, at, nil
}

func (f *fakePersister) EventsSince(sinceSeq int64) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, 0, len(f.events))
	for _, e := range f.events {
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestBus_PublishSubscribe_Roundtrip(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	ctx := context.Background()

	ch, cancel, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if err := bus.Publish(ctx, Event{
		Scope:       ScopeSession,
		SessionID:   "s1",
		Kind:        "session.started",
		PayloadJSON: `{"to":"running"}`,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if got.Kind != "session.started" {
			t.Errorf("Kind = %q, want session.started", got.Kind)
		}
		if got.SessionID != "s1" {
			t.Errorf("SessionID = %q, want s1", got.SessionID)
		}
		if got.Seq <= 0 {
			t.Errorf("Seq = %d, want >0", got.Seq)
		}
		if got.At.IsZero() {
			t.Error("At is zero")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestBus_Filter_ScopeAllowList(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	ctx := context.Background()

	ch, cancel, err := bus.Subscribe(ctx, Filter{Scopes: []Scope{ScopeDaemon}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// Session event must NOT arrive.
	if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "session.started"}); err != nil {
		t.Fatalf("Publish session: %v", err)
	}
	// Daemon event must arrive.
	if err := bus.Publish(ctx, Event{Scope: ScopeDaemon, Kind: "daemon.started"}); err != nil {
		t.Fatalf("Publish daemon: %v", err)
	}

	select {
	case got := <-ch:
		if got.Scope != ScopeDaemon {
			t.Errorf("Scope = %q, want daemon", got.Scope)
		}
		if got.Kind != "daemon.started" {
			t.Errorf("Kind = %q, want daemon.started", got.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for daemon event")
	}

	// No second event — filter should have rejected the session one.
	select {
	case unexpected := <-ch:
		t.Fatalf("unexpected event delivered past filter: %+v", unexpected)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBus_Filter_SessionID(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	ctx := context.Background()

	ch, cancel, err := bus.Subscribe(ctx, Filter{SessionID: "target"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "other", Kind: "state"}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "target", Kind: "state"}); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-ch:
		if got.SessionID != "target" {
			t.Errorf("SessionID = %q, want target", got.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	select {
	case unexpected := <-ch:
		t.Fatalf("unexpected extra event: %+v", unexpected)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBus_Publish_RejectsEmptyScope(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	err := bus.Publish(context.Background(), Event{Kind: "no-scope"})
	if err != ErrScopeEmpty {
		t.Errorf("err = %v, want ErrScopeEmpty", err)
	}
}

func TestBus_Publish_RejectsNilPersister(t *testing.T) {
	bus := NewBus(BusOptions{}) // no persister
	err := bus.Publish(context.Background(), Event{Scope: ScopeSession, Kind: "k"})
	if err != ErrNoPersister {
		t.Errorf("err = %v, want ErrNoPersister", err)
	}
}

func TestBus_Cancel_ClosesOutChannel(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	ctx := context.Background()

	ch, cancel, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel yielded a value after cancel; want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}

	// Second cancel must be safe (idempotent).
	cancel()
}

func TestBus_ConcurrentPublishSubscribeCancel(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}, MaxConsecDrops: 1_000_000})
	ctx := context.Background()

	const pubs = 200
	const subs = 10

	var wg sync.WaitGroup
	// Subscribers come and go.
	for i := 0; i < subs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel, err := bus.Subscribe(ctx, Filter{})
			if err != nil {
				t.Errorf("Subscribe: %v", err)
				return
			}
			// Read a handful then bail.
			count := 0
			deadline := time.After(500 * time.Millisecond)
		drain:
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						break drain
					}
					count++
					if count >= 20 {
						break drain
					}
				case <-deadline:
					break drain
				}
			}
			cancel()
		}()
	}

	// Publishers.
	for i := 0; i < pubs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "k"})
		}(i)
	}

	wg.Wait()
}

func TestBus_SinceSeq_NoBoundaryGap(t *testing.T) {
	// Stress the boundary: publishers racing the Subscribe replay must
	// not drop or duplicate events. Every seq published after the
	// fakePersister is seeded must appear exactly once, in order.
	p := &fakePersister{}
	// Seed 5 historical events.
	for i := 0; i < 5; i++ {
		if _, _, err := p.InsertEvent(ScopeSession, "s1", "seed", ""); err != nil {
			t.Fatal(err)
		}
	}

	bus := NewBus(BusOptions{Persister: p, SubBuffer: 4096, MaxConsecDrops: 1_000_000})
	ctx := context.Background()

	// Fire concurrent publishers as we open the subscription.
	publishDone := make(chan struct{})
	const newPubs = 200
	go func() {
		defer close(publishDone)
		for i := 0; i < newPubs; i++ {
			if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "live"}); err != nil {
				t.Errorf("Publish %d: %v", i, err)
				return
			}
		}
	}()

	ch, cancel, err := bus.Subscribe(ctx, Filter{SinceSeq: 0}) // replay everything + live
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	<-publishDone

	// Collect until we see the last expected seq (5 seeds + newPubs).
	wantLast := int64(5 + newPubs)
	seen := map[int64]bool{}
	var gotLast bool
	timeout := time.After(5 * time.Second)
	for !gotLast {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed before seq %d; saw %d events", wantLast, len(seen))
			}
			if seen[ev.Seq] {
				t.Errorf("duplicate delivery of seq %d", ev.Seq)
			}
			seen[ev.Seq] = true
			if ev.Seq == wantLast {
				gotLast = true
			}
		case <-timeout:
			t.Fatalf("timeout waiting for seq %d; saw %d events", wantLast, len(seen))
		}
	}

	// Every seq in [1..wantLast] must be present — no gaps.
	for i := int64(1); i <= wantLast; i++ {
		if !seen[i] {
			t.Errorf("missing seq %d — replay/live boundary leaked", i)
		}
	}
}

func TestBus_SinceSeq_ReplaysHistoryThenLive(t *testing.T) {
	p := &fakePersister{}
	// Seed five historical events directly into the persister.
	for i := 0; i < 5; i++ {
		if _, _, err := p.InsertEvent(ScopeSession, "s1", "historical", ""); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	bus := NewBus(BusOptions{Persister: p})
	ctx := context.Background()

	ch, cancel, err := bus.Subscribe(ctx, Filter{SinceSeq: 2})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// Expect seqs 3, 4, 5 in order.
	for _, wantSeq := range []int64{3, 4, 5} {
		select {
		case got := <-ch:
			if got.Seq != wantSeq {
				t.Errorf("replay Seq = %d, want %d", got.Seq, wantSeq)
			}
			if got.Kind != "historical" {
				t.Errorf("replay Kind = %q, want historical", got.Kind)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for replay seq %d", wantSeq)
		}
	}

	// Now publish a new (live) event; must arrive as seq 6.
	if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "live"}); err != nil {
		t.Fatalf("Publish live: %v", err)
	}
	select {
	case got := <-ch:
		if got.Seq != 6 {
			t.Errorf("live Seq = %d, want 6", got.Seq)
		}
		if got.Kind != "live" {
			t.Errorf("live Kind = %q, want live", got.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for live event")
	}
}

func TestBus_SlowSubscriber_DoesNotBlockPublisher(t *testing.T) {
	bus := NewBus(BusOptions{
		Persister:      &fakePersister{},
		SubBuffer:      2,
		MaxConsecDrops: 10000, // large: test block-avoidance, not eviction
	})
	ctx := context.Background()

	_, cancel, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "k"}); err != nil {
				t.Errorf("Publish %d: %v", i, err)
				return
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on slow subscriber — non-blocking fan-out violated")
	}

	mb := bus.(*memBus)
	if drops := mb.subscriberDropCount(); drops == 0 {
		t.Errorf("drop count = 0, want >0 (subscriber never read)")
	}
}

func TestBus_SlowSubscriber_EvictedAfterMaxConsecDrops(t *testing.T) {
	bus := NewBus(BusOptions{
		Persister:      &fakePersister{},
		SubBuffer:      1,
		MaxConsecDrops: 3,
	})
	ctx := context.Background()

	ch, _, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for i := 0; i < 50; i++ {
		if err := bus.Publish(ctx, Event{Scope: ScopeSession, Kind: "k"}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed — eviction observed
			}
		case <-timeout:
			t.Fatal("subscriber channel not closed — eviction didn't fire")
		}
	}
}

func TestBus_TwoSubscribers_EachReceiveEvent(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	ctx := context.Background()

	chA, cancelA, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	defer cancelA()

	chB, cancelB, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	defer cancelB()

	if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "session.started"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for name, ch := range map[string]<-chan Event{"A": chA, "B": chB} {
		select {
		case got := <-ch:
			if got.Kind != "session.started" {
				t.Errorf("sub %s: Kind = %q, want session.started", name, got.Kind)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %s: timeout", name)
		}
	}
}
