-- Current runtime work-state per session (CW-20261021-0003).
-- session.work_state_changed is recorded only when the value changes, and event
-- retention can purge the last one of a long-idle session. The current value and
-- when it began therefore live here, upserted in the SAME transaction as the
-- event (the environment_request_status pattern), so the environment snapshot
-- reads a row, never the log. Additive and forward-only: nothing is rewritten and
-- there is no backfill (a session with no row reports an unknown work-state until
-- its next worker frame). An older binary never references this table and keeps
-- working against a database that has it.
CREATE TABLE IF NOT EXISTS environment_work_state (
    session_id TEXT PRIMARY KEY,
    value      TEXT NOT NULL CHECK (value IN ('idle', 'processing')),
    since      TEXT NOT NULL,
    event_seq  INTEGER NOT NULL
);
