package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpadapter"
)

// KindMCPUpstreamStateChanged is published on every MCP upstream state
// transition (CW-20261114-0182): an upstream going failed, reconnecting (with
// its next attempt) and connected again is visible without polling
// tether_gateway_status.
const KindMCPUpstreamStateChanged = "mcp.upstream_state_changed"

// upstreamStatePublisher returns a state handler that publishes off the
// pool's goroutines: the pool calls it with its catalog lock held on one path,
// and a slow event store must not stall the gateway. A full queue drops (and
// logs) the change; gateway status still shows the current state.
func upstreamStatePublisher(ctx context.Context, bus events.Publisher) func(mcpadapter.UpstreamStateChange) {
	queue := make(chan mcpadapter.UpstreamStateChange, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case change := <-queue:
				publishUpstreamState(ctx, bus, change)
			}
		}
	}()
	return func(change mcpadapter.UpstreamStateChange) {
		select {
		case queue <- change:
		default:
			log.Printf("mcp upstream %s: %s -> %s not published (queue full)", change.ServerID, change.From, change.To)
		}
	}
}

func publishUpstreamState(ctx context.Context, bus events.Publisher, change mcpadapter.UpstreamStateChange) {
	if bus == nil {
		return
	}
	raw, _ := json.Marshal(change)
	e := events.Event{Scope: events.ScopeDaemon, Kind: KindMCPUpstreamStateChanged, At: time.Now().UTC(), PayloadJSON: string(raw)}
	e.Envelope.Class.Category = "state"
	if change.To == "failed" || change.To == "recovering" || change.To == "reconnecting" {
		e.Envelope.Class.Severity = "warning"
	}
	if err := bus.Publish(context.WithoutCancel(ctx), e); err != nil {
		log.Printf("mcp upstream %s: publish %s: %v", change.ServerID, KindMCPUpstreamStateChanged, err)
	}
}
