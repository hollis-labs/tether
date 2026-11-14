-- CW-20261114-0017: hub mail triage before wake. One row per message to a
-- triaged seat: its class, and what became of it. seat_wake_clock is the
-- per-seat interrupt cap's memory (last action wake).
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
