package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/registry"
)

func TestMessageEventsRealWakeOutboxAndJoinedShutdown(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "worker"}
	bound, err := reg.LeaseBinding(ctx, to.URN(), "session-1", "local", "session-1", nil, registry.VisibilityPrivateLocal, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime()
	rt.setAlive("session-1", true, agentsessions.LiveStateIdle)
	env := sendMessage(t, st, to)
	bus := events.NewBus(events.BusOptions{Persister: st})
	svc := &Service{Store: st, Bus: bus}
	svc.StartEventsOutbox(ctx)
	svc.StartMessageEvents(ctx)
	svc.StartMessageEvents(ctx)
	defer svc.stopEventsOutbox()
	defer svc.stopMessageEvents()
	ch, unsubscribe, err := bus.Subscribe(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	result := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "session-1", "WAKE-CANARY")
	if !result.Delivered {
		t.Fatalf("real wake failed: %+v", result)
	}
	wake := map[string]events.Event{}
	deadline := time.After(5 * time.Second)
	for len(wake) < 2 {
		select {
		case ev := <-ch:
			if !strings.HasPrefix(ev.Kind, "wake.") {
				continue
			}
			if _, exists := wake[ev.Kind]; exists {
				t.Fatal("duplicate wake phase")
			}
			wake[ev.Kind] = ev
			if ev.EventID == "" || ev.Seq == 0 || strings.Contains(ev.PayloadJSON, "WAKE-CANARY") {
				t.Fatal("missing durable identity or leaked wake text")
			}
		case <-deadline:
			t.Fatal("wake did not reach live outbox stream")
		}
	}
	var start, end map[string]any
	if err = json.Unmarshal([]byte(wake["wake.attempted"].PayloadJSON), &start); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(wake["wake.outcome"].PayloadJSON), &end); err != nil {
		t.Fatal(err)
	}
	if start["message_id"] != env.ID || end["message_id"] != env.ID || start["target"] != to.URN() || start["attempt_id"] != end["attempt_id"] || start["attempt"] != float64(1) || end["binding_generation"] != float64(bound.Generation) || end["delivered"] != true || end["attempted"] != true {
		t.Fatalf("wake metadata start=%v end=%v", start, end)
	}
	if wake["wake.outcome"].Envelope.Corr.CorrelationID != env.ID {
		t.Fatal("lost message correlation")
	}
	var stopped sync.WaitGroup
	for range 3 {
		stopped.Add(1)
		go func() { defer stopped.Done(); svc.stopMessageEvents() }()
	}
	stopped.Wait()
	// A joined shutdown leaves no drainer querying a store after its owner exits.
	svc.messageEventsMu.Lock()
	stop := svc.messageEventsStop
	svc.messageEventsMu.Unlock()
	if stop != nil {
		t.Fatal("worker still installed")
	}
}

func TestMessageEventsWakeFailureAndNoWakePrivacy(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("session-1", true, agentsessions.LiveStateProcessing)
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	out := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "session-1", "WAKE-CANARY")
	if out.Delivered || out.Reason != "busy" {
		t.Fatalf("want busy outcome: %+v", out)
	}
	bus := events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
	if _, err := st.DrainMessageEvents(ctx, bus); err != nil {
		t.Fatal(err)
	}
	rows, err := st.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, ev := range rows {
		if ev.Kind != "wake.outcome" {
			continue
		}
		var payload map[string]any
		if err = json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["reason"] != "busy" || payload["delivered"] != false {
			t.Fatal(payload)
		}
		if strings.Contains(ev.PayloadJSON, "WAKE-CANARY") {
			t.Fatal("wake text retained")
		}
		seen = true
	}
	if !seen {
		t.Fatal("busy outcome missing")
	}
}

func TestMessageEventsWakeAttemptsMeanRuntimeSubmission(t *testing.T) {
	for _, scenario := range []string{"busy", "offline", "handled", "marker-failure", "binding-changed", "untracked"} {
		t.Run(scenario, func(t *testing.T) {
			st, reg := newWakeHarness(t)
			ctx := context.Background()
			rt := newFakeRuntime()
			rt.setAlive("session-1", true, agentsessions.LiveStateIdle)
			to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
			env := sendMessage(t, st, to)
			switch scenario {
			case "busy":
				rt.setAlive("session-1", true, agentsessions.LiveStateProcessing)
			case "offline":
				rt.setAlive("session-1", false, agentsessions.LiveStateStopped)
			case "handled":
				if err := st.MessagingStore().MarkRead(ctx, env.ID, to); err != nil {
					t.Fatal(err)
				}
			case "marker-failure":
				if _, err := st.DB().Exec(`CREATE TRIGGER fail_marker BEFORE UPDATE OF pending_receipt_attempt_id ON messages BEGIN SELECT RAISE(ABORT,'marker fault'); END`); err != nil {
					t.Fatal(err)
				}
			case "binding-changed":
				if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "other-session", "local", "other-session", nil, "", 0); err != nil {
					t.Fatal(err)
				}
			case "untracked":
				if _, err := st.DB().Exec(`UPDATE messages SET delivery_id=NULL WHERE id=?`, env.ID); err != nil {
					t.Fatal(err)
				}
			}
			_ = attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "session-1", "WAKE-CANARY")
			bus := events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
			if _, err := st.DrainMessageEvents(ctx, bus); err != nil {
				t.Fatal(err)
			}
			rows, err := st.EventsSince(0)
			if err != nil {
				t.Fatal(err)
			}
			submissions, outcomes := 0, 0
			for _, ev := range rows {
				if ev.Kind == "wake.attempted" {
					submissions++
				}
				if ev.Kind != "wake.outcome" {
					continue
				}
				outcomes++
				var payload map[string]any
				if err = json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
					t.Fatal(err)
				}
				if payload["attempted"] != (scenario == "untracked") || payload["delivered"] != (scenario == "untracked") {
					t.Fatalf("false submission assertion for %s: %v", scenario, payload)
				}
			}
			wantSubmissions := 0
			if scenario == "untracked" {
				wantSubmissions = 1
			}
			if submissions != wantSubmissions || outcomes != 1 || rt.sendCallCount("session-1") != wantSubmissions {
				t.Fatalf("%s submissions=%d outcomes=%d runtime=%d", scenario, submissions, outcomes, rt.sendCallCount("session-1"))
			}
		})
	}
}
