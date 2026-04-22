-- 0011_proxy_events.sql
--
-- Persistent ring buffer for MCP proxy tool call events. Populated by the
-- mcp --proxy subprocess forwarding tool_call_end events to the daemon.
-- The TUI Activity feed polls GET /proxy/events from the daemon rather than
-- requiring the MCP process and TUI to share in-process memory.
--
-- Ring-buffer semantics are enforced in Go (store.AppendProxyEvent trims the
-- table to proxy_events_max_rows after each insert). SQLite does not natively
-- support ring buffers, so we use a timestamp-ordered DELETE … LIMIT approach.
--
-- All timestamps are RFC3339 nanosecond strings in UTC.

CREATE TABLE IF NOT EXISTS proxy_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT,
    server        TEXT    NOT NULL,
    tool_name     TEXT    NOT NULL,
    args_schema_fp TEXT,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    ok            INTEGER NOT NULL DEFAULT 1,  -- 1 = success, 0 = error
    error         TEXT,
    timestamp     TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_proxy_events_timestamp
    ON proxy_events(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_proxy_events_server
    ON proxy_events(server, timestamp DESC);
