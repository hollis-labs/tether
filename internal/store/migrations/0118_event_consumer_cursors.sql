-- CW-20261121-0154: durable read positions for in-daemon event consumers (the
-- Ops status emitter first). One row per consumer; sequence only advances.
CREATE TABLE event_consumer_cursors (
    consumer   TEXT PRIMARY KEY,
    sequence   INTEGER NOT NULL,
    updated_at TEXT NOT NULL
);

-- CW-20261121-0154: one row per reminder the Ops status reflex sent a session,
-- so each (session, task, kind) is nudged at most once, across restarts.
CREATE TABLE ops_status_nudges (
    session_id TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    nudged_at  TEXT NOT NULL,
    PRIMARY KEY (session_id, task_id, kind)
);
