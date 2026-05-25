package client

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
)

type streamTestBus struct {
	filter events.Filter
	subCh  chan events.Event
}

func (b *streamTestBus) Publish(context.Context, events.Event) error { return nil }

func (b *streamTestBus) Subscribe(_ context.Context, filter events.Filter) (<-chan events.Event, func(), error) {
	b.filter = filter
	if b.subCh == nil {
		b.subCh = make(chan events.Event, 4)
	}
	return b.subCh, func() {}, nil
}

func TestClientStreamEvents(t *testing.T) {
	bus := &streamTestBus{}
	srv := httptest.NewServer(api.NewHandler(api.Deps{Bus: bus}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := New("tcp:" + srv.URL[len("http://"):])
	ch, errCh, err := c.StreamEvents(ctx, EventsStreamQuery{Scopes: []string{events.ScopeDaemon}, Kinds: []string{events.KindAIBudgetRejected}, SinceSeq: 5})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if len(bus.filter.Scopes) != 1 || bus.filter.Scopes[0] != events.ScopeDaemon || len(bus.filter.Kinds) != 1 || bus.filter.Kinds[0] != events.KindAIBudgetRejected || bus.filter.SinceSeq != 5 {
		t.Fatalf("filter = %+v", bus.filter)
	}

	bus.subCh <- events.Event{
		Seq:         9,
		Scope:       events.ScopeDaemon,
		Kind:        events.KindAIBudgetRejected,
		PayloadJSON: `{"provider":"anthropic-work","error":"usage budget exceeded"}`,
	}

	select {
	case ev := <-ch:
		if ev.Seq != 9 || ev.Kind != events.KindAIBudgetRejected || ev.Scope != events.ScopeDaemon {
			t.Fatalf("event = %+v", ev)
		}
		if ev.PayloadJSON != `{"provider":"anthropic-work","error":"usage budget exceeded"}` {
			t.Fatalf("payload = %q", ev.PayloadJSON)
		}
	case err := <-errCh:
		t.Fatalf("stream err = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}
