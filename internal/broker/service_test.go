package broker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/chrispian/agent-mux/internal/events"
)

type fakeStore struct {
	mu        sync.Mutex
	inserted  []Envelope
	insertErr error
}

func (f *fakeStore) CreateEnvelope(e Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, e)
	return nil
}

type fakePub struct {
	mu  sync.Mutex
	evs []events.Event
}

func (f *fakePub) Publish(_ context.Context, e events.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, e)
	return nil
}

func (f *fakePub) snapshot() []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]events.Event, len(f.evs))
	copy(out, f.evs)
	return out
}

type metaPayload struct {
	ID            string `json:"id"`
	Sender        string `json:"sender,omitempty"`
	Recipient     string `json:"recipient,omitempty"`
	WorkflowID    string `json:"workflow_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	MessageType   string `json:"message_type,omitempty"`
}

func TestService_CreateEnvelope_EmitsCreatedEvent(t *testing.T) {
	st := &fakeStore{}
	pub := &fakePub{}
	svc := NewService(st, pub)

	env := Envelope{
		ID:          "env-1",
		Sender:      "agent-a",
		Recipient:   "agent-b",
		WorkflowID:  "wf-1",
		MessageType: "request",
		Payload:     `{"secret":"should-not-leak"}`,
		CreatedAt:   "2026-04-19T00:00:00Z",
	}
	if err := svc.CreateEnvelope(context.Background(), env); err != nil {
		t.Fatalf("CreateEnvelope: %v", err)
	}

	evs := pub.snapshot()
	if len(evs) != 1 {
		t.Fatalf("events len = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Scope != events.ScopeBroker {
		t.Errorf("Scope = %q, want broker", ev.Scope)
	}
	if ev.Kind != events.KindBrokerEnvelopeCreated {
		t.Errorf("Kind = %q, want %q", ev.Kind, events.KindBrokerEnvelopeCreated)
	}
	if ev.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", ev.SessionID)
	}
	var p metaPayload
	if err := json.Unmarshal([]byte(ev.PayloadJSON), &p); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if p.ID != "env-1" || p.Sender != "agent-a" || p.Recipient != "agent-b" ||
		p.WorkflowID != "wf-1" || p.MessageType != "request" {
		t.Errorf("payload metadata mismatch: %+v", p)
	}
	// Envelope payload body must NOT leak into the event.
	for _, secret := range []string{"should-not-leak", env.Payload} {
		if contains(ev.PayloadJSON, secret) {
			t.Errorf("event payload leaked envelope body (%q found in %q)", secret, ev.PayloadJSON)
		}
	}
}

func TestService_ReplyEnvelope_EmitsRepliedEventWithCorrelation(t *testing.T) {
	st := &fakeStore{}
	pub := &fakePub{}
	svc := NewService(st, pub)

	reply := Envelope{
		ID:            "env-2",
		Sender:        "agent-b",
		Recipient:     "agent-a",
		CorrelationID: "env-1",
		MessageType:   "response",
		CreatedAt:     "2026-04-19T00:00:05Z",
	}
	if err := svc.ReplyEnvelope(context.Background(), reply); err != nil {
		t.Fatalf("ReplyEnvelope: %v", err)
	}

	evs := pub.snapshot()
	if len(evs) != 1 {
		t.Fatalf("events len = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Kind != events.KindBrokerEnvelopeReplied {
		t.Errorf("Kind = %q, want %q", ev.Kind, events.KindBrokerEnvelopeReplied)
	}
	var p metaPayload
	_ = json.Unmarshal([]byte(ev.PayloadJSON), &p)
	if p.ID != "env-2" {
		t.Errorf("payload.id = %q, want env-2", p.ID)
	}
	if p.CorrelationID != "env-1" {
		t.Errorf("payload.correlation_id = %q, want env-1", p.CorrelationID)
	}
}

func TestService_CreateEnvelope_StoreFailure_NoEvent(t *testing.T) {
	st := &fakeStore{insertErr: errors.New("boom")}
	pub := &fakePub{}
	svc := NewService(st, pub)

	err := svc.CreateEnvelope(context.Background(), Envelope{ID: "x", CreatedAt: "now"})
	if err == nil {
		t.Fatal("expected error from store")
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("events published despite store failure: %d", len(pub.snapshot()))
	}
}

func TestMessageType_ValidValues(t *testing.T) {
	for _, mt := range ValidMessageTypes() {
		if !IsValidMessageType(mt) {
			t.Errorf("ValidMessageTypes() returned %q but IsValidMessageType rejects it", mt)
		}
	}
}

func TestMessageType_InvalidRejects(t *testing.T) {
	for _, bad := range []string{"", "unknown", "REQUEST", "RESPONSE", "reply"} {
		if IsValidMessageType(bad) {
			t.Errorf("IsValidMessageType(%q) = true, want false", bad)
		}
	}
}

func TestService_CreateEnvelope_RejectsInvalidType(t *testing.T) {
	st := &fakeStore{}
	pub := &fakePub{}
	svc := NewService(st, pub)

	err := svc.CreateEnvelope(context.Background(), Envelope{
		ID:          "e1",
		MessageType: "bogus",
		CreatedAt:   "2026-04-21T00:00:00Z",
	})
	if err == nil {
		t.Error("expected error for invalid message_type, got nil")
	}
	if len(st.inserted) != 0 {
		t.Error("envelope should not be persisted when type is invalid")
	}
}

func TestService_CreateEnvelope_ResponseRequiresCorrelation(t *testing.T) {
	st := &fakeStore{}
	svc := NewService(st, nil)

	err := svc.CreateEnvelope(context.Background(), Envelope{
		ID:          "e2",
		MessageType: "response",
		// No CorrelationID
		CreatedAt: "2026-04-21T00:00:00Z",
	})
	if err == nil {
		t.Error("expected error for response without correlation_id, got nil")
	}
}

func TestService_CreateEnvelope_ResponseWithCorrelationOK(t *testing.T) {
	st := &fakeStore{}
	svc := NewService(st, nil)

	err := svc.CreateEnvelope(context.Background(), Envelope{
		ID:            "e3",
		MessageType:   "response",
		CorrelationID: "req-id",
		CreatedAt:     "2026-04-21T00:00:00Z",
	})
	if err != nil {
		t.Errorf("response with correlation_id should succeed: %v", err)
	}
}

func TestService_NilPublisher_NoCrash(t *testing.T) {
	st := &fakeStore{}
	svc := NewService(st, nil)
	if err := svc.CreateEnvelope(context.Background(), Envelope{ID: "x", CreatedAt: "now"}); err != nil {
		t.Fatal(err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
