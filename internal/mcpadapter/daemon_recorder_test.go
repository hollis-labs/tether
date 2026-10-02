package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

type blockedProxyWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w blockedProxyWriter) AppendProxyEvent(store.ProxyEvent) error {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return nil
}

func TestDaemonRecorderNeverBlocksToolOnSlowDatabase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := blockedProxyWriter{make(chan struct{}, 1), make(chan struct{})}
	recorder := NewDaemonToolCallRecorder(ctx, nil, writer)
	raw, _ := json.Marshal(events.ToolCallEvent{ToolName: "app_call"})
	event := events.Event{Kind: events.EventTypeToolCallEnd, PayloadJSON: string(raw)}
	if err := recorder.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("writer never started")
	}
	finished := make(chan struct{})
	go func() {
		for range 1000 {
			_ = recorder.Publish(ctx, event)
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		close(writer.release)
		t.Fatal("a database commit blocked tool publication")
	}
	dropped, _ := recorder.Stats()
	if dropped == 0 {
		t.Fatal("bounded queue did not count dropped records")
	}
	cancel()
	close(writer.release)
	select {
	case <-recorder.done:
	case <-time.After(time.Second):
		t.Fatal("recorder worker leaked after daemon cancellation")
	}
}
