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
	ch       chan events.Event
	filter   events.Filter
	canceled bool
}

func (b *forwardBus) Subscribe(_ context.Context, f events.Filter) (<-chan events.Event, func(), error) {
	b.filter = f
	return b.ch, func() { b.canceled = true }, nil
}
func TestForwardFiltersTruncatesBoundsAndContinuesAfterFailure(t *testing.T) {
	bus := &forwardBus{ch: make(chan events.Event, 5)}
	bus.ch <- events.Event{Kind: events.EventTypeToolCallStart}
	bus.ch <- events.Event{Kind: events.EventTypeToolCallEnd, PayloadJSON: "not json"}
	stamp := time.Now().UTC()
	raw, _ := json.Marshal(events.ToolCallEvent{SessionID: "own", ClaimedSessionID: "other", ToolName: "tool", Server: "server", Timestamp: stamp, Error: strings.Repeat("€", 5000), DurationMs: 7})
	for range 2 {
		bus.ch <- events.Event{Kind: events.EventTypeToolCallEnd, PayloadJSON: string(raw)}
	}
	close(bus.ch)
	calls := 0
	Forward(context.Background(), bus, func(ctx context.Context, req ProxyEventIngestRequest) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second || time.Until(deadline) <= 0 {
			t.Fatalf("deadline=%v", deadline)
		}
		if req.SessionID != "own" || req.ClaimedSessionID != "other" || req.DurationMs != 7 || req.Timestamp != stamp.Format(time.RFC3339Nano) || len(req.Error) > MaxProxyEventErrorBytes || req.Publish {
			t.Fatalf("body=%+v", req)
		}
		return errors.New("offline")
	}, 42)
	if calls != 2 || !bus.canceled || bus.filter.SinceSeq != 42 {
		t.Fatalf("calls=%d canceled=%v filter=%+v", calls, bus.canceled, bus.filter)
	}
}
