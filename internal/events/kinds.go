package events

// Event kind constants. Keep this list small and justified — clients
// depend on stable kind strings. New kinds should be documented in
// docs/api/events.md alongside their payload schema.
const (
	// KindSessionStateChanged is emitted by the eventSinkAdapter wired
	// into agentsessions.Manager on each session lifecycle transition
	// (created → launching → running → done|failed; the adapter remaps
	// done+reason="killed" → "killed" for mux-domain consumers).
	// Payload schema:
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

	// KindSessionBootDirPlanted fires once per session start when
	// go-agent-sessions v0.9.x materializes the adapter's BootDirSpec
	// into a per-session tempdir. Carries {"path":"<absolute>"} so
	// attach observers and operators can locate the planted files for
	// the lifetime of the session (cleanup is automatic at terminal
	// state).
	KindSessionBootDirPlanted = "session.boot_dir_planted"
)
