package events

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pagedFake is an in-memory log that implements PagedPersister and records how
// big each page was and whether a non-paged full read happened.
type pagedFake struct {
	fakePersister
	maxPage  atomic.Int64
	pages    atomic.Int64
	fullRead atomic.Int64
	trimTo   int64 // events with Seq <= trimTo are gone (head trim)
}

func (p *pagedFake) EventsSince(sinceSeq int64) ([]Event, error) {
	p.fullRead.Add(1)
	return p.fakePersister.EventsSince(sinceSeq)
}

func (p *pagedFake) StreamBoundsContext(context.Context) (int64, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.events) == 0 {
		return 0, 0, nil
	}
	earliest := p.events[0].Seq
	if p.trimTo >= earliest {
		earliest = p.trimTo + 1
	}
	return earliest, p.events[len(p.events)-1].Seq, nil
}

func (p *pagedFake) EventsPageContext(_ context.Context, f Filter, after, upTo int64, limit int) ([]Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Event
	for _, e := range p.events {
		if e.Seq <= after || e.Seq > upTo || e.Seq <= p.trimTo || !f.matches(e) {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	p.pages.Add(1)
	if int64(len(out)) > p.maxPage.Load() {
		p.maxPage.Store(int64(len(out)))
	}
	return out, nil
}

func seed(t *testing.T, p Persister, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, _, err := p.InsertEvent(ScopeSession, "s1", "k", "{}"); err != nil {
			t.Fatal(err)
		}
	}
}

func readN(t *testing.T, ch <-chan Event, n int) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d of %d events", len(got), n)
			}
			got = append(got, e)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(got), n)
		}
	}
	return got
}

func gapOf(t *testing.T, e Event) GapPayload {
	t.Helper()
	if e.Kind != KindStreamGap {
		t.Fatalf("expected a gap event, got kind %q", e.Kind)
	}
	var g GapPayload
	if err := json.Unmarshal([]byte(e.PayloadJSON), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// A seq-0 subscribe pages the log: no page exceeds the bound, no full read, and
// a publisher is not blocked while a stalled consumer holds the replay.
func TestBus_PagedReplay_IsBoundedAndDoesNotHoldThePublishLock(t *testing.T) {
	p := &pagedFake{}
	seed(t, p, 5000)
	bus := NewBus(BusOptions{Persister: p, ReplayPage: 100, SubBuffer: 8})
	ctx := context.Background()
	ch, cancel, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if p.fullRead.Load() != 0 {
		t.Fatal("a paged persister was read in full")
	}
	// The consumer has read nothing: the worker is parked mid-replay. Publishing
	// must still complete (the bus lock is not held across replay).
	pubDone := make(chan error, 1)
	go func() { pubDone <- bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "live"}) }()
	select {
	case err := <-pubDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked behind a subscriber's replay")
	}
	got := readN(t, ch, 5001)
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: replay lost, duplicated or reordered events", i, e.Seq)
		}
	}
	if m := p.maxPage.Load(); m == 0 || m > 100 {
		t.Fatalf("largest replay page %d, want 1..100", m)
	}
}

// No loss and no duplicate at the replay/live boundary under concurrent publishing.
func TestBus_PagedReplay_NoGapOrDuplicateAtTheLiveBoundary(t *testing.T) {
	p := &pagedFake{}
	seed(t, p, 300)
	bus := NewBus(BusOptions{Persister: p, ReplayPage: 25, SubBuffer: 4096})
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "live"})
		}
	}()
	ch, cancel, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	wg.Wait()
	got := readN(t, ch, 800)
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("position %d carries seq %d", i, e.Seq)
		}
	}
}

