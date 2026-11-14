-- Tether's squashed schema: the old store's schema at its migration 0101 (main f6c37bfa,
-- internal/store/migrations), minus the objects the allowlist in internal/migrate/schema_test.go names.
-- GENERATED: do not edit. Regenerate with
--   go test ./internal/migrate -run TestSchemaEqualsOld -update
-- and review the diff table by table. Later schema changes are new migrations (00002 onwards), never
-- edits here. One-way: there is no Down.
-- The seven messaging_* tables are go-messaging's and are created by the library at open.

-- +goose Up

CREATE TABLE launch_plans (
    session_id    TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    plan_json     TEXT NOT NULL
);

CREATE TABLE client_attachments (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES sessions(id),
    client_kind   TEXT,
    attached_at   TEXT NOT NULL,
    detached_at   TEXT
);

CREATE INDEX idx_client_attachments_session
    ON client_attachments(session_id, attached_at);

CREATE TABLE logical_agents (
    id                TEXT PRIMARY KEY,
    role              TEXT,
    name              TEXT,
    responsibilities  TEXT,
    capabilities      TEXT,
    memory_scopes     TEXT,
    policies_json     TEXT,
    permitted_tools   TEXT,
    escalation_rules  TEXT,
    checkpoint_policy TEXT,
    hot_cold_policy   TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
, claude_session_id TEXT, launch_id TEXT);

CREATE TABLE "sessions" (
    id                TEXT PRIMARY KEY,
    launch_id         TEXT NOT NULL,
    project_id        TEXT NOT NULL,
    logical_agent_id  TEXT NOT NULL REFERENCES logical_agents(id),
    provider_id       TEXT NOT NULL,
    workspace         TEXT NOT NULL,
    state             TEXT NOT NULL,
    pid               INTEGER,
    exit_code         INTEGER,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    ended_at          TEXT
, session_group_id TEXT REFERENCES session_groups(id), provider_kind TEXT NOT NULL DEFAULT '', parent_session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL, intent TEXT NOT NULL DEFAULT 'fresh'
    CHECK(intent IN ('preassigned', 'resume', 'compact', 'fresh', 'fork')), publication TEXT NOT NULL DEFAULT 'private-local'
    CHECK(publication IN ('private-local', 'published-local', 'tether-hosted')), workstream_id TEXT REFERENCES workstreams(id) ON DELETE SET NULL, ref_attribution TEXT, pid_started_at TEXT, route_json TEXT CHECK(route_json IS NULL OR json_valid(route_json)), worker_id TEXT NOT NULL DEFAULT '', task_id TEXT, app TEXT);

CREATE INDEX idx_sessions_state ON sessions(state);

CREATE INDEX idx_sessions_logical_agent ON sessions(logical_agent_id);

CREATE TABLE checkpoints (
    id                   TEXT PRIMARY KEY,
    logical_agent_id     TEXT NOT NULL REFERENCES logical_agents(id),
    task_id              TEXT,
    workflow_id          TEXT,
    status               TEXT,
    completed_work       TEXT,
    pending_work         TEXT,
    key_decisions        TEXT,
    referenced_artifacts TEXT,
    summary              TEXT,
    next_recommendation  TEXT,
    created_at           TEXT NOT NULL,
    source_session_id    TEXT REFERENCES sessions(id)
, provider_hints TEXT);

CREATE INDEX idx_checkpoints_logical_agent
    ON checkpoints(logical_agent_id, created_at);

CREATE INDEX idx_checkpoints_workflow
    ON checkpoints(workflow_id, created_at);

CREATE TABLE broker_envelopes (
    id             TEXT PRIMARY KEY,
    sender         TEXT,
    recipient      TEXT,
    workflow_id    TEXT,
    correlation_id TEXT,
    message_type   TEXT,
    priority       INTEGER,
    payload        TEXT,
    created_at     TEXT NOT NULL,
    delivered_at   TEXT,
    consumed_at    TEXT,
    audit_json     TEXT
);

CREATE INDEX idx_broker_envelopes_recipient
    ON broker_envelopes(recipient, delivered_at);

CREATE INDEX idx_broker_envelopes_correlation
    ON broker_envelopes(workflow_id, correlation_id);

CREATE TABLE "events" (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    scope        TEXT NOT NULL DEFAULT 'session',
    session_id   TEXT,
    at           TEXT NOT NULL,
    kind         TEXT NOT NULL,
    payload_json TEXT
, event_id TEXT, pending INTEGER NOT NULL DEFAULT 0, scope_environment_id TEXT, scope_authority TEXT, scope_worker_id TEXT, scope_host TEXT, scope_project_id TEXT, scope_workstream_id TEXT, scope_epic_id TEXT, scope_sprint_id TEXT, scope_plan_id TEXT, scope_phase_id TEXT, scope_task_id TEXT, scope_run_id TEXT, scope_attempt INTEGER, scope_launch_id TEXT, scope_agent_urn TEXT, scope_logical_agent_id TEXT, scope_team_run_id TEXT, category TEXT, severity TEXT, sensitivity TEXT, pii INTEGER, visibility TEXT, payload_schema TEXT, sanitized INTEGER, redactions INTEGER, labels_json TEXT, content_type TEXT, truncated INTEGER, trace_id TEXT, span_id TEXT, causation_id TEXT, correlation_id TEXT, thread_id TEXT, idempotency_key TEXT, depth INTEGER, actor_kind TEXT, actor_urn TEXT, actor_verified TEXT, on_behalf_of TEXT, recorded_at TEXT, epoch INTEGER, retention_class TEXT
  CHECK (retention_class IS NULL OR retention_class IN ('R-audit','R-state','R-activity','R-debug')), retention_policy_id TEXT, payload_digest TEXT
  CHECK (payload_digest IS NULL OR payload_digest = 'withheld'
         OR (length(payload_digest) = 71 AND substr(payload_digest, 1, 7) = 'sha256:'
             AND substr(payload_digest, 8) NOT GLOB '*[^0-9a-f]*'))
  CHECK (payload_digest IS NULL OR payload_json IS NULL), schema_version TEXT, producer_app TEXT, subject TEXT, turn_id TEXT, parent_id TEXT, raw_offset INTEGER, process_pid INTEGER, process_provider TEXT, process_runtime TEXT, provider_session_id TEXT, source_channel TEXT, source_confidence TEXT, producer_sequence TEXT, producer_generation TEXT, traceparent TEXT, in_reply_to TEXT, actor_role TEXT, on_behalf_of_json TEXT, origin_at TEXT, summary TEXT, scope_cell TEXT);

CREATE INDEX idx_events_session ON events(session_id, at);

CREATE INDEX idx_events_scope ON events(scope, at);

CREATE TABLE session_groups (
    id         TEXT PRIMARY KEY,
    name       TEXT,
    workflow_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_sessions_group
    ON sessions(session_group_id);

CREATE TABLE messages (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    channel      TEXT,
    from_urn     TEXT NOT NULL,
    to_urn       TEXT NOT NULL,
    thread_id    TEXT,
    in_reply_to  TEXT,
    payload      TEXT,
    content_type TEXT,
    metadata     TEXT,
    created_at   TEXT NOT NULL,
    delivered_at TEXT,
    consumed_at  TEXT,
    canceled_at  TEXT
, read_at TEXT, archived_at TEXT, group_urn TEXT REFERENCES registry_entries(urn) ON DELETE SET NULL, group_seq INTEGER, delivery_id TEXT, delivery_message_id TEXT, pending_receipt_attempt_id TEXT, pending_receipt_lease_token TEXT, routing_staged INTEGER NOT NULL DEFAULT 0 CHECK (routing_staged IN (0, 1)));

CREATE INDEX idx_messages_to_delivered
    ON messages(to_urn, delivered_at);

CREATE INDEX idx_messages_thread
    ON messages(thread_id, created_at);

CREATE INDEX idx_messages_in_reply_to
    ON messages(in_reply_to);

CREATE TABLE proxy_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT,
    server        TEXT    NOT NULL,
    tool_name     TEXT    NOT NULL,
    args_schema_fp TEXT,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    ok            INTEGER NOT NULL DEFAULT 1,  -- 1 = success, 0 = error
    error         TEXT,
    timestamp     TEXT    NOT NULL
, attribution_json TEXT NOT NULL DEFAULT '{"verified":false}', claimed_session_id TEXT NOT NULL DEFAULT '', telemetry_json TEXT NOT NULL DEFAULT '{}');

