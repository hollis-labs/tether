package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/chrispian/agent-mux/internal/events"
)

// fakePublisher captures every Publish call for assertion in tests.
type fakePublisher struct {
	mu   sync.Mutex
	sent []events.Event
}

func (p *fakePublisher) Publish(_ context.Context, e events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, e)
	return nil
}

func (p *fakePublisher) snapshot() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]events.Event, len(p.sent))
	copy(out, p.sent)
	return out
}

// TestMakeBootDirPlantedCallback_EmitsEvent pins the v005-07 wiring:
// the StartOptions.OnBootDirPlanted callback we build in LaunchSession
// must publish a KindSessionBootDirPlanted event to the bus with the
// session id, logical agent id, and JSON payload {"path":"<absolute>"}.
func TestMakeBootDirPlantedCallback_EmitsEvent(t *testing.T) {
	pub := &fakePublisher{}
	cb := makeBootDirPlantedCallback(pub, "sess-123", "logical-agent-7")

	cb("/tmp/agent-sessions-boot-abc123")

	got := pub.snapshot()
	if len(got) != 1 {
		t.Fatalf("publish count = %d, want 1", len(got))
	}
	ev := got[0]
	if ev.Kind != events.KindSessionBootDirPlanted {
		t.Errorf("Kind = %q, want %q", ev.Kind, events.KindSessionBootDirPlanted)
	}
	if ev.Scope != events.ScopeSession {
		t.Errorf("Scope = %q, want %q", ev.Scope, events.ScopeSession)
	}
	if ev.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, "sess-123")
	}
	if ev.LogicalAgentID != "logical-agent-7" {
		t.Errorf("LogicalAgentID = %q, want %q", ev.LogicalAgentID, "logical-agent-7")
	}
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v (raw=%q)", err, ev.PayloadJSON)
	}
	if payload.Path != "/tmp/agent-sessions-boot-abc123" {
		t.Errorf("payload.path = %q, want %q", payload.Path, "/tmp/agent-sessions-boot-abc123")
	}
}

// TestMakeBootDirPlantedCallback_NilBusIsNoOp asserts the callback is
// safe to invoke when the service was composed without a bus (test
// composition paths that skip event wiring).
func TestMakeBootDirPlantedCallback_NilBusIsNoOp(t *testing.T) {
	cb := makeBootDirPlantedCallback(nil, "sess-1", "agent-1")
	cb("/tmp/anything") // must not panic
}
