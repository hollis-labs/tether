-- 0018_ai_events.sql
--
-- Durable, sanitized AI gateway audit records. Privacy posture matches the
-- AI gateway spec: summaries and usage only, no raw prompts or raw responses.
-- Bounded via trim-after-insert in Go code, mirroring proxy_events.

CREATE TABLE IF NOT EXISTS ai_events (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type         TEXT NOT NULL,
    request_id         TEXT,
    session_id         TEXT,
    caller_id          TEXT,
    operation          TEXT NOT NULL,
    provider           TEXT,
    model              TEXT,
    policy_version     TEXT,
    latency_ms         INTEGER NOT NULL DEFAULT 0,
    success            INTEGER NOT NULL DEFAULT 0,
    refusal            TEXT,
    error              TEXT,
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
    estimated_cost_usd REAL NOT NULL DEFAULT 0,
    request_summary    TEXT,
    response_summary   TEXT,
    timestamp          DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ai_events_timestamp
    ON ai_events(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_ai_events_provider
    ON ai_events(provider, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_ai_events_session
    ON ai_events(session_id, timestamp DESC);