CREATE INDEX idx_proxy_events_timestamp
    ON proxy_events(timestamp DESC);

CREATE INDEX idx_proxy_events_server
    ON proxy_events(server, timestamp DESC);

CREATE INDEX idx_messages_to_archived
    ON messages(to_urn, archived_at, created_at);

CREATE INDEX idx_messages_to_read ON messages(to_urn, read_at);

CREATE TABLE registry_capabilities (
    urn         TEXT NOT NULL,
    capability  TEXT NOT NULL,
    PRIMARY KEY (urn, capability),
    FOREIGN KEY (urn) REFERENCES registry_entries(urn) ON DELETE CASCADE
);

CREATE INDEX idx_registry_capabilities_capability
    ON registry_capabilities(capability);

CREATE TABLE registry_skills (
    urn         TEXT NOT NULL,
    name        TEXT NOT NULL,
    learned_at  DATETIME NOT NULL,
    via         TEXT,
    level       TEXT,
    PRIMARY KEY (urn, name),
    FOREIGN KEY (urn) REFERENCES registry_entries(urn) ON DELETE CASCADE
);

CREATE INDEX idx_registry_skills_name
    ON registry_skills(name);

CREATE TABLE registry_links (
    urn     TEXT NOT NULL,
    kind    TEXT NOT NULL,
    target  TEXT NOT NULL,
    PRIMARY KEY (urn, kind, target),
    FOREIGN KEY (urn) REFERENCES registry_entries(urn) ON DELETE CASCADE
);

CREATE TABLE "registry_entries" (
    urn              TEXT NOT NULL PRIMARY KEY,
    kind             TEXT NOT NULL CHECK(kind IN ('agent', 'project', 'group')),
    mux_instance_id  TEXT NOT NULL DEFAULT 'agent-mux',
    display_name     TEXT NOT NULL,
    title            TEXT,
    role             TEXT,
    description      TEXT,
    avatar           TEXT,
    project          TEXT,
    status           TEXT NOT NULL DEFAULT 'active',
    callback_json    TEXT,
    cached_at        DATETIME,
    health_status    TEXT,
    last_seen_at     DATETIME,
    host_address     TEXT,
    kind_meta_json   TEXT,
    last_updated_by  TEXT,
    created_at       DATETIME NOT NULL,
    updated_at       DATETIME NOT NULL
, merged_into TEXT REFERENCES registry_entries(urn) ON DELETE SET NULL, owner TEXT, tags_json TEXT, guidelines TEXT, entry_points_json TEXT, field_metadata_json TEXT, props_json TEXT, tether_instance_id TEXT);

CREATE INDEX idx_registry_entries_kind
    ON registry_entries(kind);

CREATE INDEX idx_registry_entries_kind_status
    ON registry_entries(kind, status);

CREATE INDEX idx_registry_entries_kind_project
    ON registry_entries(kind, project);

CREATE INDEX idx_registry_entries_kind_role
    ON registry_entries(kind, role);

CREATE TABLE group_members (
    grp_urn       TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    member_urn    TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    role          TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('member','moderator','owner')),
    joined_at     DATETIME NOT NULL,
    last_read_seq INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (grp_urn, member_urn)
);

CREATE INDEX idx_group_members_by_member
    ON group_members(member_urn);

CREATE INDEX idx_messages_group_seq
    ON messages(group_urn, group_seq);

CREATE TABLE registry_external_ids (
    urn         TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    substrate   TEXT NOT NULL,
    external_id TEXT NOT NULL,
    attached_at DATETIME NOT NULL,
    PRIMARY KEY (urn, substrate)
);

CREATE INDEX idx_registry_external_ids_lookup
    ON registry_external_ids (substrate, external_id);

CREATE INDEX idx_sessions_parent ON sessions(parent_session_id);

CREATE TABLE session_provider_mappings (
    session_id        TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    owner             TEXT NOT NULL,
    provider          TEXT NOT NULL,
    native_session_id TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (session_id, owner, provider)
);

CREATE INDEX idx_session_provider_mappings_native
    ON session_provider_mappings(provider, native_session_id);

