CREATE TABLE IF NOT EXISTS sessions (
    id            TEXT PRIMARY KEY,
    launch_id     TEXT NOT NULL,
    project_id    TEXT NOT NULL,
    agent_id      TEXT NOT NULL,
    provider_id   TEXT NOT NULL,
    workspace     TEXT NOT NULL,
    state         TEXT NOT NULL,
    pid           INTEGER,
    exit_code     INTEGER,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    ended_at      TEXT
);

CREATE TABLE IF NOT EXISTS launch_plans (
    session_id    TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    plan_json     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT NOT NULL,
    at            TEXT NOT NULL,
    kind          TEXT NOT NULL,
    payload       TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_state ON sessions(state);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id, at);