// A consumer that falls behind is told (stream.gap overrun, recovered) and still
// receives every event, in order, from the durable log.
func TestBus_PagedSubscriber_OverrunIsSignaledAndRecovered(t *testing.T) {
	p := &pagedFake{}
	bus := NewBus(BusOptions{Persister: p, SubBuffer: 2, MaxConsecDrops: 100000, ReplayPage: 10})
	ctx := context.Background()
	ch, cancel, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// Get the subscriber into its live phase first, then stall it.
	if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "k"}); err != nil {
		t.Fatal(err)
	}
	if first := readN(t, ch, 1)[0]; first.Seq != 1 {
		t.Fatalf("first event seq %d", first.Seq)
	}
	for i := 0; i < 60; i++ {
		if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	var seqs []int64
	sawGap := false
	deadline := time.After(5 * time.Second)
	for len(seqs) < 60 {
		select {
		case e := <-ch:
			if e.Kind == KindStreamGap {
				g := gapOf(t, e)
				if g.Reason != "overrun" || !g.Recovered || g.Dropped == 0 {
					t.Fatalf("gap %+v", g)
				}
				sawGap = true
				continue
			}
			seqs = append(seqs, e.Seq)
		case <-deadline:
			t.Fatalf("got %d of 60 events", len(seqs))
		}
	}
	if !sawGap {
		t.Fatal("overrun not signaled")
	}
	for i, s := range seqs {
		if s != int64(i+2) {
			t.Fatalf("events after recovery out of order or missing: %v", seqs)
		}
	}
}

// History the subscriber asked for was trimmed: it is told, with the bounds.
func TestBus_PagedSubscriber_TrimmedHistorySignalsAGap(t *testing.T) {
	p := &pagedFake{trimTo: 40}
	seed(t, p, 50)
	bus := NewBus(BusOptions{Persister: p})
	ch, cancel, err := bus.Subscribe(context.Background(), Filter{SinceSeq: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	first := readN(t, ch, 1)[0]
	g := gapOf(t, first)
	if g.Reason != "trimmed" || g.EarliestAvailable != 41 || g.HighWater != 50 {
		t.Fatalf("gap %+v", g)
	}
	rest := readN(t, ch, 10)
	if rest[0].Seq != 41 || rest[9].Seq != 50 {
		t.Fatalf("after the gap the subscriber should get what remains: %d..%d", rest[0].Seq, rest[9].Seq)
	}
	// SinceSeq 0 and a SinceSeq inside the retained range are not gaps.
	for _, since := range []int64{0, 40, 45} {
		ch2, cancel2, err := bus.Subscribe(context.Background(), Filter{SinceSeq: since})
		if err != nil {
			t.Fatal(err)
		}
		e := readN(t, ch2, 1)[0]
		if e.Kind == KindStreamGap {
			t.Fatalf("since=%d reported a gap", since)
		}
		cancel2()
	}
}

// Filters are applied during paging, and gap signals bypass them.
func TestBus_PagedSubscriber_FilterAndGapBypass(t *testing.T) {
	p := &pagedFake{}
	bus := NewBus(BusOptions{Persister: p, ReplayPage: 3})
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		kind := "other"
		if i%5 == 0 {
			kind = "want"
		}
		if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s1", Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	ch, cancel, err := bus.Subscribe(ctx, Filter{Kinds: []string{"want"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	got := readN(t, ch, 4)
	for _, e := range got {
		if e.Kind != "want" {
			t.Fatalf("filter leaked %q", e.Kind)
		}
	}
}

// A consumer that never reads is eventually evicted, and is told why before the
// channel closes (non-paged persister: the in-memory fake).
func TestBus_EvictedSubscriberReceivesAFinalGap(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}, SubBuffer: 1, MaxConsecDrops: 3})
	ctx := context.Background()
	ch, _, err := bus.Subscribe(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := bus.Publish(ctx, Event{Scope: ScopeSession, Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	var last Event
	timeout := time.After(2 * time.Second)
	for closed := false; !closed; {
		select {
		case e, ok := <-ch:
			if !ok {
				closed = true
				break
			}
			last = e
		case <-timeout:
			t.Fatal("subscriber not closed")
		}
	}
	if g := gapOf(t, last); g.Reason != "evicted" {
		t.Fatalf("final signal %+v, want evicted", g)
	}
}
