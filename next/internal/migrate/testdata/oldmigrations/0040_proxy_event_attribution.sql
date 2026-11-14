-- Secret-free resolved attribution shares proxy_events' existing age retention.
-- Pre-migration rows are explicitly unverified; there is no inferred backfill.
ALTER TABLE proxy_events ADD COLUMN attribution_json TEXT NOT NULL DEFAULT '{"verified":false}';
ALTER TABLE proxy_events ADD COLUMN claimed_session_id TEXT NOT NULL DEFAULT '';
