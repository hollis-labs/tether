package app

import "github.com/hollis-labs/tether/internal/events"

// prepareOutputPersistence runs before the first durable boundary. An explicit
// native secret class is a policy floor, never a detector finding that Tether
// can downgrade. Only an app-authored notice may be journaled or routed.
// Detector-based redaction remains gated on the reviewed sanitizer release.
func (w *turnOutputWrite) prepareOutputPersistence(s *Service) {
	if w.origin == nil || w.origin.Envelope.Class.Sensitivity != "secret" {
		return
	}
	w.withheldReason = "secret_withheld"
	w.withheldSize = len(w.result.Text)
	w.result.Text = events.SecretWithheldNotice(w.row.ID, w.result.TurnID, w.withheldSize)
	w.result.StopReason = ""
	w.textTruncated = false
	// The event now describes the withholding, not the secret content. Keep
	// provenance and PII, but do not inherit content labels/schema or claim a
	// detector redaction count. PayloadDigest remains retention-owned.
	origin := *w.origin
	origin.Envelope.Class.Sensitivity = "private"
	origin.Envelope.Class.Labels = []string{"secret_withheld"}
	origin.Envelope.Class.PayloadSchema = ""
	origin.Envelope.Class.Sanitized = true
	origin.Envelope.Class.Redactions = 0
	origin.Envelope.Truncated = false
	w.origin = &origin
	s.secretOutputWithheld.Add(1)
}

// TurnOutputPersistenceStats counts newly withheld outputs in this process.
// Replaying the already-safe notice does not count or retain its former body.
func (s *Service) TurnOutputPersistenceStats() map[string]uint64 {
	return map[string]uint64{"secret_withheld": s.secretOutputWithheld.Load(), "withheld_identity_violation": s.secretOutputDuplicate.Load()}
}
