package events

// Event kind constants. Keep this list small and justified — clients
// depend on stable kind strings. New kinds should be documented in
// docs/api/README.md alongside their payload schema.
const (
	// KindStreamGap is a synthetic signal a Bus delivers to a subscriber in place
	// of silent loss: it is never persisted, always bypasses the subscriber's
	// Filter, and carries Seq = the last sequence the subscriber processed (so an
	// SSE id never moves). Payload (see GapPayload): reason, dropped, last_seq,
	// earliest_available, high_water, recovered.
	KindStreamGap = "stream.gap"

	// KindSessionTurnRouted is the atomic durable audit of channel attachment.
	KindSessionTurnRouted = "session.turn_routed"

	// KindSessionShimRetired audits an operator retirement of a detached shim session.
	KindSessionShimRetired = "session.shim_retired"

	// KindSessionTurnOutput carries a reduced turn and an optional staged message id.
	KindSessionTurnOutput = "session.turn_output"

	// KindSessionStateChanged is emitted by the eventSinkAdapter wired
	// into agentsessions.Manager on each session lifecycle transition
	// (including detached/orphaned recovery; only completed/failed/killed
	// are terminal). The adapter maps the lib's "done" to "completed", and a terminal state
	// after a stop request to "killed".
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
	// KindDaemonShutdownCompleted bracket the tetherd daemon's lifetime.
	KindDaemonStarted           = "daemon.started"
	KindDaemonShutdownStarted   = "daemon.shutdown_started"
	KindDaemonShutdownCompleted = "daemon.shutdown_completed"

	// KindDaemonShutdownSessionsEnded is emitted during a graceful daemon
	// shutdown, after the session drain (CW-20260912-0086). Sessions in
	// ended were recorded `killed` with reason "daemon-shutdown"; sessions
	// in still_running had not exited when the drain ended and are left for
	// the next start's sweep. Payload schema:
	//   {"ended":<n>,"ended_session_ids":[...],"still_running":<n>,"still_running_session_ids":[...]}
	KindDaemonShutdownSessionsEnded = "daemon.shutdown_sessions_ended"

	// KindDaemonSessionsSwept is emitted at daemon start when the startup
	// sweep settles sessions the previous daemon left launching/running
	// (CW-20260912-0085). Swept sessions were failed with exit_code -1
	// ("swept at daemon start", not an observed exit); spared ones still
	// had their own process alive and keep their state. Payload schema:
	//   {"swept":<n>,"swept_session_ids":[...],"spared":<n>,"spared_session_ids":[...]}
	KindDaemonSessionsSwept = "daemon.sessions_swept"

	// KindDaemonExecutionPolicyChanged is emitted when an operator changes
	// the execution policy (pause/resume or cap change). Unchanged PUTs emit
	// nothing. Payload schema:
	//   {"change":"paused|resumed|caps_changed","actor":"<principal id or empty>","at":"<RFC3339>","before":<policy>,"after":<policy>}
	KindDaemonExecutionPolicyChanged = "daemon.execution_policy_changed"

	// KindAIBudgetRejected is emitted when durable AI usage_budget policy
	// rejects one route candidate. Payload schema:
	//   {"request_id":"...","caller_id":"...","session_id":"...","provider":"...","model":"...","policy_version":"...","error":"..."}
	KindAIBudgetRejected = "ai.budget_rejected"

	// KindSessionBootDirPlanted fires once per session start when
	// go-agent-sessions v0.9.x materializes the adapter's BootDirSpec
	// into a per-session tempdir. Carries {"path":"<absolute>"} so
	// attach observers and operators can locate the planted files for
	// the lifetime of the session (cleanup is automatic at terminal
	// state).
	KindSessionBootDirPlanted = "session.boot_dir_planted"

	// KindProviderSessionLost fires when a resume turn did not continue the
	// requested provider session and ran in a new one instead (Antigravity
	// does this silently for an unknown conversation id). The turn itself
	// ran; the old history is not in the new session. Payload schema:
	//   {"requested":"<id>","actual":"<id>","reason":"<string>"}
	KindProviderSessionLost = "provider.session_lost"

	// KindProviderPermissionDenied fires for each tool action a headless
	// provider refused because it needed an approval it could not ask for,
	// so an otherwise silent no-op is visible. Payload schema:
	//   {"action":"<string>","display_name":"<string>"}
	KindProviderPermissionDenied = "provider.permission_denied"

	// KindSessionStalled fires once when a session's refused tool actions
	// since its last turn output reach the stall threshold, so a worker that
	// is silently making no progress is visible to operators. Payload schema:
	//   {"reason":"repeated_tool_denials","denied":<int>}
	KindSessionStalled = "session.stalled"
)
