-- Inert team host state; no enrollment, session or transport backfill.
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
-- Backfill legacy launch-intent lookup; subsequent entries are written by the
-- host in the same Go transaction as the launch record.
CREATE TABLE team_host_launch_intents (
 intent_key TEXT PRIMARY KEY,
 launch_key TEXT NOT NULL REFERENCES team_launches(launch_key)
);
INSERT OR IGNORE INTO team_host_launch_intents SELECT json_extract(CASE WHEN json_valid(value) THEN value ELSE '{}' END,'$.Key'),launch_key FROM team_launches,json_each(CASE WHEN json_valid(payload) THEN payload ELSE '{}' END,'$.Intents') WHERE json_valid(payload) AND json_valid(value) AND json_extract(CASE WHEN json_valid(value) THEN value ELSE '{}' END,'$.Key') IS NOT NULL;
CREATE INDEX team_host_recipient_pending ON team_host_deliveries(json_extract(payload,'$.Recipient.actor'),json_extract(payload,'$.Recipient.session_id'),sequence) WHERE dispatched=0 AND dead=0;
