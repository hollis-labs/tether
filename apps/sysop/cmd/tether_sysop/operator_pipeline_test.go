package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// Sysop's independent metadata outbox is not configured by the root daemon's
// event_stage_mode. An enforce-mode daemon drains these already-proven rows
// without reprocessing or downgrading them through the root registry.
func TestOperatorKindsPreserveIndependentSanitationThroughEnforceDrain(t *testing.T) {
	_, db := inboxFixture(t)
	if _, err := db.SeedEventIdentity(context.Background(), operatorTestEnv); err != nil {
		t.Fatal(err)
	}
	kinds := []string{kindSysopSessionStop, kindSysopSessionCheckpoint, kindSysopLogicalResume, kindSysopSessionCleanup, kindSysopLogicalAgentPolicy}
	for _, kind := range kinds {
		err := enqueueOperatorEvent(context.Background(), db, events.ScopeDaemon, "", kind, map[string]any{"summary": "SYNTHETIC_PRIVATE_BODY", "status": "SYNTHETIC_PRIVATE_BODY", "actor": "SYNTHETIC_PRIVATE_BODY", "summary_bytes": 4, "older_than_days": 2, "limit": 10, "deleted_count": 1, "logical_agent_id": "fixture-agent", "provider_id": "fixture-provider", "provider_kind": "cli", "checkpoint_policy": "on_stop"})
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := events.NewPipeline(events.Enforce)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: db, Pipeline: p}).(events.OutboxBus)
	live, stop, err := bus.(events.LiveSubscriber).SubscribeLive(context.Background(), events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	stopDrain := bus.StartOutbox(context.Background())
	defer stopDrain()
	seen := map[string]bool{}
	for range kinds {
		select {
		case e := <-live:
			if !e.Envelope.Class.Sanitized || e.Envelope.Actor.URN != "" || e.Envelope.App != "tether-sysop" || strings.Contains(e.PayloadJSON, "SYNTHETIC_PRIVATE_BODY") {
				t.Fatal("independent sanitation changed")
			}
			seen[e.Kind] = true
		case <-time.After(3 * time.Second):
			t.Fatal("missing independently sanitized Sysop event")
		}
	}
	for _, kind := range kinds {
		if !seen[kind] {
			t.Fatalf("missing Sysop inventory kind %s", kind)
		}
	}
	if len(p.Counts()) != 0 {
		t.Fatal("outbox drain reprocessed independent producer payloads")
	}
}