CREATE TABLE runtime_bindings (
    id               TEXT PRIMARY KEY,
    target_urn       TEXT NOT NULL,
    session_id       TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    host_id          TEXT NOT NULL,
    attempt_id       TEXT NOT NULL,
    generation       INTEGER NOT NULL,
    capabilities_json TEXT,
    visibility       TEXT NOT NULL DEFAULT 'private-local'
        CHECK(visibility IN ('private-local', 'published-local', 'tether-hosted')),
    leased_at        TEXT NOT NULL,
    lease_expires_at TEXT,
    revoked_at       TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE INDEX idx_runtime_bindings_target_generation
    ON runtime_bindings(target_urn, generation DESC);

CREATE INDEX idx_runtime_bindings_session
    ON runtime_bindings(session_id);

CREATE TABLE scoped_role_bindings (
    id                TEXT PRIMARY KEY,
    scope             TEXT NOT NULL,
    slot              TEXT NOT NULL,
    revision          INTEGER NOT NULL,
    target_urns_json  TEXT NOT NULL,
    relationship_json TEXT,
    created_by        TEXT,
    created_at        TEXT NOT NULL
);

CREATE INDEX idx_scoped_role_bindings_scope_slot_revision
    ON scoped_role_bindings(scope, slot, revision DESC);

CREATE TABLE workstreams (
    id          TEXT PRIMARY KEY,
    name        TEXT,
    -- workflow_id is free-form and carries no FK: the workflow it names is
    -- owned by whatever orchestrator created it (Torque, Nanite, a caller
    -- outside the portfolio), never by Tether. Tether records the
    -- correlation and does not resolve it -- the same boundary ADR 0041
    -- draws for the registry, which holds identity and a callback and never
    -- operational content.
    --
    -- Consequence: a workflow_id may name a workflow that never existed, or
    -- one since deleted, and nothing here will notice. A reader resolving it
    -- must treat "not found" as ordinary, not as corruption. Filtering
    -- workstreams by workflow_id is therefore a lookup, never a validation.
    workflow_id TEXT,
    status      TEXT NOT NULL DEFAULT 'active'
        CHECK(status IN ('active', 'closed')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX idx_workstreams_workflow ON workstreams(workflow_id);

CREATE INDEX idx_workstreams_status ON workstreams(status);

CREATE INDEX idx_sessions_workstream ON sessions(workstream_id);

CREATE INDEX idx_sessions_ref_attribution ON sessions(ref_attribution);

CREATE INDEX idx_registry_entries_owner
    ON registry_entries(owner);

CREATE TABLE settings (
    scope TEXT NOT NULL,
    scope_id TEXT NOT NULL DEFAULT '',
    key TEXT NOT NULL,
    value_json TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (scope, scope_id, key)
);

CREATE INDEX idx_settings_scope
    ON settings(scope, scope_id);

CREATE TABLE a2a_tasks (
    binding_id TEXT    NOT NULL,
    task_id    TEXT    NOT NULL,
    owner      TEXT    NOT NULL DEFAULT '',
    context_id TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL,
    version    INTEGER NOT NULL,
    task_json  TEXT    NOT NULL,
    created_ns INTEGER NOT NULL,
    updated_ns INTEGER NOT NULL,
    PRIMARY KEY (binding_id, task_id)
);

CREATE INDEX idx_a2a_tasks_state ON a2a_tasks(state);

-- +goose StatementBegin
CREATE TRIGGER registry_instance_insert AFTER INSERT ON registry_entries
BEGIN
    UPDATE registry_entries
    SET tether_instance_id = COALESCE(NEW.tether_instance_id, NEW.mux_instance_id),
        mux_instance_id = COALESCE(NEW.tether_instance_id, NEW.mux_instance_id)
    WHERE urn = NEW.urn;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER registry_instance_update_new
AFTER UPDATE OF tether_instance_id ON registry_entries
WHEN NEW.tether_instance_id IS NOT NULL AND NEW.tether_instance_id != NEW.mux_instance_id
BEGIN
    UPDATE registry_entries SET mux_instance_id = NEW.tether_instance_id WHERE urn = NEW.urn;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER registry_instance_update_previous
AFTER UPDATE OF mux_instance_id ON registry_entries
WHEN NEW.tether_instance_id IS NULL OR NEW.tether_instance_id != NEW.mux_instance_id
BEGIN
    UPDATE registry_entries SET tether_instance_id = NEW.mux_instance_id WHERE urn = NEW.urn;
END;
-- +goose StatementEnd

CREATE TABLE retention_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL,
    table_name TEXT NOT NULL,
    cutoff TEXT NOT NULL,
    removed INTEGER NOT NULL CHECK (removed > 0)
);

CREATE INDEX idx_events_retention_at ON events(at);

CREATE TABLE identity_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL,
    principal_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    mode TEXT NOT NULL,
    authentication TEXT NOT NULL,
    method TEXT NOT NULL,
    route TEXT NOT NULL
, route_pattern TEXT NOT NULL DEFAULT '', required_scope TEXT NOT NULL DEFAULT '', would_allow INTEGER CHECK(would_allow IN (0, 1)), event TEXT NOT NULL DEFAULT '', verified INTEGER NOT NULL DEFAULT 0 CHECK(verified IN (0, 1)));

CREATE INDEX idx_identity_audit_at ON identity_audit(at);

CREATE INDEX idx_a2a_tasks_retention ON a2a_tasks(updated_ns, binding_id, task_id)
WHERE state IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED');

CREATE TABLE message_purge_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL,
    table_name TEXT NOT NULL,
    message_id TEXT NOT NULL,
    authorized_by TEXT NOT NULL
);

CREATE INDEX idx_message_purge_audit_message ON message_purge_audit(message_id, id);

CREATE TABLE session_mcp_policy (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    policy_json TEXT NOT NULL CHECK(json_valid(policy_json))
);

CREATE TABLE channel_publications (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    message_id TEXT NOT NULL UNIQUE REFERENCES messages(id)
);

CREATE INDEX idx_channel_publications_name_seq ON channel_publications(name, seq);

CREATE INDEX idx_messages_routing_pending ON messages(id)
WHERE routing_staged=1 AND payload IS NOT NULL;

CREATE TABLE routing_replies (
    reply_id TEXT PRIMARY KEY REFERENCES messages(id),
    parent_id TEXT NOT NULL,
    original_session_id TEXT NOT NULL,
    target_session_id TEXT NOT NULL,
    delivered_to_session_id TEXT NOT NULL DEFAULT '',
    logical_agent_id TEXT NOT NULL DEFAULT '',
    actor TEXT NOT NULL DEFAULT '',
    interrupt INTEGER NOT NULL DEFAULT 0 CHECK (interrupt IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('pending', 'queued', 'delivering', 'delivered', 'undeliverable')),
    reason TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT,
    idempotency_key TEXT NOT NULL DEFAULT '',
    request_hash TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    settled_at TEXT
);

CREATE INDEX idx_routing_replies_queue ON routing_replies(target_session_id, state, created_at, reply_id);

CREATE INDEX idx_routing_replies_parent ON routing_replies(parent_id);

CREATE UNIQUE INDEX idx_routing_replies_idempotency ON routing_replies(parent_id, actor, idempotency_key) WHERE idempotency_key != '';

CREATE INDEX idx_routing_replies_state ON routing_replies(state, target_session_id);

