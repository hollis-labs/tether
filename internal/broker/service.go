package broker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hollis-labs/tether/internal/events"
)

// EnvelopeStore is the storage seam Service depends on. *store.Store
// satisfies it; tests inject fakes. Keeping this narrow avoids an
// internal/broker → internal/store cycle (store imports broker for
// the Envelope type).
type EnvelopeStore interface {
	CreateEnvelope(e Envelope) error
}

// Service is the broker write layer. It persists envelopes through
// EnvelopeStore, publishes broker.* events on the Bus, and notifies
// the in-memory Dispatcher when a response arrives so any blocking
// Wait call can be unblocked.
type Service struct {
	store      EnvelopeStore
	publisher  events.Publisher
	dispatcher *Dispatcher
}

// NewService wires an EnvelopeStore and Publisher. Publisher may be nil
// (no events emitted). A Dispatcher is always created; callers can
// retrieve it via Dispatcher() to wire up blocking request endpoints.
func NewService(store EnvelopeStore, publisher events.Publisher) *Service {
	return &Service{
		store:      store,
		publisher:  publisher,
		dispatcher: NewDispatcher(),
	}
}

// Dispatcher returns the service's in-memory correlation dispatcher.
func (s *Service) Dispatcher() *Dispatcher { return s.dispatcher }

// WaitForResponse blocks until a response envelope with the given
// correlationID is delivered via CreateEnvelope, or ctx is canceled.
// Satisfies the api.BrokerService interface.
func (s *Service) WaitForResponse(ctx context.Context, correlationID string) (*Envelope, error) {
	return s.dispatcher.Wait(ctx, correlationID)
}

// envelopeMeta is the lean metadata shape published on broker.*
// events. Deliberately excludes Payload, Priority, and audit fields:
// events must not leak envelope bodies; subscribers fetch bodies
// through the read path (GET /broker/envelopes/{id}) if they need
// them.
type envelopeMeta struct {
	ID            string `json:"id"`
	Sender        string `json:"sender,omitempty"`
	Recipient     string `json:"recipient,omitempty"`
	WorkflowID    string `json:"workflow_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	MessageType   string `json:"message_type,omitempty"`
}

// CreateEnvelope validates the envelope, persists it, and publishes
// broker.envelope_created on success. Validation: message_type must be
// in the enum; response must carry a correlation_id. Store errors are
// returned without publishing (persistence-before-emit ordering).
func (s *Service) CreateEnvelope(ctx context.Context, e Envelope) error {
	if e.MessageType != "" && !IsValidMessageType(e.MessageType) {
		return fmt.Errorf("broker: unknown message_type %q; valid types: %v", e.MessageType, ValidMessageTypes())
	}
	if e.MessageType == TypeResponse && e.CorrelationID == "" {
		return fmt.Errorf("broker: response envelope requires a correlation_id")
	}
	if err := s.store.CreateEnvelope(e); err != nil {
		return err
	}
	s.publishEnvelope(ctx, events.KindBrokerEnvelopeCreated, e)
	// Unblock any Wait calls registered for this correlation ID.
	if e.MessageType == TypeResponse && e.CorrelationID != "" {
		s.dispatcher.Deliver(&e)
	}
	return nil
}

// ReplyEnvelope persists a reply envelope (expected to carry
// CorrelationID referencing the original envelope's id) and
// publishes broker.envelope_replied. The Service does not validate
// that CorrelationID references an existing envelope — callers
// enforce that invariant.
func (s *Service) ReplyEnvelope(ctx context.Context, reply Envelope) error {
	if err := s.store.CreateEnvelope(reply); err != nil {
		return err
	}
	s.publishEnvelope(ctx, events.KindBrokerEnvelopeReplied, reply)
	return nil
}

func (s *Service) publishEnvelope(ctx context.Context, kind string, e Envelope) {
	if s.publisher == nil {
		return
	}
	payload, err := json.Marshal(envelopeMeta{
		ID:            e.ID,
		Sender:        e.Sender,
		Recipient:     e.Recipient,
		WorkflowID:    e.WorkflowID,
		CorrelationID: e.CorrelationID,
		MessageType:   e.MessageType,
	})
	if err != nil {
		return
	}
	_ = s.publisher.Publish(ctx, events.Event{
		Scope:       events.ScopeBroker,
		Kind:        kind,
		PayloadJSON: string(payload),
	})
}
