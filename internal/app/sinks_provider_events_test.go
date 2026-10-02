package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	gopevents "github.com/hollis-labs/go-providers/provider/events"

	"github.com/hollis-labs/tether/internal/events"
)

type capturePublisher struct{ got []events.Event }

func (c *capturePublisher) Publish(_ context.Context, e events.Event) error {
	c.got = append(c.got, e)
	return nil
}

func TestProviderSessionLostCallbackPublishes(t *testing.T) {
	pub := &capturePublisher{}
	makeProviderSessionLostCallback(pub, "s1", "agent-1")("conv-old", "conv-new", "not found")
	if len(pub.got) != 1 {
		t.Fatalf("events = %+v", pub.got)
	}
	e := pub.got[0]
	if e.Kind != events.KindProviderSessionLost || e.Scope != events.ScopeSession || e.SessionID != "s1" || e.LogicalAgentID != "agent-1" {
		t.Errorf("event = %+v", e)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(e.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["requested"] != "conv-old" || payload["actual"] != "conv-new" || payload["reason"] != "not found" {
		t.Errorf("payload = %v", payload)
	}
}

func TestProviderTypedEventCallbackPublishesOnlyDenials(t *testing.T) {
	pub := &capturePublisher{}
	cb := makeProviderTypedEventCallback(pub, "s1", "agent-1")
	cb(gopevents.Delta{Text: "hi"})
	cb(gopevents.PermissionDenied{Action: "command", DisplayName: "RunCommand"})
	cb(gopevents.Done{})
	if len(pub.got) != 1 || pub.got[0].Kind != events.KindProviderPermissionDenied {
		t.Fatalf("events = %+v", pub.got)
	}
	if pub.got[0].PayloadJSON != `{"action":"command","display_name":"RunCommand"}` {
		t.Errorf("payload = %s", pub.got[0].PayloadJSON)
	}
}

type deadlinePublisher struct{ deadline bool }

func (p *deadlinePublisher) Publish(ctx context.Context, _ events.Event) error {
	deadline, ok := ctx.Deadline()
	p.deadline = ok && time.Until(deadline) <= 5*time.Second && time.Until(deadline) > 0
	return nil
}
func TestProviderSessionLostCallbackBoundsPersistence(t *testing.T) {
	pub := &deadlinePublisher{}
	makeProviderSessionLostCallback(pub, "s1", "agent")("old", "new", "lost")
	if !pub.deadline {
		t.Fatal("session-lost callback used unbounded persistence")
	}
}

func TestBootDirPlantedCallbackBoundsPersistence(t *testing.T) {
	pub := &deadlinePublisher{}
	makeBootDirPlantedCallback(pub, "s1", "agent")("/boot")
	if !pub.deadline {
		t.Fatal("boot directory callback used unbounded persistence")
	}
}
func TestLifecycleReportsBoundPersistence(t *testing.T) {
	for _, report := range []func(*Service){func(s *Service) { s.reportSweep([]string{"s1"}, nil) }, func(s *Service) { s.reportShutdownSessions([]string{"s1"}, nil) }} {
		svc, _ := outputHarness(t, nil)
		pub := &deadlineReportBus{Bus: svc.Bus}
		svc.Bus = pub
		report(svc)
		if !pub.deadlinePublisher.deadline {
			t.Fatal("lifecycle report used unbounded persistence")
		}
	}
}

type deadlineReportBus struct {
	events.Bus
	deadlinePublisher
}

func (b *deadlineReportBus) Publish(ctx context.Context, ev events.Event) error {
	return b.deadlinePublisher.Publish(ctx, ev)
}
