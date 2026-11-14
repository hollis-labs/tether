-- 0006_events_evolution.sql
--
-- Scope: T-v002-s03-05 (events portion). client_attachments already
-- landed in migration 0002 (pulled forward into Sprint v002-02), so
-- this migration only touches events.
--
-- Evolves the v0.0.1 session-scoped events table into a scope-aware
-- stream that the Sprint v002-06 pub/sub bus can publish into. Changes:
--   * session_id becomes nullable (daemon / broker events carry NULL).
--   * scope TEXT NOT NULL DEFAULT 'session' classifies each row.
--   * payload TEXT is renamed/retyped as payload_json TEXT; the
--     migration copies existing payload values across unchanged.
--   * stream_seq is intentionally NOT added — id INTEGER PRIMARY KEY
--     AUTOINCREMENT already provides monotonic stream ordering via
--     sqlite_sequence. Revisit if Sprint v002-06 requires a separate
--     per-scope counter.
--
-- Clean rebuild (no legacy `payload` column survives) per no-compat
-- pre-launch policy. LogEvent has no external consumers — verified by
-- grep before landing.

CREATE TABLE events_new (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    scope        TEXT NOT NULL DEFAULT 'session',
    session_id   TEXT,
    at           TEXT NOT NULL,
    kind         TEXT NOT NULL,
    payload_json TEXT
);

INSERT INTO events_new (id, scope, session_id, at, kind, payload_json)
SELECT id, 'session', session_id, at, kind, payload
FROM events;

DROP TABLE events;
ALTER TABLE events_new RENAME TO events;

CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id, at);
CREATE INDEX IF NOT EXISTS idx_events_scope ON events(scope, at);
