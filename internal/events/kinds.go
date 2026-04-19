package events

// Event kind constants. Keep this list small and justified — clients
// depend on stable kind strings. New kinds should be documented in
// docs/api/events.md alongside their payload schema.
const (
	// KindSessionStateChanged is emitted by runtime.Manager on each
	// session lifecycle transition (created → launching → running →
	// completed|failed|killed). Payload schema:
	//   {"from":"<prev>","to":"<next>","exit_code":<int,optional>,"reason":"<string,optional>"}
	KindSessionStateChanged = "session.state_changed"

	// KindBrokerEnvelopeCreated is emitted when a broker envelope is
	// persisted (broker service layer). Payload carries metadata only
	// (id, sender, recipient, workflow_id, correlation_id,
	// message_type) — never the full envelope body.
	KindBrokerEnvelopeCreated = "broker.envelope_created"

	// KindBrokerEnvelopeReplied is emitted when a reply envelope is
	// persisted that references a prior envelope's correlation id.
	KindBrokerEnvelopeReplied = "broker.envelope_replied"

	// KindDaemonStarted / KindDaemonShutdownStarted /
	// KindDaemonShutdownCompleted bracket the muxd daemon's lifetime.
	KindDaemonStarted           = "daemon.started"
	KindDaemonShutdownStarted   = "daemon.shutdown_started"
	KindDaemonShutdownCompleted = "daemon.shutdown_completed"
)
