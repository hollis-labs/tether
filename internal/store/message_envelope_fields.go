package store

import (
	"context"
	"encoding/json"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

// messageEnvelopeFields are the routing fields of a typed envelope
// (CW-20261114-0013) that message.* lifecycle events carry: kind, attention,
// awaiting, intent and re, and on_behalf_of/drafted_by when set
// (CW-20261114-0116). Never the subject, body or options: lifecycle events
// stay metadata only. Nil for a message that is not an envelope.
func (s *Store) messageEnvelopeFields(ctx context.Context, messageID string) map[string]any {
	if messageID == "" {
		return nil
	}
	// Ordinary mail never loads its body here.
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(payload,'') FROM messages WHERE id=? AND content_type=?`, messageID, envelope.ContentType).Scan(&payload); err != nil {
		return nil
	}
	var e envelope.Envelope
	if json.Unmarshal([]byte(payload), &e) != nil || e.Kind == "" {
		return nil
	}
	fields := map[string]any{"envelope_kind": string(e.Kind)}
	if e.OnBehalfOf != "" && e.DraftedBy == "" {
		e.DraftedBy = envelope.DraftedByAssistant
	}
	for k, v := range map[string]string{"attention": string(e.EffectiveAttention()), "awaiting": e.Awaiting, "intent": e.Intent, "re": e.Re,
		"on_behalf_of": e.OnBehalfOf, "drafted_by": string(e.DraftedBy)} {
		if v != "" {
			fields[k] = v
		}
	}
	return fields
}

// withEnvelopeFields merges the message's envelope fields into a lifecycle
// payload; raw is returned unchanged for an ordinary message.
func (s *Store) withEnvelopeFields(ctx context.Context, raw string) string {
	var p map[string]any
	if json.Unmarshal([]byte(raw), &p) != nil {
		return raw
	}
	id, _ := p["message_id"].(string)
	fields := s.messageEnvelopeFields(ctx, id)
	if fields == nil {
		return raw
	}
	for k, v := range fields {
		p[k] = v
	}
	out, err := json.Marshal(p)
	if err != nil {
		return raw
	}
	return string(out)
}
