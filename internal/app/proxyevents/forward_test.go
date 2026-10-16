package proxyevents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

type forwardBus struct {
	events.Bus
	ch          chan events.Event
	filters     []events.Filter
	canceled    bool
	onSubscribe func()
}

func (b *forwardBus) Subscribe(_ context.Context, f events.Filter) (<-chan events.Event, func(), error) {
	b.filters = append(b.filters, f)
	if b.onSubscribe != nil {
		b.onSubscribe()
	}
	return b.ch, func() { b.canceled = true }, nil
}
func TestForwardFiltersTruncatesBoundsAndContinuesAfterFailure(t *testing.T) {
	events.FollowBackoffFloor, events.FollowBackoffCeiling = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { events.FollowBackoffFloor, events.FollowBackoffCeiling = 200*time.Millisecond, 10*time.Second })
	bus := &forwardBus{ch: make(chan events.Event, 6)}
	bus.ch <- events.Event{Seq: 43, Kind: events.EventTypeToolCallStart}
	bus.ch <- events.Event{Seq: 44, Kind: events.EventTypeToolCallEnd, PayloadJSON: "not json"}
	// a gap frame is never forwarded, whatever it carries
	bus.ch <- events.Event{Seq: 44, Kind: events.KindStreamGap, PayloadJSON: `{"reason":"overrun","recovered":true}`}
	stamp := time.Now().UTC()
	raw, _ := json.Marshal(events.ToolCallEvent{SessionID: "own", ClaimedSessionID: "other", ToolName: "tool", Server: "server", Timestamp: stamp, Error: strings.Repeat("€", 5000), DurationMs: 7})
	for i := range 2 {
		bus.ch <- events.Event{Seq: int64(45 + i), Kind: events.EventTypeToolCallEnd, PayloadJSON: string(raw)}
	}
	close(bus.ch) // the bus evicted us
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	Forward(ctx, bus, func(ctx context.Context, req ProxyEventIngestRequest) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second || time.Until(deadline) <= 0 {
			t.Fatalf("deadline=%v", deadline)
		}
		if req.SessionID != "own" || req.ClaimedSessionID != "other" || req.DurationMs != 7 || req.Timestamp != stamp.Format(time.RFC3339Nano) || len(req.Error) > MaxProxyEventErrorBytes || req.Publish {
			t.Fatalf("body=%+v", req)
		}
		if calls == 2 {
			cancel() // both finished calls forwarded: stop following
		}
		return errors.New("offline")
	}, 42)
	if calls != 2 || !bus.canceled || bus.filters[0].SinceSeq != 42 {
		t.Fatalf("calls=%d canceled=%v filters=%+v", calls, bus.canceled, bus.filters)
	}
}

// After the bus closes the channel Forward re-subscribes from the last sequence
// it saw instead of ending.
func TestForwardResubscribesFromItsCursor(t *testing.T) {
	events.FollowBackoffFloor, events.FollowBackoffCeiling = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { events.FollowBackoffFloor, events.FollowBackoffCeiling = 200*time.Millisecond, 10*time.Second })
	bus := &forwardBus{ch: make(chan events.Event, 4)}
	raw, _ := json.Marshal(events.ToolCallEvent{ToolName: "tool", Server: "server", Timestamp: time.Now().UTC()})
	bus.ch <- events.Event{Seq: 10, Kind: events.EventTypeToolCallEnd, PayloadJSON: string(raw)}
	bus.ch <- events.Event{Seq: 11, Kind: events.EventTypeToolCallEnd, PayloadJSON: string(raw)}
	close(bus.ch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscribed := make(chan struct{}, 8)
	bus.onSubscribe = func() { subscribed <- struct{}{} }
	go func() {
		<-subscribed // first
		<-subscribed // re-subscribe
		cancel()
	}()
	Forward(ctx, bus, func(context.Context, ProxyEventIngestRequest) error { return nil }, 5)
	if len(bus.filters) < 2 || bus.filters[0].SinceSeq != 5 || bus.filters[1].SinceSeq != 11 {
		t.Fatalf("subscribe filters %+v, want since 5 then 11", bus.filters)
	}
}
