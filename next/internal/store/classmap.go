// The class of every table, derived from the per-table classification (CW-20261114-0149) behind plan
// section 13.2, with its one-line reason. Maintained by hand: a migration that adds a table adds it here,
// and TestEveryTableHasAClass fails until it does.

package store

// tableClass maps every table to its storage class.
var tableClass = map[string]Class{
	// Class A.
	"event_identity":              A, // environment/event identity singleton, seeded and read at boot
	"event_retention_admission":   A, // kind -> retention class mapping, rewritten at daemon start
	"event_retention_policies":    A, // immutable retention policy epochs (config)
	"execution_policy":            A, // singleton execution policy
	"fabric_actors":               A, // read by the fabric repository's commit-time invariant at boot (teams enabled)
	"fabric_agents":               A, // same invariant as fabric_actors
	"fabric_definition_artifacts": A, // same Index transaction as fabric_definition_revisions at boot
	"fabric_definition_revisions": A, // policy definition index built at boot when teams are enabled
	"logical_agents":              A, // seeded from the catalog at every start (seedLogicalAgents aborts boot on error)
	"pairing_grants":              A, // device pairing exchange (auth route); hashed one-time grants
	"principals":                  A, // every stored-token check reads it; enforce mode refuses with 503 if unreadable; operator bootstrap at start
	"settings":                    A, // settings cascade (global/project/user)
	"team_definitions":            A, // team definitions (definitions)
	"worker_mail_grants":          A, // worker mail authorization checked per request
	"workers":                     A, // outpost hello/auth reads it (worker identity)

	// Class B.
	"event_ingest_digests":     B, // idempotency digests for POST /events ingest, swept with events
	"events":                   B, // main event log, 90-day rolling window
	"identity_audit":           B, // per-request auth observations, ~335k rows/day
	"proxy_events":             B, // MCP proxy tool-call events (activity feed, tool metrics)
	"retention_audit":          B, // receipts of retention sweep batches
	"session_usage":            B, // per-session turn/token/cost meter (telemetry)
	"team_activation_evidence": B, // report-only team activation observations
	"turn_inputs":              B, // turn input provenance; swept by the events retention window

	// Class C.
	"a2a_tasks":                       C, // inbound A2A tasks
	"agent_recovery_cursors":          C, // per-agent recovery delivery cursors
	"broker_envelopes":                C, // legacy v002 mailbox envelopes
	"channel_publications":            C, // public channel publication sequence
	"checkpoints":                     C, // logical agent checkpoints
	"client_attachments":              C, // live attach subscriptions
	"codex_shim_protocol":             C, // Codex shim protocol state
	"environment_request_resolutions": C, // environment request fences
	"environment_request_status":      C, // environment request state
	"environment_request_turn_ends":   C, // environment turn-end fences
	"environment_work_state":          C, // runtime work state per session
	"fabric_admissions":               C, // fabric records
	"fabric_binding_heads":            C, // fabric records
	"fabric_binding_history":          C, // fabric records
	"fabric_import_receipts":          C, // fabric records
	"fabric_instances":                C, // fabric records
	"fabric_legacy_refs":              C, // fabric records
	"fabric_migration_candidates":     C, // fabric records
	"fabric_outbox":                   C, // fabric aggregate event outbox
	"fabric_sessions":                 C, // fabric records
	"forum_categories":                C, // forum categories
	"forum_participants":              C, // forum access
	"forum_post_fts":                  C, // FTS index over forum_posts (rebuilt)
	"forum_posts":                     C, // forum posts
	"forum_topics":                    C, // forum topics
	"forums":                          C, // forums
	"group_members":                   C, // group membership and read cursor
	"launch_plans":                    C, // resolved launch plan per session
	"message_aliases":                 C, // readable alias -> message URN
	"message_event_checkpoints":       C, // cursors over messaging_transitions
	"message_event_deferred":          C, // deferred delivery transitions (work queue)
	"message_event_journal":           C, // mail attention journal written by triggers on messages
	"message_purge_audit":             C, // receipts of manual message purges
	"messages":                        C, // mailbox messages (rowid is the watch cursor)
	"recipient_wake_admissions":       C, // wake admission per message
	"registry_capabilities":           C, // registry capabilities
	"registry_entries":                C, // federation directory profiles
	"registry_external_ids":           C, // registry external ids
	"registry_links":                  C, // registry links
	"registry_skills":                 C, // registry skills
	"routing_replies":                 C, // routed-message reply dispatch queue
	"runtime_bindings":                C, // leased runtime bindings target -> session
	"scoped_role_bindings":            C, // revisioned scoped role bindings
	"seat_status":                     C, // declared seat status
	"seat_wake_clock":                 C, // per-seat last wake
	"session_groups":                  C, // workflow-scoped session groups
	"session_idempotency":             C, // idempotent session create/resume keys
	"session_mcp_policy":              C, // effective MCP authority per session
	"session_provider_mappings":       C, // session -> provider session id
	"session_refs":                    C, // objects a session touched
	"session_replacements":            C, // session replacement lineage
	"session_shim_retention_cursors":  C, // dead (no production writer)
	"session_shim_retirements":        C, // shim retirement receipts
	"session_shims":                   C, // shim placement/authority
	"session_worker_seq":              C, // last worker frame sequence per session
	"sessions":                        C, // core session rows
	"team_gone_shim_recoveries":       C, // gone-shim recovery custody
	"team_host_adoptions":             C, // team host
	"team_host_approvals":             C, // team host
	"team_host_bindings":              C, // team host
	"team_host_calls":                 C, // team host
	"team_host_delegations":           C, // team host
	"team_host_deliveries":            C, // team host
	"team_host_intents":               C, // team host
	"team_host_launch_intents":        C, // team host
	"team_host_messages":              C, // team host
	"team_host_recovery_cursors":      C, // team host
	"team_host_routing":               C, // team host
	"team_host_workflows":             C, // team host
	"team_launch_grants":              C, // authority a team session launched under
	"team_launch_journal":             C, // team launch journal
	"team_launch_leases":              C, // team launch leases
	"team_launches":                   C, // team launches
	"team_phase_signals":              C, // team phase signals
	"team_port_intents":               C, // team port intents (binding_secret column)
	"team_recovery_attempts":          C, // team recovery retries
	"team_roster_snapshots":           C, // roster history
	"team_rosters":                    C, // current team roster
	"team_runs":                       C, // team runs
	"team_signal_resolutions":         C, // resolved phase signals
	"wake_triage":                     C, // hub mail triage before wake
	"worker_permission_parser":        C, // permission forwarding parser checkpoints
	"workspace_scratch_allocations":   C, // workspace scratch allocations
	"workstream_projects":             C, // workstream <-> project
	"workstreams":                     C, // workstream containers

	// Carried from main after the classification's snapshot.
	"address_aliases": C, // address alias chain used to route mail (old migration 0103)

	// migrate-from's bookkeeping (internal/migrate), created by the engine, not by migrations: the S0
	// guard reads an existing tether_migration_mark as "this database was migrated".
	"tether_migration_mark":     A,
	"tether_migration_tables":   A,
	"tether_migration_ranges":   A,
	"tether_migration_triggers": A,

	// go-messaging's tables (created by the library at open, not by migrations): class C.
	"messaging_attempts":        C,
	"messaging_deliveries":      C,
	"messaging_delivery_schema": C,
	"messaging_idempotency":     C,
	"messaging_messages":        C,
	"messaging_receipts":        C,
	"messaging_transitions":     C,
}
