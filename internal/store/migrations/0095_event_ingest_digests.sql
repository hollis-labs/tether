-- Idempotency evidence for the generic event ingest (POST /events, CW-20261020-0010).
-- The events table dedupes on event_id, but it stores the sanitized payload, so
-- it cannot tell a retry (same id, same event) from a different event that
-- reuses the id. The ingest records a digest of the payload exactly as the
-- producer sent it, beside the event id, and compares on a repeat.
-- A row is pruned by the same retention window as the events it describes.
CREATE TABLE IF NOT EXISTS event_ingest_digests (
    event_id TEXT PRIMARY KEY,
    digest   TEXT NOT NULL,
    app      TEXT NOT NULL,
    at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_event_ingest_digests_at ON event_ingest_digests(at);
