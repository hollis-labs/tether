-- CW-20261016-0136 (X1a): every events row carries a producer-minted D6 id
-- (evt_ + 32 hex, runtimeevents.NewEventID), and rows written on a caller's
-- transaction are an outbox: pending=1 until the bus has fanned them out after
-- the commit, so a crash between commit and publish still publishes (at least
-- once; the evt_ id is the idempotency key).
--
-- Existing rows keep event_id NULL (no backfill: a mass UPDATE on a ~1M-row
-- table would hold the single database connection) and pending=0, so none of
-- them is ever re-published. The unique index is partial for the same reason.
-- Forward-only: the new columns are additive and an older binary ignores them.
ALTER TABLE events ADD COLUMN event_id TEXT;
ALTER TABLE events ADD COLUMN pending INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_events_event_id ON events(event_id) WHERE event_id IS NOT NULL;
CREATE INDEX idx_events_pending ON events(id) WHERE pending = 1;
