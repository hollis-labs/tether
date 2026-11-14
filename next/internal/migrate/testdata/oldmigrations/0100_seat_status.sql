-- CW-20261114-0013: a seat's declared status (tether_status_set), one row per
-- agent URN. Live facts are computed at read time, never stored here.
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
