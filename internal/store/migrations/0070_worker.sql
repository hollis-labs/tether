ALTER TABLE sessions ADD COLUMN worker_id TEXT NOT NULL DEFAULT '';
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
