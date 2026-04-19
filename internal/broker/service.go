package broker

import (
	"context"
	"encoding/json"

	"github.com/chrispian/agent-mux/internal/events"
)

// EnvelopeStore is the storage seam Service depends on. *store.Store
// satisfies it; tests inject fakes. Keeping this narrow avoids an
// internal/broker → internal/store cycle (store imports broker for
// the Envelope type).
type EnvelopeStore interface {
	CreateEnvelope(e Envelope) error
}

// Service is the broker write layer. It persists envelopes through
// EnvelopeStore and publishes broker.* events on the Bus. Sprint
// v002-05's HTTP broker endpoints wrap Service, so event emission
// is inherited by the API surface for free. Request/reply blocking
// semantics are Sprint v003-05 territory — not here.
type Service struct {
	store     EnvelopeStore
	publisher events.Publisher
}

// NewService wires an EnvelopeStore and Publisher. Publisher may be
// nil, in which case envelope persistence still happens but no
// events are emitted.
func NewService(store EnvelopeStore, publisher events.Publisher) *Service {
	return &Service{store: store, publisher: publisher}
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

// CreateEnvelope persists the envelope and publishes
// broker.envelope_created on success. Store errors are returned
// without publishing anything (persistence-before-emit ordering).
func (s *Service) CreateEnvelope(ctx context.Context, e Envelope) error {
	if err := s.store.CreateEnvelope(e); err != nil {
		return err
	}
	s.publishEnvelope(ctx, events.KindBrokerEnvelopeCreated, e)
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