CREATE TABLE fabric_definition_revisions (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_definition_artifacts (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_actors (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_agents (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_sessions (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_instances (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_binding_heads (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_binding_history (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_admissions (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_migration_candidates (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_import_receipts (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE TABLE fabric_legacy_refs (
    record_key TEXT PRIMARY KEY NOT NULL CHECK (length(record_key) > 0),
    record_version INTEGER NOT NULL CHECK (record_version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

CREATE INDEX fabric_sessions_agent ON fabric_sessions(json_extract(record_json, '$.agent_urn'));

CREATE INDEX fabric_instances_session ON fabric_instances(json_extract(record_json, '$.session_urn'));

CREATE INDEX fabric_binding_history_agent ON fabric_binding_history(json_extract(record_json, '$.lease.agent_urn'));

CREATE TABLE fabric_outbox (
    cursor INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE CHECK (length(event_id) > 0),
    aggregate_urn TEXT NOT NULL CHECK (length(aggregate_urn) > 0),
    event_type TEXT NOT NULL CHECK (length(event_type) > 0),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    created_at TEXT NOT NULL,
    delivered_at TEXT
);

CREATE TABLE team_definitions (
    team_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK(version > 0),
    payload BLOB NOT NULL,
    PRIMARY KEY(team_id, version)
);

CREATE TABLE team_runs (
    run_id TEXT PRIMARY KEY,
    session_group_id TEXT NOT NULL UNIQUE REFERENCES session_groups(id),
    team_id TEXT NOT NULL,
    team_version INTEGER NOT NULL CHECK(team_version > 0),
    payload BLOB NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY(team_id, team_version) REFERENCES team_definitions(team_id, version)
);

CREATE TABLE team_rosters (
    run_id TEXT PRIMARY KEY REFERENCES team_runs(run_id),
    version INTEGER NOT NULL CHECK(version > 0),
    payload BLOB NOT NULL
);

CREATE TABLE team_roster_snapshots (
    run_id TEXT NOT NULL REFERENCES team_runs(run_id),
    version INTEGER NOT NULL CHECK(version > 0),
    payload BLOB NOT NULL,
    PRIMARY KEY(run_id, version)
);

CREATE TABLE team_launch_leases (
    launch_key TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    fence INTEGER NOT NULL CHECK(fence > 0),
    expires_ns INTEGER NOT NULL
);

CREATE TABLE team_launches (
    launch_key TEXT PRIMARY KEY REFERENCES team_launch_leases(launch_key),
    digest TEXT NOT NULL,
    state TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(revision > 0),
    payload BLOB NOT NULL
);

CREATE TABLE team_launch_journal (
    launch_key TEXT NOT NULL REFERENCES team_launches(launch_key),
    revision INTEGER NOT NULL CHECK(revision > 0),
    fence INTEGER NOT NULL CHECK(fence > 0),
    payload BLOB NOT NULL,
    PRIMARY KEY(launch_key, revision)
);

CREATE TABLE team_phase_signals (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES team_runs(run_id),
    phase_id TEXT NOT NULL,
    actor TEXT NOT NULL,
    payload BLOB NOT NULL,
    UNIQUE(run_id, phase_id, actor)
);

CREATE TABLE team_signal_resolutions (
    run_id TEXT NOT NULL REFERENCES team_runs(run_id),
    phase_id TEXT NOT NULL,
    payload BLOB NOT NULL,
    PRIMARY KEY(run_id, phase_id)
);

CREATE TABLE session_shims (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 shim_key TEXT NOT NULL UNIQUE,
 host_backend TEXT NOT NULL CHECK(host_backend IN ('detached','systemd-user')),
 unit_name TEXT NOT NULL DEFAULT '',
 socket_path TEXT NOT NULL,
 descriptor_path TEXT NOT NULL,
 journal_id TEXT NOT NULL DEFAULT '',
 runtime TEXT NOT NULL,
 runtime_generation TEXT NOT NULL,
 boot_generation TEXT NOT NULL,
 controller_epoch TEXT NOT NULL DEFAULT '0',
 last_committed_cursor TEXT NOT NULL DEFAULT '',
 inject_counter TEXT NOT NULL DEFAULT '0',
 host_pid INTEGER NOT NULL DEFAULT 0,
 shim_pid INTEGER NOT NULL DEFAULT 0,
 provider_pid INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);

CREATE TABLE team_host_workflows (
 launch_key TEXT PRIMARY KEY,
 definition BLOB,
 run_id TEXT NOT NULL UNIQUE,
 failed INTEGER NOT NULL DEFAULT 0 CHECK(failed IN (0,1)),
 failure TEXT NOT NULL DEFAULT ''
);

CREATE TABLE team_host_intents (
 sequence INTEGER PRIMARY KEY,
 intent_key TEXT NOT NULL UNIQUE,
 request BLOB NOT NULL,
 enrollment BLOB,
 member BLOB,
 parent_session TEXT NOT NULL DEFAULT '',
 tombstone TEXT NOT NULL DEFAULT '' CHECK(tombstone IN ('','release','retire')),
 cleaned INTEGER NOT NULL DEFAULT 0 CHECK(cleaned IN (0,1)),
 attempts INTEGER NOT NULL DEFAULT 0,
 dead INTEGER NOT NULL DEFAULT 0 CHECK(dead IN (0,1)),
 next_attempt_at INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE team_host_bindings (
 actor TEXT PRIMARY KEY,
 intent_key TEXT NOT NULL UNIQUE REFERENCES team_host_intents(intent_key)
);

CREATE TABLE team_host_messages (
 message_key TEXT PRIMARY KEY,
 digest TEXT NOT NULL
);

CREATE TABLE team_host_deliveries (
 sequence INTEGER PRIMARY KEY,
 delivery_key TEXT NOT NULL UNIQUE,
 payload BLOB NOT NULL,
 dispatched INTEGER NOT NULL DEFAULT 0 CHECK(dispatched IN (0,1)),
 attempts INTEGER NOT NULL DEFAULT 0,
 dead INTEGER NOT NULL DEFAULT 0 CHECK(dead IN (0,1)),
 next_attempt_at INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT ''
);

CREATE INDEX team_host_delivery_pending ON team_host_deliveries(next_attempt_at,sequence) WHERE dispatched=0 AND dead=0;

CREATE TABLE team_host_delegations (
 delivery_key TEXT PRIMARY KEY REFERENCES team_host_deliveries(delivery_key),
 state TEXT NOT NULL
);

CREATE TABLE team_host_routing (
 run_id TEXT PRIMARY KEY REFERENCES team_runs(run_id),
 install_key TEXT NOT NULL UNIQUE,
 payload BLOB NOT NULL,
 channel TEXT NOT NULL,
 removed INTEGER NOT NULL DEFAULT 0 CHECK(removed IN (0,1))
);

CREATE TABLE team_host_approvals (
 request_key TEXT PRIMARY KEY,
 payload BLOB NOT NULL
);

CREATE TABLE team_host_calls (
 principal TEXT NOT NULL,
 verb TEXT NOT NULL,
 call_key TEXT NOT NULL,
 digest TEXT NOT NULL,
 request BLOB NOT NULL,
 plan BLOB,
 result BLOB,
 PRIMARY KEY(principal,verb,call_key)
);

CREATE INDEX team_host_cleanup_pending ON team_host_intents(next_attempt_at,sequence) WHERE tombstone<>'' AND cleaned=0 AND dead=0;

CREATE INDEX team_host_intent_member ON team_host_intents(json_extract(request,'$.MemberID'));

CREATE INDEX team_host_recorded_member ON team_host_intents(json_extract(member,'$.id'));

CREATE TABLE team_host_recovery_cursors (kind TEXT PRIMARY KEY, position INTEGER NOT NULL);

CREATE TABLE team_host_launch_intents (
 intent_key TEXT PRIMARY KEY,
 launch_key TEXT NOT NULL REFERENCES team_launches(launch_key)
);

CREATE INDEX team_host_recipient_pending ON team_host_deliveries(json_extract(payload,'$.Recipient.actor'),json_extract(payload,'$.Recipient.session_id'),sequence) WHERE dispatched=0 AND dead=0;

CREATE TABLE team_port_intents (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 port_kind TEXT NOT NULL CHECK(port_kind IN ('enrollment','session','delivery')),
 intent_key TEXT NOT NULL,
 nonce TEXT NOT NULL DEFAULT '',
 binding_secret TEXT NOT NULL DEFAULT '',
 acquired_urn TEXT NOT NULL DEFAULT '',
 request BLOB,
 payload BLOB,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','done','cleaned','failed')),
 ended TEXT NOT NULL DEFAULT '' CHECK(ended IN ('','release','retire','stop')),
 binding_ended INTEGER NOT NULL DEFAULT 0 CHECK(binding_ended IN (0,1)),
 UNIQUE(port_kind,intent_key)
);

CREATE INDEX team_port_pending ON team_port_intents(sequence) WHERE port_kind<>'delivery' AND state IN ('pending','done');

CREATE TABLE codex_shim_protocol (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES session_shims(session_id),
    shim_key TEXT NOT NULL,
    protocol TEXT NOT NULL,
    revision TEXT NOT NULL,
    state_json TEXT NOT NULL
);

CREATE TABLE session_shim_retirements (
    operation_id TEXT PRIMARY KEY NOT NULL CHECK(length(operation_id)>0),
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    request_digest TEXT NOT NULL CHECK(length(request_digest)>0),
    revision TEXT NOT NULL CHECK(length(revision)>0),
    phase TEXT NOT NULL CHECK(phase IN ('intent_recorded','retirement_committed','descriptor_cleanup_complete','state_reconciled')),
    record_json TEXT NOT NULL CHECK(json_valid(record_json) AND length(CAST(record_json AS BLOB))<=262144),
    retired_at TEXT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_session_shim_retirements_session
ON session_shim_retirements(session_id,retired_at,operation_id);

-- +goose StatementBegin
CREATE TRIGGER session_retirement_preserve_audit
BEFORE DELETE ON sessions
WHEN EXISTS(SELECT 1 FROM session_shim_retirements WHERE session_id=OLD.id)
BEGIN
    SELECT RAISE(ABORT,'session retirement audit retained');
END;
-- +goose StatementEnd

CREATE TABLE session_shim_retention_cursors (
    cursor_id TEXT PRIMARY KEY NOT NULL,
    request_digest TEXT NOT NULL,
    revision TEXT NOT NULL,
    record_json TEXT NOT NULL CHECK(json_valid(record_json) AND length(CAST(record_json AS BLOB))<=262144),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE message_aliases (
    urn TEXT PRIMARY KEY NOT NULL,
    alias TEXT UNIQUE COLLATE NOCASE NOT NULL
);

CREATE TABLE agent_recovery_cursors (
    agent_id TEXT NOT NULL,
    channel TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK(sequence >= 0),
    PRIMARY KEY(agent_id, channel)
);

CREATE TABLE team_gone_shim_recoveries (
 shim_key TEXT PRIMARY KEY NOT NULL,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
 binding_id TEXT NOT NULL,
 actor_uri TEXT NOT NULL,
 binding_generation INTEGER NOT NULL,
 intent_key TEXT NOT NULL,
 custody_json TEXT NOT NULL CHECK(json_valid(custody_json)),
 state TEXT NOT NULL CHECK(state='recovery_pending'),
 confirmed_at TEXT NOT NULL
);

CREATE INDEX team_gone_shim_recoveries_session ON team_gone_shim_recoveries(session_id);

-- +goose StatementBegin
CREATE TRIGGER team_gone_shim_recovery_preserve_audit BEFORE DELETE ON sessions
WHEN EXISTS(SELECT 1 FROM team_gone_shim_recoveries WHERE session_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'team shim recovery audit retained'); END;
-- +goose StatementEnd

CREATE TABLE session_replacements (
    source_session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE RESTRICT,
    replacement_session_id TEXT NOT NULL UNIQUE REFERENCES sessions(id) ON DELETE RESTRICT,
    actor_uri TEXT NOT NULL,
    intent_key TEXT NOT NULL,
    binding_id TEXT NOT NULL,
    binding_generation INTEGER NOT NULL,
    source_plan_digest TEXT NOT NULL,
    credential_scopes_json TEXT NOT NULL CHECK(json_valid(credential_scopes_json) AND json_type(credential_scopes_json)='array'),
    credential_expires_at TEXT,
    credential_mode TEXT NOT NULL CHECK(credential_mode IN ('off','observe','enforce')),
    source_protocol_revision TEXT,
    source_protocol_digest TEXT,
    custody_json TEXT,
    obligation_proof_ref TEXT,
    committed_at TEXT NOT NULL,
    CHECK(source_session_id <> replacement_session_id)
);

-- +goose StatementBegin
CREATE TRIGGER replacement_lineage_update BEFORE UPDATE ON session_replacements
BEGIN SELECT RAISE(ABORT,'replacement lineage is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replacement_lineage_delete BEFORE DELETE ON session_replacements
BEGIN SELECT RAISE(ABORT,'replacement lineage is retained'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_codex_protocol_update BEFORE UPDATE ON codex_shim_protocol
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced protocol is frozen'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_codex_protocol_delete BEFORE DELETE ON codex_shim_protocol
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced protocol history is retained'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_codex_protocol_insert BEFORE INSERT ON codex_shim_protocol
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=NEW.session_id)
BEGIN SELECT RAISE(ABORT,'replaced protocol cannot be reinitialized'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_shim_insert BEFORE INSERT ON session_shims
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=NEW.session_id)
BEGIN SELECT RAISE(ABORT,'replaced execution cannot reacquire placement'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_shim_update BEFORE UPDATE ON session_shims
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced custody is frozen'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_session_admission BEFORE UPDATE OF state,pid ON sessions
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.id)
 AND (NEW.state <> OLD.state OR COALESCE(NEW.pid,0) <> COALESCE(OLD.pid,0))
BEGIN SELECT RAISE(ABORT,'replaced session cannot regain runtime ownership'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replacement_preserve_sessions BEFORE DELETE ON sessions
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.id OR replacement_session_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'replacement lineage is retained'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_launch_plan_update BEFORE UPDATE ON launch_plans
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced launch plan is frozen'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_launch_plan_delete BEFORE DELETE ON launch_plans
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced launch plan is retained'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_native_mapping_update BEFORE UPDATE ON session_provider_mappings
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced native mapping is frozen'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_native_mapping_insert BEFORE INSERT ON session_provider_mappings
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=NEW.session_id)
BEGIN SELECT RAISE(ABORT,'replaced native mapping is frozen'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_native_mapping_delete BEFORE DELETE ON session_provider_mappings
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced native mapping is retained'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replaced_session_output BEFORE INSERT ON messages
WHEN NEW.routing_staged=1 AND EXISTS(
 SELECT 1 FROM session_replacements WHERE NEW.from_urn='msg://session/local/'||source_session_id)
BEGIN SELECT RAISE(ABORT,'replaced session output is fenced'); END;
-- +goose StatementEnd

CREATE TABLE recipient_wake_admissions (
    message_id TEXT PRIMARY KEY NOT NULL,
    admitted_at TEXT NOT NULL,
    outcome_json TEXT
);

CREATE TABLE environment_request_status (
 session_id TEXT NOT NULL,
 turn_id TEXT NOT NULL,
 request_id TEXT NOT NULL,
 request_kind TEXT NOT NULL CHECK(request_kind IN ('question','approval')),
 is_open INTEGER NOT NULL,
 event_seq INTEGER NOT NULL,
 source_sequence TEXT NOT NULL,
 PRIMARY KEY(session_id,turn_id,request_id)
);

CREATE TABLE workspace_scratch_allocations (
    session_id TEXT PRIMARY KEY,
    operation_id TEXT NOT NULL,
    session_digest TEXT NOT NULL,
    plan_digest TEXT NOT NULL,
    root TEXT NOT NULL,
    physical_identity TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('reserved', 'admitted'))
);

CREATE TABLE "principals" (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('operator', 'session', 'service', 'interactive', 'device')),
    display TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE CHECK(length(token_hash) = 64),
    scopes_json TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    addresses_json TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    expires_at TEXT,
    last_used_at TEXT,
    last_remote_addr TEXT NOT NULL DEFAULT '',
    last_user_agent TEXT NOT NULL DEFAULT '', launch_attribution TEXT NOT NULL DEFAULT '',
    CHECK(kind <> 'device' OR (expires_at IS NOT NULL AND principal_id <> 'msg://user/local/operator' AND session_id = ''))
);

CREATE INDEX idx_principals_identity ON principals(principal_id);

CREATE INDEX idx_principals_session ON principals(session_id);

CREATE UNIQUE INDEX principals_active_session ON principals(session_id) WHERE kind='session' AND revoked_at IS NULL;

CREATE UNIQUE INDEX principals_device_identity ON principals(principal_id) WHERE kind='device';

-- +goose StatementBegin
CREATE TRIGGER session_principal_requires_active_session BEFORE INSERT ON principals
WHEN NEW.kind = 'session' AND NOT EXISTS (
 SELECT 1 FROM sessions WHERE id = NEW.session_id AND state NOT IN ('completed', 'failed', 'killed')
)
BEGIN
 SELECT RAISE(ABORT, 'session principal requires an active session');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER session_terminal_revokes_credentials AFTER UPDATE OF state ON sessions
WHEN NEW.state IN ('completed', 'failed', 'killed')
BEGIN
 UPDATE principals SET revoked_at = COALESCE(revoked_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
 WHERE kind = 'session' AND session_id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER session_delete_revokes_credentials AFTER DELETE ON sessions
BEGIN
 UPDATE principals SET revoked_at = COALESCE(revoked_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
 WHERE kind = 'session' AND session_id = OLD.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replacement_principal_authority BEFORE INSERT ON principals
WHEN NEW.kind='session' AND EXISTS(SELECT 1 FROM session_replacements WHERE replacement_session_id=NEW.session_id)
BEGIN
 SELECT CASE WHEN EXISTS(SELECT 1 FROM session_replacements WHERE replacement_session_id=NEW.session_id AND credential_mode='off')
 THEN RAISE(ABORT,'replacement identity remains off') END;
 SELECT CASE WHEN NOT EXISTS(
  SELECT 1 FROM session_replacements x
  JOIN runtime_bindings b ON b.id=x.binding_id AND b.target_urn=x.actor_uri AND b.generation=x.binding_generation
  JOIN team_host_intents h ON h.intent_key=x.intent_key
  JOIN team_port_intents p ON p.port_kind='session' AND p.intent_key=h.intent_key
  JOIN team_port_intents e ON e.port_kind='enrollment' AND e.intent_key=h.intent_key
  JOIN registry_entries profile ON profile.urn=b.target_urn
  JOIN team_rosters r ON r.run_id=COALESCE(NULLIF(json_extract(h.request,'$.RunID'),''),(SELECT json_extract(l.payload,'$.Run.id') FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key=h.intent_key))
  JOIN team_runs t ON t.run_id=r.run_id
  WHERE x.replacement_session_id=NEW.session_id
   AND b.session_id=NEW.session_id AND b.host_id='team' AND b.visibility='tether-hosted' AND b.revoked_at IS NULL
   AND (b.lease_expires_at IS NULL OR julianday(b.lease_expires_at)>julianday('now'))
   AND b.generation=(SELECT MAX(generation) FROM runtime_bindings WHERE target_urn=b.target_urn)
   AND h.tombstone='' AND h.cleaned=0 AND h.dead=0
   AND p.state='done' AND p.ended='' AND CAST(p.payload AS TEXT)=json_quote(NEW.session_id)
   AND e.state='done' AND e.ended='' AND e.binding_ended=0 AND e.binding_secret=b.attempt_id AND e.acquired_urn=b.target_urn
   AND profile.status='active' AND profile.kind='agent'
   AND json_extract(e.payload,'$.Enrollment.actor')=b.target_urn AND json_extract(e.payload,'$.Enrollment.agent_id')=b.target_urn
   AND json_extract(json_extract(profile.props_json,'$.team_definition'),'$.id')=json_extract(e.payload,'$.Pin.id')
   AND json_extract(json_extract(profile.props_json,'$.team_definition'),'$.revision')=json_extract(e.payload,'$.Pin.revision')
   AND COALESCE(json_extract(json_extract(profile.props_json,'$.team_definition'),'$.digest'),'')=COALESCE(json_extract(e.payload,'$.Pin.digest'),'')
   AND json_extract(h.member,'$.session_id')=NEW.session_id AND json_extract(h.member,'$.actor')=b.target_urn
   AND json_extract(h.member,'$.status')='active' AND json_extract(h.member,'$.enrolled')=1
   AND json_extract(t.payload,'$.status') NOT IN ('completed','failed','canceled','rejected')
   AND EXISTS(SELECT 1 FROM json_each(r.payload,'$.members') m WHERE json_extract(m.value,'$.session_id')=NEW.session_id AND json_extract(m.value,'$.actor')=b.target_urn AND json_extract(m.value,'$.status')='active')
 ) THEN RAISE(ABORT,'replacement retained authority unavailable') END;
 SELECT CASE WHEN EXISTS(
  SELECT 1 FROM session_replacements x WHERE x.replacement_session_id=NEW.session_id
   AND (json_type(NEW.scopes_json)<>'array'
    OR (NOT EXISTS(SELECT 1 FROM json_each(x.credential_scopes_json) WHERE value='*')
     AND EXISTS(SELECT 1 FROM json_each(NEW.scopes_json) n WHERE NOT EXISTS(SELECT 1 FROM json_each(x.credential_scopes_json) ceiling WHERE ceiling.value=n.value)))
    OR (x.credential_expires_at IS NOT NULL AND (NEW.expires_at IS NULL OR julianday(NEW.expires_at)>julianday(x.credential_expires_at) OR julianday(x.credential_expires_at)<=julianday('now'))))
 ) THEN RAISE(ABORT,'replacement execution credential exceeds inherited ceiling') END;
END;
-- +goose StatementEnd

CREATE TABLE pairing_grants (
 id TEXT PRIMARY KEY,
 code_hash TEXT NOT NULL UNIQUE CHECK(length(code_hash)=64),
 label TEXT NOT NULL,
 scopes_json TEXT NOT NULL CHECK(json_valid(scopes_json) AND json_type(scopes_json)='array'),
 created_by TEXT NOT NULL,
 created_at TEXT NOT NULL,
 expires_at TEXT NOT NULL,
 consumed_at TEXT,
 revoked_at TEXT,
 key_thumbprint TEXT NOT NULL DEFAULT ''
);

CREATE INDEX pairing_grants_expiry ON pairing_grants(expires_at);

CREATE TABLE "session_idempotency" (
    principal_id               TEXT NOT NULL,
    key                        TEXT NOT NULL,
    operation                  TEXT NOT NULL,
    request_digest             TEXT NOT NULL,
    session_id                 TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    resolved_parent_session_id TEXT,
    created_at                 TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (principal_id, key)
);

CREATE INDEX idx_session_idempotency_session ON session_idempotency(session_id);

CREATE TABLE workers (
    id TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    device_id TEXT NOT NULL UNIQUE,
    platform TEXT NOT NULL,
    caps_json TEXT NOT NULL DEFAULT '{}',
    last_seen TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE session_worker_seq (
    session_id TEXT PRIMARY KEY,
    last_seq INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE worker_mail_grants (
    worker_id TEXT NOT NULL REFERENCES workers(id),
    urn TEXT NOT NULL,
    PRIMARY KEY (worker_id, urn)
);

CREATE INDEX idx_events_session_kind_at ON events(session_id, kind, at);

CREATE TABLE forum_categories (id TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', archived INTEGER NOT NULL DEFAULT 0, owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, sort_order INTEGER NOT NULL DEFAULT 0);

CREATE INDEX forum_categories_namespace ON forum_categories(namespace,id);

CREATE TABLE forums (
 category_id TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', archived INTEGER NOT NULL DEFAULT 0,
 id TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL,
 owner_type TEXT NOT NULL, owner_id TEXT NOT NULL,
 config_json TEXT NOT NULL DEFAULT '{}', policy_json TEXT NOT NULL DEFAULT '{"mode":"owner-only"}', metadata_json TEXT NOT NULL DEFAULT '{}',
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_seq INTEGER NOT NULL DEFAULT 0
, sort_order INTEGER NOT NULL DEFAULT 0);

CREATE TABLE forum_topics (
 source_topic_id TEXT NOT NULL DEFAULT '', archived INTEGER NOT NULL DEFAULT 0,
 id TEXT PRIMARY KEY, forum_id TEXT NOT NULL REFERENCES forums(id), title TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','resolved','archived')),
 author_id TEXT NOT NULL, author_kind TEXT NOT NULL,
 post_count INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, last_activity_at TEXT NOT NULL, last_seq INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX forum_topics_scope ON forum_topics(forum_id,id);

CREATE TABLE forum_posts (
 subject TEXT NOT NULL DEFAULT '', attachments_json TEXT NOT NULL DEFAULT '[]', source_post_id TEXT NOT NULL DEFAULT '',
 id TEXT PRIMARY KEY, forum_id TEXT NOT NULL REFERENCES forums(id), topic_id TEXT NOT NULL REFERENCES forum_topics(id),
 parent_post_id TEXT NOT NULL DEFAULT '', seq INTEGER NOT NULL, body_md TEXT NOT NULL, body_chars INTEGER NOT NULL,
 author_id TEXT NOT NULL, author_kind TEXT NOT NULL, verified INTEGER NOT NULL,
 session_id TEXT NOT NULL DEFAULT '', agent_urn TEXT NOT NULL DEFAULT '', workstream_id TEXT NOT NULL DEFAULT '', project_id TEXT NOT NULL DEFAULT '', launch_id TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, edited_at TEXT NOT NULL DEFAULT '', depth INTEGER NOT NULL DEFAULT 0, reply_count INTEGER NOT NULL DEFAULT 0,
 UNIQUE(forum_id,seq)
);

CREATE INDEX forum_posts_topic_seq ON forum_posts(topic_id,seq);

CREATE INDEX forum_posts_parent ON forum_posts(parent_post_id);

CREATE INDEX forum_posts_workstream ON forum_posts(forum_id,workstream_id,seq);

CREATE INDEX forum_posts_project ON forum_posts(forum_id,project_id,seq);

CREATE VIRTUAL TABLE forum_post_fts USING fts5(forum_id UNINDEXED, post_id UNINDEXED, topic_title, body_md);

CREATE TABLE forum_participants (
 forum_id TEXT NOT NULL REFERENCES forums(id),
 principal_kind TEXT NOT NULL CHECK(principal_kind IN ('user','agent')),
 principal_id TEXT NOT NULL,
 access TEXT NOT NULL CHECK(access IN ('read','post')),
 PRIMARY KEY (forum_id,principal_kind,principal_id)
);

CREATE INDEX forum_participants_principal ON forum_participants(principal_kind,principal_id,forum_id);

CREATE INDEX forums_category ON forums(namespace,category_id,id);

CREATE INDEX forum_categories_sort ON forum_categories(namespace,sort_order,id);

CREATE INDEX forums_category_sort ON forums(namespace,category_id,sort_order,id);

CREATE TABLE execution_policy (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  policy_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE session_usage (
  session_id TEXT PRIMARY KEY,
  turns INTEGER NOT NULL DEFAULT 0,
  failed_turns INTEGER NOT NULL DEFAULT 0,
  usage_turns INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  turn_ms INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);

CREATE TABLE "session_refs" (
    id INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    ref_id TEXT NOT NULL,
    uri TEXT,
    relation TEXT NOT NULL CHECK(relation IN ('created', 'updated', 'read', 'referenced')),
    source TEXT NOT NULL CHECK(source IN ('proxy', 'proxy_claimed', 'session', 'operator', 'api', 'agent')),
    at TEXT NOT NULL,
    parent_item_id TEXT,
    UNIQUE(session_id, kind, ref_id, relation)
);

CREATE INDEX idx_session_refs_session ON session_refs(session_id);

CREATE INDEX idx_session_refs_lookup ON session_refs(kind, ref_id);

CREATE INDEX idx_session_refs_parent_item ON session_refs(parent_item_id);

CREATE UNIQUE INDEX idx_events_event_id ON events(event_id) WHERE event_id IS NOT NULL;

CREATE INDEX idx_events_pending ON events(id) WHERE pending = 1;

CREATE INDEX idx_events_scope_project ON events(scope_project_id, id) WHERE scope_project_id IS NOT NULL;

CREATE INDEX idx_events_scope_task ON events(scope_task_id, id) WHERE scope_task_id IS NOT NULL;

CREATE INDEX idx_events_scope_logical_agent ON events(scope_logical_agent_id, id) WHERE scope_logical_agent_id IS NOT NULL;

CREATE INDEX idx_events_retention_class ON events(retention_class, id) WHERE retention_class IS NOT NULL;

CREATE TABLE event_retention_policies (
    id TEXT PRIMARY KEY,
    policy_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- +goose StatementBegin
CREATE TRIGGER event_retention_policies_immutable
BEFORE UPDATE ON event_retention_policies
BEGIN
    SELECT RAISE(ABORT, 'retention policy snapshots are immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER event_retention_policies_no_delete
BEFORE DELETE ON event_retention_policies
BEGIN
    SELECT RAISE(ABORT, 'retention policy snapshots are permanent');
END;
-- +goose StatementEnd

CREATE TABLE event_retention_admission (
    kind TEXT PRIMARY KEY,
    retention_class TEXT NOT NULL CHECK (retention_class IN ('R-audit','R-state','R-activity','R-debug')),
    policy_id TEXT NOT NULL REFERENCES event_retention_policies(id)
);

CREATE TABLE team_host_adoptions (
 intent_key TEXT PRIMARY KEY REFERENCES team_host_intents(intent_key),
 run_id TEXT NOT NULL,
 actor TEXT NOT NULL,
 session_id TEXT NOT NULL,
 binding_generation INTEGER NOT NULL CHECK(binding_generation > 0),
 launch_id TEXT NOT NULL,
 definition_id TEXT NOT NULL,
 definition_revision TEXT NOT NULL,
 definition_digest TEXT NOT NULL,
 principal TEXT NOT NULL,
 suspended INTEGER NOT NULL DEFAULT 0 CHECK(suspended IN (0,1)),
 released INTEGER NOT NULL DEFAULT 0 CHECK(released IN (0,1))
);

CREATE UNIQUE INDEX team_host_adoption_active_actor ON team_host_adoptions(actor) WHERE released=0;

CREATE UNIQUE INDEX team_host_adoption_active_session ON team_host_adoptions(session_id) WHERE released=0;

CREATE INDEX team_host_adoption_run ON team_host_adoptions(run_id) WHERE released=0;

CREATE TABLE team_recovery_attempts (
 kind TEXT NOT NULL CHECK(kind IN ('session','enrollment','formation')),
 intent_key TEXT NOT NULL,
 mode TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 7),
 next_attempt_at INTEGER NOT NULL DEFAULT 0,
 error_hash TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(kind,intent_key)
);

CREATE TABLE event_identity (
 singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
 environment_id TEXT NOT NULL,
 cutover INTEGER NOT NULL CHECK (cutover >= 0),
 next_legacy_id INTEGER NOT NULL CHECK (next_legacy_id >= 0),
 remaining INTEGER NOT NULL CHECK (remaining >= 0),
 backfilled INTEGER NOT NULL DEFAULT 0 CHECK (backfilled >= 0),
 failed INTEGER NOT NULL DEFAULT 0 CHECK (failed >= 0)
);

CREATE TABLE team_launch_grants (
 intent_key TEXT PRIMARY KEY,
 principal_id TEXT NOT NULL,
 scopes_json TEXT NOT NULL,
 recorded_at TEXT NOT NULL
);

CREATE TABLE workstream_projects (
 workstream_id TEXT NOT NULL REFERENCES workstreams(id),
 project_urn TEXT NOT NULL REFERENCES registry_entries(urn),
 PRIMARY KEY (workstream_id, project_urn)
);

CREATE INDEX idx_workstream_projects_project ON workstream_projects(project_urn, workstream_id);

CREATE TABLE message_event_journal (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 kind TEXT NOT NULL,
 at TEXT NOT NULL,
 payload_json TEXT NOT NULL,
 team_run_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE message_event_deferred (transition_id INTEGER PRIMARY KEY);

CREATE TABLE message_event_checkpoints (
 source TEXT PRIMARY KEY,
 position INTEGER NOT NULL DEFAULT 0
);

-- +goose StatementBegin
CREATE TRIGGER message_events_untracked_sent AFTER INSERT ON messages
 WHEN NEW.delivery_id IS NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.sent',NEW.created_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','sent','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_events_delivered AFTER UPDATE OF delivered_at ON messages
 WHEN OLD.delivered_at IS NULL AND NEW.delivered_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.delivered',NEW.delivered_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','delivered','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_events_read AFTER UPDATE OF read_at ON messages
 WHEN OLD.read_at IS NULL AND NEW.read_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.read',NEW.read_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','read','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_events_consumed AFTER UPDATE OF consumed_at ON messages
 WHEN OLD.consumed_at IS NULL AND NEW.consumed_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.consumed',NEW.consumed_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','consumed','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_events_archived AFTER UPDATE OF archived_at ON messages
 WHEN OLD.archived_at IS NULL AND NEW.archived_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.archived',NEW.archived_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','archived','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_events_canceled AFTER UPDATE OF canceled_at ON messages
 WHEN OLD.canceled_at IS NULL AND NEW.canceled_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.canceled',NEW.canceled_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','canceled','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_events_purged AFTER UPDATE OF payload,metadata ON messages
 WHEN (OLD.payload IS NOT NULL OR OLD.metadata IS NOT NULL) AND NEW.payload IS NULL AND NEW.metadata IS NULL AND OLD.routing_staged=0 AND OLD.kind NOT LIKE 'stream.%' AND coalesce(OLD.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.purged',strftime('%Y-%m-%dT%H:%M:%fZ','now'),json_object('message_id',OLD.id,'from',OLD.from_urn,'to',json_array(OLD.to_urn),
 'thread_id',coalesce(OLD.thread_id,''),'in_reply_to',coalesce(OLD.in_reply_to,''),
 'msg_kind',OLD.kind,'body_ref','/messages/' || OLD.id,
 'body_bytes',length(CAST(coalesce(OLD.payload,'') AS BLOB)), 'content_type',coalesce(OLD.content_type,''),
 'urgency',CASE WHEN json_valid(OLD.metadata) AND json_extract(OLD.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(OLD.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','purged','delivery_id',coalesce(OLD.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(OLD.metadata) THEN json_extract(OLD.metadata,'$.team_run') END),''));
 END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_events_group_read AFTER UPDATE OF last_read_seq ON group_members
WHEN NEW.last_read_seq > OLD.last_read_seq
BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json)
 SELECT 'message.read',strftime('%Y-%m-%dT%H:%M:%fZ','now'),
 json_object('message_id',id,'from',from_urn,'to',json_array(NEW.member_urn),
 'thread_id',coalesce(thread_id,''),'msg_kind',kind,'group',group_urn,'group_seq',group_seq,
 'body_ref','/groups/' || group_urn || '/messages?message_id=' || id,
 'body_bytes',length(CAST(coalesce(payload,'') AS BLOB)), 'content_type',coalesce(content_type,''),
 'delivery',json_object('state','read'))
 FROM messages WHERE group_urn=NEW.grp_urn AND group_seq>OLD.last_read_seq AND group_seq<=NEW.last_read_seq AND kind NOT LIKE 'stream.%';
END;
-- +goose StatementEnd

CREATE TABLE worker_permission_parser (
 session_id TEXT PRIMARY KEY,
 last_seq INTEGER NOT NULL,
 spans_json TEXT NOT NULL,
 discard INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE environment_request_resolutions (
 session_id TEXT NOT NULL, turn_id TEXT NOT NULL, request_id TEXT NOT NULL,
 source_sequence TEXT NOT NULL,
 PRIMARY KEY(session_id,turn_id,request_id)
);

CREATE TABLE environment_request_turn_ends (
 session_id TEXT NOT NULL, turn_id TEXT NOT NULL, source_sequence TEXT NOT NULL,
 PRIMARY KEY(session_id,turn_id)
);

-- +goose StatementBegin
CREATE TRIGGER events_require_id_after_seed
BEFORE INSERT ON events
WHEN (NEW.event_id IS NULL OR NEW.event_id = '')
 AND EXISTS (SELECT 1 FROM event_identity WHERE singleton = 1)
BEGIN
 SELECT RAISE(ABORT, 'event_id_required_after_cutover');
END;
-- +goose StatementEnd

CREATE TABLE team_activation_evidence (
 kind TEXT NOT NULL,
 evidence_key TEXT NOT NULL,
 payload TEXT NOT NULL,
 recorded_at TEXT NOT NULL,
 PRIMARY KEY(kind,evidence_key)
);

CREATE TABLE event_ingest_digests (
    event_id TEXT PRIMARY KEY,
    digest   TEXT NOT NULL,
    app      TEXT NOT NULL,
    at       TEXT NOT NULL
);

CREATE INDEX idx_event_ingest_digests_at ON event_ingest_digests(at);

CREATE TABLE environment_work_state (
    session_id TEXT PRIMARY KEY,
    value      TEXT NOT NULL CHECK (value IN ('idle', 'processing')),
    since      TEXT NOT NULL,
    event_seq  INTEGER NOT NULL
);

CREATE TABLE turn_inputs (
    input_id       TEXT PRIMARY KEY,
    session_id     TEXT NOT NULL,
    seq            INTEGER NOT NULL,
    source         TEXT NOT NULL CHECK (source IN ('chat_send','session_turn','mcp_send_turn','mail_wake','routing_reply','system')),
    sender_urn     TEXT,
    sender_class   TEXT NOT NULL CHECK (sender_class IN ('operator','agent','system','unknown')),
    basis          TEXT NOT NULL CHECK (basis IN ('authenticated_principal','stamped_message_sender','shared_operator_credential','system_injection','none')),
    ref_id         TEXT,
    state          TEXT NOT NULL CHECK (state IN ('submitting','accepted','failed')),
    queued         INTEGER NOT NULL DEFAULT 0,
    submitted_at   TEXT NOT NULL,
    accepted_at    TEXT,
    turn_id        TEXT,
    audience       TEXT,
    audience_basis TEXT,
    classes        TEXT,
    UNIQUE (session_id, seq)
);

CREATE INDEX idx_turn_inputs_unbound ON turn_inputs(session_id, seq) WHERE turn_id IS NULL AND state IN ('submitting','accepted');

CREATE INDEX idx_turn_inputs_turn ON turn_inputs(session_id, turn_id) WHERE turn_id IS NOT NULL;

CREATE INDEX idx_turn_inputs_submitted ON turn_inputs(submitted_at);

CREATE INDEX idx_events_scope_cell ON events(scope_cell, id) WHERE scope_cell IS NOT NULL;

CREATE TABLE seat_status (
    agent_urn  TEXT PRIMARY KEY,
    state      TEXT NOT NULL CHECK (state IN ('available','working','waiting-decision','waiting-agent','blocked','paused','offline')),
    reason     TEXT NOT NULL DEFAULT '',
    ref        TEXT NOT NULL DEFAULT '',
    owner      TEXT NOT NULL DEFAULT '',
    since      TEXT NOT NULL,
    expires_at TEXT,
    set_by     TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (state <> 'blocked' OR owner <> '')
);

CREATE TABLE wake_triage (
    message_id   TEXT PRIMARY KEY,
    recipient    TEXT NOT NULL,
    seat         TEXT NOT NULL,
    class        TEXT NOT NULL CHECK (class IN ('human','action','fyi','read')),
    reason       TEXT NOT NULL,
    typed        INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','woken','covered','surfaced','settled')),
    not_before   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE INDEX idx_wake_triage_seat_state ON wake_triage(seat, state, class);

CREATE TABLE seat_wake_clock (
    seat                TEXT PRIMARY KEY,
    last_action_wake_at TEXT NOT NULL
);

-- Seed rows a fresh old database holds.
INSERT INTO message_event_checkpoints (source, position) VALUES ('attention', 0);
INSERT INTO message_event_checkpoints (source, position) VALUES ('delivery', -1);
INSERT INTO message_event_checkpoints (source, position) VALUES ('retry', 0);
