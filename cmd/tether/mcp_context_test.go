package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
)

type subscribedForwardBus struct {
	events.Bus
	ready chan struct{}
}

func (b *subscribedForwardBus) Subscribe(ctx context.Context, f events.Filter) (<-chan events.Event, func(), error) {
	ch, cancel, err := b.Bus.Subscribe(ctx, f)
	close(b.ready)
	return ch, cancel, err
}

func TestProxyEventForwarderCarriesSessionCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_TOKEN", "session-token")
	t.Setenv("TETHER_MCP_TOKEN", "session-proxy")
	oldFile := tokenFilePath
	tokenFilePath = ""
	t.Cleanup(func() { tokenFilePath = oldFile })
	seen := make(chan api.ProxyEventIngestRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer session-token" {
			t.Error("forwarder did not carry its session credential")
		}
		var body api.ProxyEventIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen <- body
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bus := &subscribedForwardBus{Bus: events.NewBus(events.BusOptions{}), ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardProxyEventsToDaemon(ctx, bus, "tcp:"+strings.TrimPrefix(srv.URL, "http://"), srv.URL, 0)
	}()
	select {
	case <-bus.ready:
	case <-ctx.Done():
		t.Fatal("forwarder did not subscribe")
	}
	raw, _ := json.Marshal(events.ToolCallEvent{SessionID: "own", ClaimedSessionID: "forged", ToolName: "tool", Server: "upstream", Timestamp: time.Now()})
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "own", Kind: events.EventTypeToolCallEnd, PayloadJSON: string(raw)}); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-seen:
		if body.SessionID != "own" || body.ClaimedSessionID != "forged" {
			t.Fatalf("body=%+v", body)
		}
	case <-ctx.Done():
		t.Fatal("no forwarded record")
	}
	cancel()
	<-done
}
